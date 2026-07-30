package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDevnetMagiLPMultiEpoch runs THREE epochs of LP rewards on a live chain, driven
// by the real reporter binary in `source.kind: "lp"` mode.
//
// C5 had been a payout mechanism with nothing behind it: every other suite feeds it
// hand-written share literals. This drives the actual producer — the indexer replay,
// the min(start,end) rule, canonicalisation, pagination, submission and claims.
//
// The indexer is faked with an httptest GraphQL server, the same way the content
// reporter suite fakes Hive. What that CANNOT catch is a mismatch between my queries
// and the real Hasura schema — a fake server I wrote will always agree with me. That
// needs one read-only `reporter compute` against a live indexer.
//
// The fixture is built so each epoch has a DIFFERENT correct answer, which is what
// makes multi-epoch meaningful rather than the same assertion three times:
//
//	provider  epoch 0   epoch 1   epoch 2   why
//	steady      1000      1000      1000    in before the run, never moves
//	grower      1000      1000      4000    tops up mid-epoch-1: counts from epoch 2 ONLY
//	exiter      1000         0         0    exits mid-epoch-1: forfeits that epoch
//	flash          0         0         0    in and out inside epoch 1: earns nothing
//	total       3000      2000      5000
//
// grower is the anti-flash rule doing its job: a mid-epoch top-up must NOT raise the
// epoch it lands in, because min(LP(start), LP(end)) takes the smaller boundary.
//
// Run: go test -v -run TestDevnetMagiLPMultiEpoch -timeout 60m ./tests/devnet/
type lpEvent struct {
	Provider string
	Amount   string
	Height   uint64
	Add      bool
}

// lpFixture is a stand-in Hasura serving the two liquidity event tables.
type lpFixture struct {
	mu     sync.Mutex
	pool   string
	events []lpEvent
	hits   int
	health uint64 // what indexer_health reports; the reporter's freshness gate reads it
}

func (f *lpFixture) set(events []lpEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = events
}

func (f *lpFixture) setHealth(h uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = h
}

