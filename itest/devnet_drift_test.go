package itest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The devnet suites live under testdata/, which Go ignores by construction: they are
// `package devnet` and need go-vsc-node internals, so they are copied into a
// go-vsc-node checkout to run. The consequence is that `go build ./...`,
// `go vet ./...` and `go test ./...` NEVER type-check them. Rename an entrypoint or a
// state key and 3,000+ lines of suite rot silently until someone spends 20-60 minutes
// discovering it on a devnet run — which happened more than once while writing them.
//
// This is the cheap half of a drift detector: it cannot type-check, but it can check
// that every contract action and state-key prefix the suites reference still exists in
// the contracts. That catches renames, which is the failure that actually occurs.

var (
	devnetDir   = "../testdata/devnet"
	contractDir = ".."

	// d.CallContract(ctx, node, id, "action", payload) and the local call helpers,
	// which all pass the action as a literal in the same position.
	reAction = regexp.MustCompile(`(?:CallContract\([^,]+,[^,]+,[^,]+|call[A-Za-z]*\((?:t, )?(?:ct|d)?[^"]*)"([a-z][A-Za-z0-9]*)"`)
	// state keys are referenced as "prefix|..." literals
	reStateKey = regexp.MustCompile(`"([a-z][A-Za-z0-9_]*)\|`)
	// A keyed READ: a contract id variable, then within the same call a "prefix|"
	// literal. Attributing the key to its CONTRACT is the point — checking that a key
	// exists in *some* contract let `claimed|` (written by the distributor) satisfy a
	// read against C1, which writes `y_claimed|`. That cost a 20-minute devnet run.
	reKeyedRead = regexp.MustCompile(`\b(c[1-7]ID|distID)\b[^\n]{0,120}?"([a-z][A-Za-z0-9_]*)\|`)
)

func devnetSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(devnetDir)
	if err != nil {
		t.Skipf("devnet suites not present: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(devnetDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		out[e.Name()] = string(b)
	}
	if len(out) == 0 {
		t.Skip("no devnet suites found")
	}
	return out
}

func contractSources(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range []string{"c1-staking", "c2-emission", "c3-distributor"} {
		b, err := os.ReadFile(filepath.Join(contractDir, c, "contract", "main.go"))
		if err != nil {
			t.Fatalf("read %s: %v", c, err)
		}
		sb.Write(b)
	}
	// the external token contract is referenced too
	for _, f := range []string{"token.go", "internal.go"} {
		if b, err := os.ReadFile(filepath.Join(
			"/mnt/HC_Volume_105012347/magi/testnet/magi_token-contract/contract", f)); err == nil {
			sb.Write(b)
		}
	}
	return sb.String()
}

// Every action the devnet suites invoke must still be an exported entrypoint.
func TestDevnetDrift_EveryReferencedActionExists(t *testing.T) {
	contracts := contractSources(t)
	exported := map[string]bool{}
	for _, m := range regexp.MustCompile(`//go:wasmexport (\w+)`).FindAllStringSubmatch(contracts, -1) {
		exported[m[1]] = true
	}
	assert.NotEmpty(t, exported, "found no //go:wasmexport at all — the scan is broken, not the suites")

	// actions that are deliberately not framework entrypoints
	ignore := map[string]bool{
		"init": true, "transfer": true, "approve": true, "mint": true, "burn": true,
		"changeOwner": true, "increaseAllowance": true, "decreaseAllowance": true,
		"balanceOf": true, "getInfo": true, "allowance": true, "pause": true, "unpause": true,
		"transferFrom": true, "totalSupply": true,
	}

	missing := map[string][]string{}
	for name, src := range devnetSources(t) {
		for _, m := range reAction.FindAllStringSubmatch(src, -1) {
			act := m[1]
			if ignore[act] || exported[act] {
				continue
			}
			// only flag things that look like contract actions: they appear next to a
			// contract id argument in these suites, so require camelCase or a known verb
			if len(act) < 4 || strings.ToLower(act) == act && !strings.Contains(act, "_") && len(act) < 6 {
				continue
			}
			missing[name] = append(missing[name], act)
		}
	}
	for f, acts := range missing {
		sort.Strings(acts)
		t.Errorf("%s references actions that are not //go:wasmexport in any contract: %v\n"+
			"  either they were renamed and the suite has rotted, or this scan needs a new exception",
			f, uniqStrings(acts))
	}
}

// Every state-key prefix the suites read must still be written somewhere.
func TestDevnetDrift_EveryReferencedStateKeyExists(t *testing.T) {
	perContract := contractSourcesByName(t)
	missing := map[string][]string{}
	// prefixes the test constructs itself rather than reading from a contract
	ignore := map[string]bool{"epoch": true, "block": true}

	for name, src := range devnetSources(t) {
		for _, m := range reKeyedRead.FindAllStringSubmatch(src, -1) {
			idVar, k := m[1], m[2]
			if ignore[k] {
				continue
			}
			src, ok := perContract[contractOfVar(idVar)]
			if !ok {
				continue // an id this guard cannot attribute; not worth a false alarm
			}
			if strings.Contains(src, `"`+k+`|`) {
				continue
			}
			missing[name] = append(missing[name], idVar+" -> "+k)
		}
	}
	for f, keys := range missing {
		sort.Strings(keys)
		t.Errorf("%s reads state keys THAT CONTRACT does not write: %v\n"+
			"  a renamed key reads as empty on devnet, so assertions pass vacuously or time out",
			f, uniqStrings(keys))
	}
}

