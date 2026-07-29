package devnet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDevnetMagiReporter runs the REAL reporter binary end-to-end against a REAL
// multi-node devnet, with injected Hive post/vote data.
//
// This closes the last untested seam in the system. Everything else was verified
// in halves: contract tests hand-wrote their shares string, and reporter tests
// stopped at producing a page. Here the actual `reporter` binary reads Hive over
// HTTP, computes shares, paginates, builds the call plan, and those exact payloads
// are broadcast to a live chain — then claimed against.
//
//	PHASE 1  deploy token + C2 + C3, init, hand token ownership to C2
//	PHASE 2  serve dummy-but-realistic Hive data from a local JSON-RPC server
//	PHASE 3  run `reporter plan -json` against it + the devnet's GraphQL
//	PHASE 4  broadcast the reporter's own payloads, verbatim
//	PHASE 5  assert the chain agrees with the reporter, and that claims pay out
//
// Run: go test -v -run TestDevnetMagiReporter -timeout 45m ./tests/devnet/
var reporterBin = magiFrameworkDir + "/reporter/bin/reporter"

// hiveFixture is the injected Hive state. Times are derived from the devnet's own
// block clock so the reporter's epoch window lines up with the real chain.
type hiveFixture struct {
	head      uint64
	genesis   uint64
	epochLen  uint64
	blockTime time.Time // timestamp of block `genesis`
	posts     []map[string]any
	votes     map[string][]map[string]any
	hits      map[string]int
}