// serve implements enough of Hasura's contract for the reporter's queries: filter by
// pool and height, order totally, and page. The paging is real so the reporter's
// offset walk is exercised rather than assumed.
func (f *lpFixture) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.hits++
		events := append([]lpEvent(nil), f.events...)
		pool := f.pool
		health := f.health
		f.mu.Unlock()

		// The reporter refuses to score an epoch the indexer has not provably reached,
		// so it asks for indexer_health first. Answering it is part of being a
		// believable stand-in.
		if strings.Contains(req.Query, "indexer_health") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":{"indexer_health":[{"latest_block_height":%d}]}}`, health)
			return
		}

		wantAdd := strings.Contains(req.Query, "add_liq_events")
		num := func(k string) uint64 {
			switch v := req.Variables[k].(type) {
			case float64:
				return uint64(v)
			case string:
				n, _ := strconv.ParseUint(v, 10, 64)
				return n
			}
			return 0
		}
		if got, _ := req.Variables["pool"].(string); got != pool {
			// A pool mismatch must not look like "no liquidity" — that would let a
			// misconfigured reporter quietly report an empty epoch.
			http.Error(w, fmt.Sprintf("unexpected pool %q, fixture serves %q", got, pool), 400)
			return
		}
		h, limit, offset := num("h"), int(num("limit")), int(num("offset"))

		kept := []lpEvent{}
		for _, e := range events {
			if e.Add == wantAdd && e.Height <= h {
				kept = append(kept, e)
			}
		}
		end := offset + limit
		if end > len(kept) {
			end = len(kept)
		}
		page := []lpEvent{}
		if offset < len(kept) {
			page = kept[offset:end]
		}
		rows := make([]string, 0, len(page))
		for _, e := range page {
			rows = append(rows, fmt.Sprintf(
				`{"provider":%q,"amount":%s,"indexer_block_height":%d}`, e.Provider, e.Amount, e.Height))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"rows":[%s]}}`, strings.Join(rows, ","))
	}))
}

// hiveHead is the minimum Hive endpoint the reporter needs in LP mode: epoch
// selection refuses an epoch whose end block is past the head, so the head has to
// advance as the run progresses.
type hiveHead struct {
	mu   sync.Mutex
	head uint64
}

func (h *hiveHead) set(v uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.head = v
}

func (h *hiveHead) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     int    `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		head := h.head
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"head_block_number":%d}}`, req.ID, head)
	}))
}

func TestDevnetMagiLPMultiEpoch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	if _, err := os.Stat(reporterBin); err != nil {
		t.Fatalf("reporter binary missing at %s — build it first (see %s/README.md)",
			reporterBin, magiFrameworkDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
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

	w := func(n int) string { return d.cfg.WitnessPrefix + strconv.Itoa(n) }
	owner := w(1) // deployer, pool holder, reporter authority
	treasury := w(4)
	guardian := w(5)

	deploy := func(name, wasm string) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, e := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name, DeployerNode: 1})
			if e == nil {
				t.Logf("deployed %-10s = %s", name, id)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = e
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	tokenID := deploy("lp-token", magiTokenWasm)
	c2ID := deploy("lp-c2", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c5ID := deploy("lp-c5", magiWasm(t, "c5-lp/artifacts/main.wasm"))

	// Node 2 claims three times of its own, so it needs RC too — not just node 1.
	for _, n := range []int{1, 2} {
		for round := 0; round < 4; round++ {
			ok := false
			for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
				if _, e := d.Deposit(ctx, n, amt, "hbd"); e == nil {
					t.Logf("node %d deposited %s HBD", n, amt)
					ok = true
					break
				}
			}
			if !ok {
				break
			}
		}
	}
	dl := time.Now().Add(4 * time.Minute)
	for {
		b1, e1 := d.GetAccountBalance(ctx, 1, "hive:"+owner)
		b2, e2 := d.GetAccountBalance(ctx, 1, "hive:"+w(2))
		if e1 == nil && e2 == nil && b1 != nil && b2 != nil && b1.Hbd > 0 && b2.Hbd > 0 {
			t.Logf("RC ready: %s hbd=%d, %s hbd=%d", owner, b1.Hbd, w(2), b2.Hbd)
			break
		}
		if time.Now().After(dl) {
			t.Fatal("deposits never credited — a three-epoch run cannot fit the free tier")
		}
		time.Sleep(6 * time.Second)
	}

	call := func(id, action, payload, what string) {
		if _, e := d.CallContract(ctx, 1, id, action, payload); e != nil {
			t.Fatalf("%s: broadcast failed: %v", what, e)
		}
		t.Logf("sent: %s", what)
		time.Sleep(9 * time.Second)
	}
	stateOf := func(id, key string) string {
		st, e := d.GetStateByKeys(ctx, 1, id, []string{key})
		if e != nil {
			return ""
		}
		if v, ok := st[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	waitKey := func(id, key, what string) string {
		if !waitStateKeyPresent(t, d, ctx, 1, id, key, 4*time.Minute) {
			t.Fatalf("timed out waiting for %s (%s[%s])", what, id, key)
		}
		return stateOf(id, key)
	}
	waitValue := func(id, key, want, what string) {
		deadline := time.Now().Add(4 * time.Minute)
		last := ""
		for time.Now().Before(deadline) {
			if last = stateOf(id, key); last == want {
				t.Logf("%s = %s", what, want)
				return
			}
			time.Sleep(6 * time.Second)
		}
		t.Fatalf("%s never reached %s (last %q) — %s[%s]", what, want, last, id, key)
	}

	const epochLen = 30
	const emission = 30000 // baseAnnual*epochLen/blocksPerYear = 1000000*30/1000

	// ---------------- init ----------------
	call(tokenID, "init", `{"name":"LPRun","symbol":"LPR","decimals":0,"maxSupply":"100000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")
	// Pool for four epochs' worth; ownership is NOT handed to C2 (it needs none).
	call(tokenID, "mint", `{"amount":"120000"}`, "mint the emission pool")
	call(tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"120000"}`, c2ID), "approve C2 to draw the pool")

	call(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"lp","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"lp:contract:%s:10000"}`,
		tokenID, epochLen, guardian, treasury, c5ID), "C2 init")
	genesis, _ := strconv.ParseUint(waitKey(c2ID, "cfg_genesis", "genesis"), 10, 64)
	t.Logf("genesis=%d epochLen=%d emission=%d/epoch -> all of it to C5", genesis, epochLen, emission)

	call(c5ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, owner, treasury, guardian), "C5 init")
	waitKey(c5ID, "cfg_funder", "C5 ready")

	// ---------------- fixture: heights are relative to the real genesis ----------
	// Epoch e spans [genesis+e*epochLen, genesis+(e+1)*epochLen-1], so events can be
	// placed inside a chosen epoch exactly. Providers are hive: prefixed because the
	// distributor credits only ledger addresses; lpsrc refuses anything else rather
	// than submit a report the contract would silently discard.
	steady := "hive:" + owner
	grower := "hive:" + w(2)
	const exiter, flash = "hive:lpexiter", "hive:lpflash"
	mid1 := genesis + epochLen + 5 // inside epoch 1

	fx := &lpFixture{pool: "vsc1lppool"}
	fx.set([]lpEvent{
		{steady, "1000", genesis, true},
		{grower, "1000", genesis, true},
		{grower, "3000", mid1, true}, // top-up: must lift epoch 2, NOT epoch 1
		{exiter, "1000", genesis, true},
		{exiter, "1000", mid1, false}, // exit: forfeits epoch 1 onward
		{flash, "5000", genesis + epochLen + 10, true},
		{flash, "5000", genesis + epochLen + 15, false}, // in and out inside epoch 1
	})
	idx := fx.serve(t)
	defer idx.Close()

	head := &hiveHead{}
	hv := head.serve(t)
	defer hv.Close()
	t.Logf("fake indexer at %s, fake hive head at %s", idx.URL, hv.URL)

	cfgBody, err := json.Marshal(map[string]any{
		"hive": map[string]any{"api": []string{hv.URL}},
		"vsc":  map[string]any{"api": d.GQLEndpoint(1), "net_id": "vsc-devnet"},
		"indexer": map[string]any{
			"api": idx.URL, "secret": "", "pool": fx.pool, "page_size": 2, // tiny: forces paging
		},
		"contracts": map[string]any{"distributor": c5ID, "funder": c2ID, "stake": ""},
		"epoch":     map[string]any{"genesis": genesis, "len": epochLen},
		"source":    map[string]any{"kind": "lp"},
		"page":      map[string]any{"max_entries": 12, "max_bytes": 3500},
		"submit": map[string]any{
			"rc_limit": 10000, "keeper": true, "pull_funding": true, "finalize": true,
			"progress_file": t.TempDir() + "/progress.json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := t.TempDir() + "/lp.json"
	if err := os.WriteFile(cfgPath, cfgBody, 0o600); err != nil {
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

	// ---------------- three epochs, each with a different correct answer ----------
	type expect struct {
		total  string
		shares map[string]string
	}
	want := []expect{
		{"3000", map[string]string{steady: "1000", grower: "1000", exiter: "1000"}},
		{"2000", map[string]string{steady: "1000", grower: "1000"}},
		{"5000", map[string]string{steady: "1000", grower: "4000"}},
	}

	for ep := uint64(0); ep < 3; ep++ {
		epEnd := genesis + (ep+1)*epochLen - 1
		t.Logf("---------- epoch %d (blocks %d..%d) ----------", ep, genesis+ep*epochLen, epEnd)

		// Let the real chain pass the epoch end before reporting it. The reporter
		// additionally refuses an epoch whose end is past the head it sees, so the
		// fake head is advanced to match — cfg_genesis and this head share one height
		// space, as the content reporter suite also assumes.
		if ep == 0 {
			time.Sleep(time.Duration(epochLen+6) * 3 * time.Second)
		} else {
			time.Sleep(time.Duration(epochLen) * 3 * time.Second)
		}
		head.set(epEnd + 2)
		// The indexer must also be provably past the epoch end, or the reporter
		// correctly refuses to score it. Advancing this in step with the chain is what
		// a healthy indexer looks like.
		fx.setHealth(epEnd + 1)

		epFlag := strconv.FormatUint(ep, 10)
		planJSON := runReporter("plan", "-epoch", epFlag, "-json")
		var plan reporterPlan
		if err := json.Unmarshal(planJSON, &plan); err != nil {
			t.Fatalf("epoch %d: plan is not valid json: %v\n%s", ep, err, planJSON)
		}

		got := reporterCompute(t, runReporter, epFlag)
		if got.TotalShares != want[ep].total {
			t.Fatalf("epoch %d totalShares = %s, want %s (LP min(start,end) is wrong)",
				ep, got.TotalShares, want[ep].total)
		}
		t.Logf("epoch %d: reporter computed totalShares=%s across %d providers",
			ep, got.TotalShares, got.Accounts)

		for i, c := range plan.Calls {
			t.Logf("  plan[%d] %-16s %s", i, c.Action, c.Payload)
			if _, err := d.CallContract(ctx, 1, c.ContractID, c.Action, c.Payload); err != nil {
				t.Fatalf("epoch %d plan[%d] %s broadcast failed: %v", ep, i, c.Action, err)
			}
			time.Sleep(9 * time.Second)
		}

		waitValue(c5ID, fmt.Sprintf("funded|%d", ep), strconv.Itoa(emission),
			fmt.Sprintf("C5 funded epoch %d", ep))
		waitValue(c5ID, fmt.Sprintf("totalShares|%d", ep), want[ep].total,
			fmt.Sprintf("C5 totalShares epoch %d", ep))

		// Per-provider shares must match, AND the forfeiting providers must be absent
		// rather than present-with-zero: a zero share would still dilute nothing but
		// would mean the rule was applied at the wrong place.
		for who, share := range want[ep].shares {
			waitValue(c5ID, "share|"+strconv.FormatUint(ep, 10)+"|"+who, share,
				fmt.Sprintf("epoch %d share for %s", ep, who))
		}
		for _, who := range []string{exiter, flash} {
			if _, ok := want[ep].shares[who]; ok {
				continue
			}
			if v := stateOf(c5ID, "share|"+strconv.FormatUint(ep, 10)+"|"+who); v != "" && v != "0" {
				t.Fatalf("epoch %d: %s must earn nothing, got %q", ep, who, v)
			}
		}
	}
	t.Logf("LP SEMANTICS OK across 3 epochs: totals 3000/2000/5000")
	t.Logf("  mid-epoch top-up lifted epoch 2 only (grower 1000 -> 1000 -> 4000)")
	t.Logf("  mid-epoch exit forfeited from epoch 1 (exiter 1000 -> 0 -> 0)")
	t.Logf("  flash liquidity earned nothing in any epoch")

	// ---------------- claims: the shares must actually pay ----------------
	// Measured as a DELTA, because the deployer also holds the undrawn emission pool;
	// an absolute balance would mix the two (the reporter suite was caught by exactly
	// this). Payout per epoch = funded * share / totalShares.
	bal := func(acct string) *big.Int {
		return stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|"+acct)
	}
	for _, cl := range []struct {
		node int
		acct string
		want int64 // 10000+15000+6000 for steady; 10000+15000+24000 for grower
	}{
		{1, steady, 31000},
		{2, grower, 49000},
	} {
		before := bal(cl.acct)
		for ep := 0; ep < 3; ep++ {
			if _, err := d.CallContract(ctx, cl.node, c5ID, "claim",
				fmt.Sprintf(`{"epoch":"%d"}`, ep)); err != nil {
				t.Fatalf("%s claim epoch %d failed to broadcast: %v", cl.acct, ep, err)
			}
			if !waitStateKeyPresent(t, d, ctx, 1, c5ID,
				fmt.Sprintf("claimed|%d|%s", ep, cl.acct), 3*time.Minute) {
				t.Fatalf("%s claim for epoch %d never landed", cl.acct, ep)
			}
		}
		deadline := time.Now().Add(4 * time.Minute)
		var delta *big.Int
		for time.Now().Before(deadline) {
			delta = new(big.Int).Sub(bal(cl.acct), before)
			if delta.Cmp(big.NewInt(cl.want)) == 0 {
				break
			}
			time.Sleep(6 * time.Second)
		}
		if delta.Cmp(big.NewInt(cl.want)) != 0 {
			t.Fatalf("%s claimed %s across 3 epochs, want %d (balance was %s)",
				cl.acct, delta, cl.want, before)
		}
		t.Logf("%s claimed exactly %d across 3 epochs", cl.acct, cl.want)
	}

	if fx.hits == 0 {
		t.Fatal("the fake indexer was never queried — the reporter cannot have used it")
	}
	t.Logf("indexer served %d queries (page_size 2, so paging was exercised)", fx.hits)
	t.Logf("LP MULTI-EPOCH DEVNET PASSED — 3 epochs, real reporter in lp mode, claims paid")
}