// contractOfVar maps a suite's contract id variable to the contract behind it.
//
// The suites kept their historical variable names through the merge — c5ID is the LP
// CHANNEL on the distributor, c6ID/c7ID are roles inside C1 — so the mapping is by
// convention rather than by name.
func contractOfVar(v string) string {
	switch v {
	case "c1ID", "c6ID", "c7ID":
		return "c1-staking"
	case "c2ID":
		return "c2-emission"
	case "c3ID", "c5ID", "distID":
		return "c3-distributor"
	}
	return ""
}

func contractSourcesByName(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range []string{"c1-staking", "c2-emission", "c3-distributor"} {
		b, err := os.ReadFile(filepath.Join(contractDir, c, "contract", "main.go"))
		if err != nil {
			t.Fatalf("read %s: %v", c, err)
		}
		out[c] = string(b)
	}
	return out
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// A wasm artifact older than its source means every test — unit AND devnet — is
// exercising code that no longer exists. Nothing else catches it: the suites load the
// .wasm file happily, assertions pass or fail against the old behaviour, and a devnet
// run costs 20-60 minutes before the confusion surfaces.
//
// The shared packages count too: adapter/ and auth/ are compiled INTO every contract,
// so editing one of them staleness-invalidates all six artifacts even though no
// contract/main.go changed.
func TestDevnetDrift_ArtifactsAreNewerThanSources(t *testing.T) {
	shared := []string{"../adapter/adapter.go", "../auth/auth.go"}
	newestShared := int64(0)
	for _, f := range shared {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if m := st.ModTime().Unix(); m > newestShared {
			newestShared = m
		}
	}

	for _, c := range []string{
		"c1-staking", "c2-emission", "c3-distributor",
	} {
		srcPath := filepath.Join("..", c, "contract", "main.go")
		artPath := filepath.Join("..", c, "artifacts", "main.wasm")
		src, err := os.Stat(srcPath)
		if err != nil {
			t.Fatalf("stat %s: %v", srcPath, err)
		}
		art, err := os.Stat(artPath)
		if err != nil {
			t.Fatalf("stat %s: %v — build it before testing", artPath, err)
		}
		newest := src.ModTime().Unix()
		if newestShared > newest {
			newest = newestShared
		}
		if art.ModTime().Unix() < newest {
			t.Errorf("%s: artifacts/main.wasm is OLDER than its sources — every test using it is "+
				"exercising code that no longer exists. Rebuild with:\n"+
				"  GOTOOLCHAIN=go1.25.3 tinygo build -gc=custom -scheduler=none -panic=trap "+
				"-no-debug -target=wasm-unknown -o %s/artifacts/main.wasm ./%s/contract", c, c, c)
		}
	}
}

// C3 and C5 used to be the same contract deployed twice, and this guard existed to
// catch them diverging — a change applied to one twin and not the other left both
// compiling, both deploying, and only the untouched instance misbehaving.
//
// The merge removed the twin: one distributor serves every reward channel, so there
// is nothing left to keep in sync and the whole class of bug is gone. Deleting the
// guard rather than leaving it passing vacuously, which would read as coverage.

// A devnet suite reads state in two steps: fetch a set of keys, then look them up in
// the returned map. If a key is renamed in the fetch but not the lookup, the lookup
// returns nil and every assertion built on it passes VACUOUSLY — a SECURITY FAILURE
// check that compares nil to nil and reports success.
//
// That is not hypothetical. Channel-scoping renamed `funded|0` to `funded|<ch>|0`;
// two suites had the fetch updated and the lookup left behind, and both would have
// reported "nothing was stolen" without comparing anything. One of them burned a
// 16-minute devnet run before the mismatch surfaced as a confusing nil.
//
// TestDevnetDrift_EveryReferencedStateKeyExists cannot catch this: it checks key
// PREFIXES against a contract, and `funded` is a prefix the distributor really does
// write. Only the fetch/lookup correspondence exposes it.
func TestDevnetDrift_EveryLookupMatchesAKeyThatWasRead(t *testing.T) {
	reKeyLiteral := regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*\|[^"]*)"`)
	reLookup := regexp.MustCompile(`\w+\["([^"]*\|[^"]*)"\]`)

	for name, src := range devnetSources(t) {
		// Strip the lookup expressions BEFORE collecting fetched keys. A lookup
		// contains the key literal itself, so counting it as a fetch makes this guard
		// vacuous in exactly the way it exists to prevent — which is what the first
		// version of it did, and it passed happily against a known-broken file.
		fetched := reLookup.ReplaceAllString(src, "")
		read := map[string]bool{}
		for _, m := range reKeyLiteral.FindAllStringSubmatch(fetched, -1) {
			read[m[1]] = true
		}
		var bad []string
		for _, m := range reLookup.FindAllStringSubmatch(src, -1) {
			if !read[m[1]] {
				bad = append(bad, m[1])
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			t.Errorf("%s looks up state keys it never fetched: %v\n"+
				"  the lookup returns nil, so any assertion on it passes without comparing anything",
				name, uniqStrings(bad))
		}
	}
}

// ---------------------------------------------------------------------------
// An owner-only action must be sent FROM the node that deployed the contract.
//
// magi_full spreads the four deploys across nodes so no single account carries every
// 10 HBD fee, which means the contract owner differs per contract. Its generic `call`
// helper hardcodes node 1, so using it for an owner-only action aborts with "only
// contract owner can ..." — and because the suites confirm progress by waiting for a
// state key, the symptom is a silent four-minute timeout, not an error. That cost a
// full 17-minute run on `adoptSchedule`.
//
// The check resolves the owner node from the `dep(...)` call that created each contract
// id and compares it against the node each call site sends from. Suites that deploy
// everything from one node resolve to no mismatch and cost nothing.

// ownerOnlyActions maps contract dir -> action names whose body compares
// contract.owner against msg.caller.
func ownerOnlyActions(t *testing.T) map[string]map[string]bool {
	t.Helper()
	reExport := regexp.MustCompile(`//go:wasmexport (\w+)\s*\nfunc \w+\([^)]*\)[^{]*\{`)
	out := map[string]map[string]bool{}
	for name, src := range contractSourcesByName(t) {
		acts := map[string]bool{}
		for _, loc := range reExport.FindAllStringSubmatchIndex(src, -1) {
			act := src[loc[2]:loc[3]]
			// walk the function body by brace depth from the opening brace
			depth, end := 0, loc[1]-1
			for i := loc[1] - 1; i < len(src); i++ {
				switch src[i] {
				case '{':
					depth++
				case '}':
					depth--
				}
				if depth == 0 && i > loc[1]-1 {
					end = i
					break
				}
			}
			if strings.Contains(src[loc[1]-1:end], "contract.owner") {
				acts[act] = true
			}
		}
		out[name] = acts
	}
	return out
}

func TestDevnetDrift_OwnerOnlyActionsAreCalledFromTheOwnerNode(t *testing.T) {
	ownerOnly := ownerOnlyActions(t)
	total := 0
	for _, acts := range ownerOnly {
		total += len(acts)
	}
	assert.NotZero(t, total, "found no owner-gated actions in any contract — the scan is broken, not the suites")

	// c1ID := dep("magi-c1-staking", magiWasm(t, "..."), 2)
	reDep := regexp.MustCompile(`(?m)^\s*(\w+) := dep\(.*,\s*(\d+)\)\s*$`)
	// call := func(id, action, payload, what string) { callN(1, id, ...) }
	reCallBinding := regexp.MustCompile(`call := func\([^)]*\)[^{]*\{\s*callN\((\d+),`)
	reCall := regexp.MustCompile(`\bcall\((\w+),\s*"([a-zA-Z][A-Za-z0-9]*)"`)
	reCallN := regexp.MustCompile(`\bcallN\((\d+),\s*(\w+),\s*"([a-zA-Z][A-Za-z0-9]*)"`)

	checked := 0
	for name, src := range devnetSources(t) {
		owner := map[string]int{}
		for _, m := range reDep.FindAllStringSubmatch(src, -1) {
			owner[m[1]] = atoiOr(m[2], 0)
		}
		if len(owner) == 0 {
			continue // single-node suite: the generic caller is always the owner
		}
		callNode := 1
		if m := reCallBinding.FindStringSubmatch(src); m != nil {
			callNode = atoiOr(m[1], 1)
		}

		type site struct {
			v, act string
			node   int
		}
		sites := []site{}
		for _, m := range reCall.FindAllStringSubmatch(src, -1) {
			sites = append(sites, site{m[1], m[2], callNode})
		}
		for _, m := range reCallN.FindAllStringSubmatch(src, -1) {
			sites = append(sites, site{m[2], m[3], atoiOr(m[1], 0)})
		}

		bad := []string{}
		for _, s := range sites {
			c := contractOfVar(s.v)
			if c == "" || !ownerOnly[c][s.act] {
				continue
			}
			want, ok := owner[s.v]
			if !ok {
				continue
			}
			checked++
			if s.node != want {
				bad = append(bad, s.act+" on "+s.v+" sent from node "+
					itoa(s.node)+", but "+s.v+" was deployed by node "+itoa(want))
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			t.Errorf("%s calls owner-only actions from a non-owner node: %v\n"+
				"  these abort with \"only contract owner can ...\"; use callOwner(...) instead.\n"+
				"  the suite will not report an error — it will hang on the confirming waitKey",
				name, uniqStrings(bad))
		}
	}
	assert.NotZero(t, checked,
		"resolved no owner-only call sites at all — the regexes stopped matching, so this guard is vacuous")
}

func atoiOr(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