// buildHiveFixture creates one epoch of tribe activity. Dummy data, realistic
// shape: rshares across six orders of magnitude, a whale, dust, a downvote, a
// self-vote, an author who also curates, and a vote cast after payout closed.
// w1/w2 are REAL devnet witness accounts. The fixture deliberately puts them in
// earning positions (an author and a curator) so the claim path can be exercised —
// purely fictional Hive names have no devnet keys and could never claim.
func buildHiveFixture(genesis, epochLen uint64, blockTime time.Time, w1, w2 string) *hiveFixture {
	f := &hiveFixture{
		head: genesis + 3*epochLen, genesis: genesis, epochLen: epochLen,
		blockTime: blockTime,
		votes:     map[string][]map[string]any{},
		hits:      map[string]int{},
	}
	// epoch 0 spans blocks [genesis, genesis+epochLen-1]; under cashout attribution
	// a post belongs to it when its PAYOUT lands in that window, so created is
	// exactly one payout period earlier.
	const payoutPeriod = 7 * 24 * time.Hour
	hivefmt := func(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") }

	add := func(author, permlink string, offsetBlocks uint64, votes [][3]any) {
		payout := blockTime.Add(time.Duration(offsetBlocks) * 3 * time.Second)
		created := payout.Add(-payoutPeriod)
		f.posts = append(f.posts, map[string]any{
			"author": author, "permlink": permlink, "depth": 0,
			"category":   "magitribe",
			"created":    hivefmt(created),
			"payout_at":  hivefmt(payout),
			"is_paidout": true,
			"stats":      map[string]any{"is_pinned": false},
		})
		vs := make([]map[string]any, 0, len(votes))
		for _, v := range votes {
			voter := v[0].(string)
			rshares := v[1].(string)
			// vote offset in hours after creation; > 168h means after payout
			hrs := v[2].(int)
			vs = append(vs, map[string]any{
				"voter": voter, "rshares": rshares, "percent": 10000,
				"time": hivefmt(created.Add(time.Duration(hrs) * time.Hour)),
			})
		}
		f.votes[author+"/"+permlink] = vs
	}

	// w1 authors a well-curated post; w2 curates early on another. Both therefore
	// hold claimable shares.
	add(w1, "trading-journal-week-12", 1, [][3]any{
		{"whale", "184320000000000", 1},
		{w2, "9420000000000", 3},
		{"curator1", "812000000000", 6},
		{"dust1", "9400000", 20},
		{"latevoter", "56000000000", 160},   // still inside the payout window
		{"toolate", "999000000000000", 200}, // AFTER payout — must be ignored
	})
	add("carol", "market-recap-and-charts", 3, [][3]any{
		{w2, "7300000000000", 1}, // early curator -> large slice under the sqrt curve
		{"curator1", "640000000000", 5},
		{"whale", "92160000000000", 30},
	})
	add("dave", "a-contrarian-take", 5, [][3]any{
		{"curator1", "980000000000", 1},
		{"curator2", "410000000000", 2},
		{"dust2", "8800000", 4},
		{"flagger", "-45000000000", 6}, // downvote: clamps to zero, never subtracts
	})
	return f
}

// serve stands up a Hive JSON-RPC endpoint the reporter can talk to unmodified.
func (f *hiveFixture) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     int             `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.hits[req.Method]++
		var result any
		switch req.Method {
		case "condenser_api.get_dynamic_global_properties":
			result = map[string]any{"head_block_number": f.head}
		case "block_api.get_block_header":
			var p struct {
				BlockNum uint64 `json:"block_num"`
			}
			_ = json.Unmarshal(req.Params, &p)
			ts := f.blockTime.Add(time.Duration(int64(p.BlockNum)-int64(f.genesis)) * 3 * time.Second)
			result = map[string]any{"header": map[string]any{
				"timestamp": ts.UTC().Format("2006-01-02T15:04:05")}}
		case "bridge.get_ranked_posts":
			var p map[string]any
			_ = json.Unmarshal(req.Params, &p)
			// page 1 has everything; any cursored request returns empty so the
			// reporter's walk terminates
			if _, cursored := p["start_author"]; cursored {
				result = []any{}
			} else {
				result = f.posts
			}
		case "condenser_api.get_active_votes":
			var p []string
			_ = json.Unmarshal(req.Params, &p)
			result = f.votes[p[0]+"/"+p[1]]
		default:
			http.Error(w, "unexpected method "+req.Method, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": result,
		})
	}))
}

// stateBigHex reads a contract state key as a big-endian integer.
//
// The token contract stores balances as PACKED BYTES, not decimal text: a balance
// of 3286 comes back as "\x0c\xd6". Reading it as a UTF-8 string yields mojibake
// (and JSON transport can mangle invalid UTF-8 outright), so ask the node for hex
// with getStateByKeys' `encoding` argument and decode that.
func stateBigHex(t *testing.T, d *Devnet, endpoint, contractID, key string) *big.Int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"query":     `query($c:String!,$k:[String!]!){getStateByKeys(contractId:$c,keys:$k,encoding:"hex")}`,
		"variables": map[string]any{"c": contractID, "k": []string{key}},
	})
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("hex state read %s: %v", key, err)
	}
	defer resp.Body.Close()
	var out struct {
		Data struct {
			GetStateByKeys map[string]any `json:"getStateByKeys"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("hex state read %s: %v", key, err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("hex state read %s: %s", key, out.Errors[0].Message)
	}
	h, _ := out.Data.GetStateByKeys[key].(string)
	if h == "" {
		return new(big.Int)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("state %s is not hex: %q", key, h)
	}
	return new(big.Int).SetBytes(raw)
}

// reporterCompute runs `compute -epoch N -json` and decodes it.
func reporterCompute(t *testing.T, run func(...string) []byte, epoch string) struct {
	TotalShares string `json:"total_shares"`
	Accounts    int    `json:"accounts"`
	Canonical   string `json:"canonical"`
} {
	t.Helper()
	var out struct {
		TotalShares string `json:"total_shares"`
		Accounts    int    `json:"accounts"`
		Canonical   string `json:"canonical"`
	}
	raw := run("compute", "-epoch", epoch, "-json")
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("reporter compute is not valid json: %v\n%s", err, raw)
	}
	return out
}

type reporterPlan struct {
	Epoch string `json:"epoch"`
	Calls []struct {
		ContractID string `json:"contract_id"`
		Action     string `json:"action"`
		Payload    string `json:"payload"`
		Note       string `json:"note"`
	} `json:"calls"`
}

