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