func TestDevnetMagiReporter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	if _, err := os.Stat(reporterBin); err != nil {
		t.Fatalf("reporter binary missing at %s — build it: "+
			"cd %s && GOTOOLCHAIN=go1.25.3 go build -o reporter/bin/reporter ./reporter/cmd/reporter",
			reporterBin, magiFrameworkDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	cfg := DefaultConfig()
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		t.Fatalf("starting devnet: %v", err)
	}

	// ---------------- PHASE 1: deploy + init ----------------
	deploy := func(name, wasm string) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name})
			if err == nil {
				t.Logf("deployed %s = %s (attempt %d)", name, id, attempt)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = err
			t.Logf("deploy %s attempt %d failed: %v — settling then retrying", name, attempt, err)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	tokenID := deploy("magi-token", magiTokenWasm)
	c2ID := deploy("magi-c2-emission", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c3ID := deploy("magi-c3-distributor", magiWasm(t, "c3-distributor/artifacts/main.wasm"))

	owner := d.cfg.WitnessPrefix + "1"
	t.Logf("owner=%s token=%s c2=%s c3=%s", owner, tokenID, c2ID, c3ID)

	// RC = VSC-ledger HBD + 10_000 free. Deposit AFTER deploying (each deploy costs
	// 10 HBD of L1 balance), and deposit generously so init+plan never starve.
	for _, amt := range []string{"50.000", "30.000", "20.000", "10.000"} {
		if _, ferr := d.Deposit(ctx, 1, amt, "hbd"); ferr == nil {
			t.Logf("deposited %s HBD for RC", amt)
			break
		}
	}
	time.Sleep(20 * time.Second)

	mustCall := func(id, action, payload, what string) {
		if _, err := d.CallContract(ctx, 1, id, action, payload); err != nil {
			t.Fatalf("%s: broadcast failed: %v", what, err)
		}
		t.Logf("sent: %s", what)
	}
	waitKey := func(id, key, what string) string {
		if !waitStateKeyPresent(t, d, ctx, 1, id, key, 4*time.Minute) {
			t.Fatalf("timed out waiting for %s (%s[%s])", what, id, key)
		}
		st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		return fmt.Sprintf("%v", st[key])
	}

	const epochLen = 10
	mustCall(tokenID, "init",
		`{"name":"MagiTribe","symbol":"MTRIBE","decimals":0,"maxSupply":"1000000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")
	// C2 DRAWS each epoch from an approved pool — it does not mint. The pool must
	// therefore exist and be approved BEFORE ownership moves, because `mint` is
	// owner-only. Without this the run does not fail here: distributeEpoch quietly
	// funds nothing and the failure surfaces minutes later as "epoch 0 was never
	// finalized on chain", pointing nowhere near the cause.
	//
	// 1000000 is 100 epochs at this config (baseAnnual*epochLen/blocksPerYear =
	// 1000000*10/1000 = 10000/epoch), so the pool cannot starve however long the run
	// takes to reach the poke.
	mustCall(tokenID, "mint", `{"amount":"1000000"}`, "mint the emission pool")
	mustCall(tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2 to draw the pool")
	waitKey(tokenID, fmt.Sprintf("alw|hive:%s|contract:%s", owner, c2ID), "C2 allowance")
	mustCall(tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token->C2 ownership")

	// genesis omitted => C2 adopts the CURRENT block height, which is how a real
	// deployment starts. Read it back to drive both C3 and the reporter config.
	mustCall(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"author",`+
			`"timelock":"5","maxCatch":"50","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:10000"}`, tokenID, epochLen, c3ID), "C2 init")
	genesisStr := waitKey(c2ID, "cfg_genesis", "C2 genesis")
	genesis, err := strconv.ParseUint(genesisStr, 10, 64)
	if err != nil {
		t.Fatalf("C2 cfg_genesis %q is not a height: %v", genesisStr, err)
	}
	t.Logf("C2 adopted genesis=%d epochLen=%d", genesis, epochLen)

	mustCall(c3ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","genesis":"%d","epochLen":"%d","window":"1",`+
			`"reporterMode":"0","reporterAuth":"hive:%s","reporterThreshold":"1","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		tokenID, c2ID, genesis, epochLen, owner), "C3 init")
	waitKey(c3ID, "cfg_funder", "C3 funder")

	// ---------------- PHASE 2: inject Hive data ----------------
	w2 := d.cfg.WitnessPrefix + "2"
	fixture := buildHiveFixture(genesis, epochLen, time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), owner, w2)
	hive := fixture.serve(t)
	defer hive.Close()
	t.Logf("fake Hive serving %d posts at %s (head=%d)", len(fixture.posts), hive.URL, fixture.head)

	// ---------------- PHASE 3: run the REAL reporter ----------------
	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "reporter.json")
	reporterCfg := map[string]any{
		"hive": map[string]any{"api": []string{hive.URL}},
		"vsc": map[string]any{
			"api":    d.GQLEndpoint(1),
			"net_id": "vsc-devnet",
		},
		"contracts": map[string]any{"distributor": c3ID, "funder": c2ID, "stake": ""},
		"epoch":     map[string]any{"genesis": genesis, "len": epochLen},
		"source": map[string]any{
			"tag": "magitribe", "limit": 100,
			"attribution": "cashout", "weight": "hive_rshares", "exclude": []string{},
		},
		"shares": map[string]any{
			"author_reward_bps": 5000, "author_curve": "1/1",
			"curation_curve": "1/2", "muted": []string{},
		},
		// tiny pages so the epoch MUST span several submitShares calls
		"page": map[string]any{"max_entries": 3, "max_bytes": 3800},
		"submit": map[string]any{
			"account": owner, "rc_limit": 200000,
			"progress_file": filepath.Join(workDir, "progress.json"),
			"keeper":        true, "pull_funding": true, "finalize": true,
		},
	}
	blob, _ := json.MarshalIndent(reporterCfg, "", "  ")
	if err := os.WriteFile(cfgPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	runReporter := func(args ...string) []byte {
		cmd := exec.CommandContext(ctx, reporterBin, append(args, "-config", cfgPath)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("reporter %v failed: %v\n%s", args, err, out)
		}
		return out
	}

	// `epoch` also cross-checks the local config against on-chain cfg_genesis /
	// cfg_epochLen / cfg_funder — a real integration assertion, not just a print.
	t.Logf("reporter epoch:\n%s", runReporter("epoch"))

	planJSON := runReporter("plan", "-json")
	var plan reporterPlan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatalf("reporter plan is not valid json: %v\n%s", err, planJSON)
	}
	if len(plan.Calls) < 4 {
		t.Fatalf("expected keeper+pull+pages+finalize, got %d calls: %s", len(plan.Calls), planJSON)
	}
	pages := 0
	for _, c := range plan.Calls {
		if c.Action == "submitShares" {
			pages++
		}
	}
	if pages < 2 {
		t.Fatalf("wanted a multi-page report to exercise paging, got %d pages", pages)
	}
	t.Logf("reporter planned epoch %s: %d calls (%d share pages)", plan.Epoch, len(plan.Calls), pages)

	// Snapshot the report BEFORE broadcasting. Re-running it afterwards must give a
	// byte-identical answer — that is the property the challenge window (a verifier
	// must be able to reproduce the report) and Attest mode (N machines must agree
	// on the same bytes) both depend on.
	computeBefore := reporterCompute(t, runReporter, plan.Epoch)
	t.Logf("reporter computed epoch %s: totalShares=%s across %d accounts",
		plan.Epoch, computeBefore.TotalShares, computeBefore.Accounts)

	// let the real chain pass the epoch boundary before the keeper poke
	t.Logf("waiting for the chain to pass block %d (end of epoch 0)...", genesis+epochLen)
	time.Sleep(time.Duration(epochLen+6) * 3 * time.Second)

	// ---------------- PHASE 4: broadcast the reporter's payloads ----------------
	for i, c := range plan.Calls {
		t.Logf("plan[%d] %-16s %s", i, c.Action, c.Payload)
		if _, err := d.CallContract(ctx, 1, c.ContractID, c.Action, c.Payload); err != nil {
			t.Fatalf("plan[%d] %s broadcast failed: %v", i, c.Action, err)
		}
		time.Sleep(9 * time.Second) // let each land before the next depends on it
	}

	// ---------------- PHASE 5: the chain must agree with the reporter ----------
	//
	// Wait for FINALIZATION before sampling totalShares. totalShares|0 exists from
	// the moment page 0 applies and then GROWS as later pages land, so reading it
	// early yields a partial sum (measured once: pages 0-2 only, short by exactly
	// the last page). status|0 is written by finalizeEpoch, the last call in the
	// plan — and because L1 transaction order is preserved, its presence proves
	// every preceding page has already been processed.
	if !waitStateKeyPresent(t, d, ctx, 1, c3ID, "status|0", 5*time.Minute) {
		t.Fatal("epoch 0 was never finalized on chain")
	}
	if st := waitKey(c3ID, "status|0", "C3 epoch status"); st != "finalized" {
		t.Fatalf("epoch 0 status = %q, want finalized", st)
	}
	total := waitKey(c3ID, "totalShares|0", "C3 totalShares")
	funded := waitKey(c3ID, "funded|0", "C3 funded")
	t.Logf("on-chain (finalized): funded=%s totalShares=%s", funded, total)

	// Recompute with the epoch PINNED. Without -epoch the reporter would correctly
	// advance to the next unfinalized epoch (epoch 0 is finalized now), which has no
	// posts — that is the oldest-unfinalized default working, not a mismatch.
	computeAfter := reporterCompute(t, runReporter, plan.Epoch)

	if computeAfter.Canonical != computeBefore.Canonical {
		t.Fatalf("REPORT NOT REPRODUCIBLE: recomputing epoch %s gave different shares\nbefore: %s\nafter:  %s",
			plan.Epoch, computeBefore.Canonical, computeAfter.Canonical)
	}
	if computeAfter.TotalShares != total {
		t.Fatalf("SEAM BROKEN: reporter computed totalShares=%s but the chain recorded %s",
			computeAfter.TotalShares, total)
	}
	t.Logf("reporter and chain agree: totalShares=%s across %d accounts (reproducible across runs)",
		computeAfter.TotalShares, computeAfter.Accounts)

	// the post-payout vote must never have reached the chain
	if st, _ := d.GetStateByKeys(ctx, 1, c3ID, []string{"share|0|hive:toolate"}); st != nil {
		if v, ok := st["share|0|hive:toolate"]; ok && v != nil && v != "" {
			t.Fatalf("a vote cast AFTER payout earned shares on-chain: %v", v)
		}
	}
	if st, _ := d.GetStateByKeys(ctx, 1, c3ID, []string{"share|0|hive:flagger"}); st != nil {
		if v, ok := st["share|0|hive:flagger"]; ok && v != nil && v != "" {
			t.Fatalf("a downvoter earned shares on-chain: %v", v)
		}
	}

	// ---- claims: both real devnet accounts must be paid exactly -------------
	fundedN, ok1 := new(big.Int).SetString(funded, 10)
	totalN, ok2 := new(big.Int).SetString(total, 10)
	if !ok1 || !ok2 || totalN.Sign() == 0 {
		t.Fatalf("bad funded/total on chain: %q / %q", funded, total)
	}
	// window=1 block; give the chain a moment past it before claiming
	time.Sleep(15 * time.Second)

	for _, cl := range []struct {
		node int
		acct string
	}{{1, owner}, {2, w2}} {
		key := "share|0|hive:" + cl.acct
		st, err := d.GetStateByKeys(ctx, 1, c3ID, []string{key})
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		raw := fmt.Sprintf("%v", st[key])
		share, good := new(big.Int).SetString(raw, 10)
		if !good || share.Sign() <= 0 {
			t.Fatalf("%s earned no on-chain share (%q) — the fixture should have paid them", cl.acct, raw)
		}
		want := new(big.Int).Div(new(big.Int).Mul(fundedN, share), totalN)
		t.Logf("hive:%s share=%s -> expected payout %s", cl.acct, share, want)

		if _, err := d.CallContract(ctx, cl.node, c3ID, "claim", `{"epoch":"0"}`); err != nil {
			t.Fatalf("claim for %s failed to broadcast: %v", cl.acct, err)
		}
		if !waitStateKeyPresent(t, d, ctx, 1, c3ID, "claimed|0|hive:"+cl.acct, 3*time.Minute) {
			t.Fatalf("claim by %s never landed on chain", cl.acct)
		}
		// the token contract must show exactly that balance
		got := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+cl.acct)
		if got.Cmp(want) != 0 {
			t.Fatalf("%s token balance = %s, want %s (funded*share/totalShares)",
				cl.acct, got, want)
		}
		t.Logf("hive:%s claimed and holds exactly %s", cl.acct, want)
	}

	t.Logf("REPORTER DEVNET TEST PASSED — the reporter's own payloads drove a live chain")
	t.Logf("fake Hive call counts: %v", fixture.hits)
}
