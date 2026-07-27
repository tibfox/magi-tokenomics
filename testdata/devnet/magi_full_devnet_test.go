package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDevnetMagiFull exercises EVERY current component in ONE devnet run.
//
// The three existing devnet suites each cover a slice (C0+C2+C3, C1+C2+C5+C6+C7,
// and the reporter), so nothing had ever proven the whole system works together:
// one token, one emission controller splitting into three different distributor
// types simultaneously, with staking underneath and the real reporter driving the
// content bucket.
//
//	PHASE 1  deploy all 7 contracts (spread across nodes so no single account
//	         carries every 10 HBD deploy fee)
//	PHASE 2  bootstrap: mint+transfer to C6, airdrop to holders, hand token to C2
//	PHASE 3  init C1, then STAKE, then init C2/C3/C5/C7 — the stake must exist
//	         before C2 sets genesis, or C7's epoch-0 yield is unclaimable
//	PHASE 4  C2 splits 50/30/20 into content/LP/yield
//	PHASE 5  one keeper poke funds all three buckets from a single emission
//	PHASE 6  content via the REAL reporter binary; LP direct; yield trustless
//	PHASE 7  claims + conservation invariants + outsider rejection
//
// Run: go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/
func TestDevnetMagiFull(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	if _, err := os.Stat(reporterBin); err != nil {
		t.Fatalf("reporter binary missing at %s — build it first", reporterBin)
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
	owner := w(1)    // deployer / trusted owner / reporter
	holderA := w(1)  // also a staker + content earner
	holderB := w(2)  // staker + curator
	outsider := w(3) // must never succeed at anything privileged
	treasury := w(4) // sweep destination
	guardian := w(5) // veto authority (disjoint from reporter — enforced at init)

	// ---------------- PHASE 1: deploy all seven ----------------
	//
	// Each deploy costs 10 HBD of the DEPLOYING identity's L1 balance, so spreading
	// them keeps one account from being drained. But the deployer also becomes the
	// contract's `owner`, and every `init` aborts unless msg.caller == owner — so a
	// contract MUST be initialised from the same node that deployed it. Deploys are
	// therefore spread only across the two nodes that get an RC deposit, and
	// `ownerNode` carries that pairing to PHASE 2/3.
	//
	// (Getting this wrong is what failed the first run: C2 deployed from node 3 and
	// initialised from node 1 aborted with "only owner can init".)
	deployN := func(name, wasm string, node int) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, err := d.DeployContract(ctx, ContractDeployOpts{
				WasmPath: wasm, Name: name, DeployerNode: node,
			})
			if err == nil {
				t.Logf("deployed %-18s = %s (node %d, attempt %d)", name, id, node, attempt)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = err
			t.Logf("deploy %s attempt %d failed: %v — settling", name, attempt, err)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	ownerNode := map[string]int{}
	dep := func(name, wasm string, node int) string {
		id := deployN(name, wasm, node)
		ownerNode[id] = node
		return id
	}
	tokenID := dep("magi-token", magiTokenWasm, 1)
	c2ID := dep("magi-c2-emission", magiWasm(t, "c2-emission/artifacts/main.wasm"), 1)
	c3ID := dep("magi-c3-content", magiWasm(t, "c3-distributor/artifacts/main.wasm"), 1)
	c6ID := dep("magi-c6-migration", magiWasm(t, "c6-migration/artifacts/main.wasm"), 1)
	c1ID := dep("magi-c1-staking", magiWasm(t, "c1-staking/artifacts/main.wasm"), 2)
	c5ID := dep("magi-c5-lp", magiWasm(t, "c5-lp/artifacts/main.wasm"), 2)
	c7ID := dep("magi-c7-yield", magiWasm(t, "c7-yield/artifacts/main.wasm"), 2)
	t.Logf("all 7 deployed: token=%s c1=%s c2=%s c3=%s c5=%s c6=%s c7=%s",
		tokenID, c1ID, c2ID, c3ID, c5ID, c6ID, c7ID)

	// Deposit AFTER deploying — depositing first drains the L1 balance the deploy
	// fee comes out of. This run makes ~30 contract calls, so fund generously.
	for _, node := range []int{1, 2} {
		for _, amt := range []string{"200.000", "100.000", "50.000", "30.000"} {
			if _, ferr := d.Deposit(ctx, node, amt, "hbd"); ferr == nil {
				t.Logf("node %d deposited %s HBD for RC", node, amt)
				break
			}
		}
	}
	time.Sleep(20 * time.Second)

	// ---------------- helpers ----------------
	callN := func(node int, id, action, payload, what string) {
		if _, err := d.CallContract(ctx, node, id, action, payload); err != nil {
			t.Fatalf("%s: broadcast failed: %v", what, err)
		}
		t.Logf("sent: %s", what)
		time.Sleep(9 * time.Second)
	}
	call := func(id, action, payload, what string) { callN(1, id, action, payload, what) }
	// callOwner routes an owner-only action to whichever node deployed (and so
	// owns) that contract.
	callOwner := func(id, action, payload, what string) { callN(ownerNode[id], id, action, payload, what) }
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
	stateOf := func(id, key string) string {
		st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
		if err != nil {
			return ""
		}
		if v, ok := st[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	// waitValue polls until a key holds an exact value. Aggregates like
	// total_staked / owed / funded EXIST as soon as the FIRST contributing tx lands
	// and then GROW, so waiting only for presence samples a partial sum — that is
	// how this test first read total_staked=600 while B's stake was still in flight.
	waitValue := func(id, key, want, what string) {
		deadline := time.Now().Add(4 * time.Minute)
		last := ""
		for time.Now().Before(deadline) {
			st, err := d.GetStateByKeys(ctx, 1, id, []string{key})
			if err == nil {
				if v, ok := st[key]; ok && v != nil {
					last = fmt.Sprintf("%v", v)
					if last == want {
						t.Logf("%s = %s", what, want)
						return
					}
				}
			}
			time.Sleep(6 * time.Second)
		}
		t.Fatalf("%s never reached %s (last seen %q) — %s[%s]", what, want, last, id, key)
	}
	bigOf := func(s string) *big.Int {
		v, ok := new(big.Int).SetString(s, 10)
		if !ok {
			return new(big.Int)
		}
		return v
	}

	const epochLen = 10

	// initAs runs a contract's init from its owning node and blocks until a key it
	// writes is visible. A silent init failure otherwise only surfaces minutes later
	// at an unrelated assertion — which is exactly how the first run wasted 15 min.
	initAs := func(id, payload, proofKey, what string) {
		callOwner(id, "init", payload, what)
		waitKey(id, proofKey, what+" (confirming)")
	}

	// ---------------- PHASE 2: bootstrap supply + airdrop ----------------
	//
	// C6 pays airdrops out of its OWN balance, so it must be funded while the
	// deployer still owns the token — i.e. before ownership moves to C2.
	initAs(tokenID, `{"name":"MagiFull","symbol":"MFULL","decimals":0,"maxSupply":"100000000"}`,
		"owner", "token init")
	// MintPayload carries only {amount}: mint credits the OWNER, it cannot target an
	// address. Funding C6 is therefore mint-then-transfer, and both must happen
	// while the deployer still owns the token.
	callOwner(tokenID, "mint", `{"amount":"1000"}`, "mint 1000 to owner")
	callOwner(tokenID, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, c6ID), "fund C6 with the airdrop float")
	initAs(c6ID, fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"1000"}`, tokenID), "cfg_token", "C6 init")
	callOwner(c6ID, "airdropBatch", fmt.Sprintf(
		`{"batchId":"1","entries":"hive:%s:600,hive:%s:400"}`, holderA, holderB), "C6 airdrop")
	callOwner(tokenID, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token ownership -> C2")

	// ---------------- PHASE 3: init the rest ----------------
	initAs(c1ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"%d","epochLen":"%d","allow":""}`,
		tokenID, epochLen*3, epochLen), "cfg_token", "C1 init")

	// ---------------- PHASE 4: stake (BEFORE C2 init) ----------------
	//
	// Ordering is load-bearing. C7 credits min(stakeAt(hStart), stakeAt(hEnd)) for
	// the epoch, and `genesis` is whatever height C2 initialises at — so staking
	// after C2 init means the stake is 0 at BOTH epoch-0 boundaries and nobody can
	// ever claim yield for it. Stake first, then start the emission clock.
	callN(1, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, c1ID), "A approve C1")
	callN(1, c1ID, "stake", `{"amount":"600"}`, "A stake 600")
	callN(2, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"400"}`, c1ID), "B approve C1")
	callN(2, c1ID, "stake", `{"amount":"400"}`, "B stake 400")
	waitValue(c1ID, "total_staked", "1000", "C1 total_staked")

	// One emission, split three ways: content 50%, LP 30%, yield 20%.
	initAs(c2ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"content","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:5000,lp:contract:%s:3000,yield:contract:%s:2000"}`,
		tokenID, epochLen, guardian, treasury, c3ID, c5ID, c7ID), "cfg_genesis", "C2 init")
	genesis := bigOf(waitKey(c2ID, "cfg_genesis", "C2 genesis")).Uint64()
	t.Logf("C2 genesis=%d epochLen=%d -> epoch 0 = blocks %d..%d",
		genesis, epochLen, genesis, genesis+epochLen-1)

	distInit := func(id, what string) {
		initAs(id, fmt.Sprintf(
			`{"token":"%s","kind":"0","funder":"%s","genesis":"%d","epochLen":"%d","window":"1",`+
				`"reporterMode":"0","reporterAuth":"hive:%s","reporterThreshold":"1",`+
				`"treasury":"hive:%s","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
			tokenID, c2ID, genesis, epochLen, owner, treasury, guardian), "cfg_funder", what)
	}
	distInit(c3ID, "C3 init (content)")
	distInit(c5ID, "C5 init (LP)")
	initAs(c7ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, c1ID, treasury, guardian), "cfg_funder", "C7 init (yield)")

	// ---------------- PHASE 5: one poke funds three buckets ----------------
	t.Logf("waiting for epoch 0 to close (block %d)...", genesis+epochLen)
	time.Sleep(time.Duration(epochLen+8) * 3 * time.Second)
	call(c2ID, "distributeEpoch", `{}`, "keeper poke")
	waitKey(c2ID, "cfg_lastEpoch", "C2 lastEpoch")

	// emission = baseAnnual*epochLen/blocksPerYear = 1000000*10/1000 = 10000
	// content 5000bps=5000, lp 3000bps=3000, yield 2000bps=2000
	// NB: C2 keys allocations by bucket TARGET, not bucket name (addOwed uses the
	// target string), so the key is owed|contract:<id>|<epoch>.
	for _, b := range []struct{ name, id, want string }{
		{"content", c3ID, "5000"}, {"lp", c5ID, "3000"}, {"yield", c7ID, "2000"},
	} {
		waitValue(c2ID, "owed|contract:"+b.id+"|0", b.want, "C2 owed["+b.name+"]")
	}

	// ---------------- PHASE 6: three distributors, three mechanisms ----------
	call(c3ID, "pullFunding", `{"epoch":"0"}`, "C3 pull")
	call(c5ID, "pullFunding", `{"epoch":"0"}`, "C5 pull")
	call(c7ID, "pullFunding", `{"epoch":"0"}`, "C7 pull")

	// -- content: driven by the REAL reporter binary over injected Hive data --
	fixture := buildHiveFixture(genesis, epochLen, time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), holderA, holderB)
	hive := fixture.serve(t)
	defer hive.Close()

	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "reporter.json")
	blob, _ := json.MarshalIndent(map[string]any{
		"hive":      map[string]any{"api": []string{hive.URL}},
		"vsc":       map[string]any{"api": d.GQLEndpoint(1), "net_id": "vsc-devnet"},
		"contracts": map[string]any{"distributor": c3ID, "funder": c2ID, "stake": c1ID},
		"epoch":     map[string]any{"genesis": genesis, "len": epochLen},
		"source": map[string]any{"tag": "magitribe", "limit": 100,
			"attribution": "cashout", "weight": "hive_rshares", "exclude": []string{}},
		"shares": map[string]any{"author_reward_bps": 5000, "author_curve": "1/1",
			"curation_curve": "1/2", "muted": []string{}},
		"page": map[string]any{"max_entries": 4, "max_bytes": 3800},
		"submit": map[string]any{"account": owner, "rc_limit": 200000,
			"progress_file": filepath.Join(workDir, "progress.json"),
			"keeper":        false, "pull_funding": false, "finalize": true},
	}, "", "  ")
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
	planJSON := runReporter("plan", "-json")
	var plan reporterPlan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		t.Fatalf("reporter plan not json: %v\n%s", err, planJSON)
	}
	expected := reporterCompute(t, runReporter, plan.Epoch)
	t.Logf("reporter: epoch %s, %d calls, totalShares=%s across %d accounts",
		plan.Epoch, len(plan.Calls), expected.TotalShares, expected.Accounts)
	for i, c := range plan.Calls {
		callN(1, c.ContractID, c.Action, c.Payload, fmt.Sprintf("C3 reporter plan[%d] %s", i, c.Action))
	}

	// -- LP: shares pushed directly (C5 is byte-identical to C3; the reporter
	//    path is already proven above, so this covers the direct-push mode) --
	call(c5ID, "submitShares", fmt.Sprintf(
		`{"epoch":"0","page":"0","entries":"hive:%s:70,hive:%s:30"}`, holderA, holderB), "C5 LP shares")
	call(c5ID, "finalizeEpoch", `{"epoch":"0"}`, "C5 finalize")

	// ---------------- PHASE 7: claims, invariants, outsider ----------------
	if st := waitKey(c3ID, "status|0", "C3 status"); st != "finalized" {
		t.Fatalf("C3 status=%q", st)
	}
	if st := waitKey(c5ID, "status|0", "C5 status"); st != "finalized" {
		t.Fatalf("C5 status=%q", st)
	}
	// each bucket's slice must have fully arrived before anything is compared
	waitValue(c3ID, "funded|0", "5000", "C3 funded")
	waitValue(c5ID, "funded|0", "3000", "C5 funded")
	waitValue(c7ID, "funded|0", "2000", "C7 funded")
	c3Funded, c3Total := stateOf(c3ID, "funded|0"), waitKey(c3ID, "totalShares|0", "C3 totalShares")
	c5Funded := stateOf(c5ID, "funded|0")
	c7Funded := stateOf(c7ID, "funded|0")
	t.Logf("funded: content=%s lp=%s yield=%s", c3Funded, c5Funded, c7Funded)

	// the reporter and the chain must agree — the seam, on a live chain.
	// totalShares grows per page, so wait for the final value first.
	waitValue(c3ID, "totalShares|0", expected.TotalShares, "C3 totalShares")
	c3Total = stateOf(c3ID, "totalShares|0")
	if expected.TotalShares != c3Total {
		t.Fatalf("SEAM BROKEN: reporter totalShares=%s, chain=%s", expected.TotalShares, c3Total)
	}
	time.Sleep(15 * time.Second) // challenge window
	claim := func(node int, id, what string) { callN(node, id, "claim", `{"epoch":"0"}`, what) }
	claim(1, c3ID, "A claims content")
	claim(2, c3ID, "B claims content")
	claim(1, c5ID, "A claims LP")
	claim(2, c5ID, "B claims LP")
	claim(1, c7ID, "A claims yield")
	claim(2, c7ID, "B claims yield")

	// Every claim must be CONFIRMED on chain before anything is compared. A single
	// immediate read here is what failed run 4: the last claim had been broadcast
	// only ~9s earlier and its state was not queryable yet, which looked identical
	// to "the claim was rejected".
	for _, c := range []struct{ id, name string }{
		{c3ID, "content"}, {c5ID, "LP"}, {c7ID, "yield"},
	} {
		for _, acct := range []string{holderA, holderB} {
			if !waitStateKeyPresent(t, d, ctx, 1, c.id, "claimed|0|hive:"+acct, 3*time.Minute) {
				t.Fatalf("%s never claimed %s", acct, c.name)
			}
		}
	}
	t.Logf("all six claims confirmed on chain")

	// CONSERVATION — end to end, against real token balances.
	//
	// C3/C5 record only `claimed|<ep>|<acct>` = "1" (no per-account amount), so the
	// only honest check is the holder's actual token balance. Each holder airdropped
	// N and staked all of it, so their balance is exactly the sum of the three
	// claims — which is also a cross-contract conservation proof.
	c3TotalN := bigOf(c3Total)
	c5TotalN := bigOf(waitKey(c5ID, "totalShares|0", "C5 totalShares"))
	share := func(id, acct string) *big.Int { return bigOf(stateOf(id, "share|0|hive:"+acct)) }
	payout := func(funded string, sh, total *big.Int) *big.Int {
		if total.Sign() == 0 {
			return new(big.Int)
		}
		return new(big.Int).Div(new(big.Int).Mul(bigOf(funded), sh), total)
	}
	stakeOf := map[string]*big.Int{holderA: big.NewInt(600), holderB: big.NewInt(400)}
	for _, acct := range []string{holderA, holderB} {
		content := payout(c3Funded, share(c3ID, acct), c3TotalN)
		lp := payout(c5Funded, share(c5ID, acct), c5TotalN)
		// C7 is trustless pro-rata over C1 stake, not over submitted shares
		yield := payout(c7Funded, stakeOf[acct], big.NewInt(1000))
		want := new(big.Int).Add(content, new(big.Int).Add(lp, yield))

		got := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+acct)
		t.Logf("%s: content=%s lp=%s yield=%s -> want %s, on-chain balance %s",
			acct, content, lp, yield, want, got)
		if got.Cmp(want) != 0 {
			t.Fatalf("%s token balance %s != content+lp+yield %s", acct, got, want)
		}
	}

	// and no distributor may pay out more than it was funded
	for _, c := range []struct {
		funded string
		a, b   *big.Int
		name   string
	}{
		{c3Funded, payout(c3Funded, share(c3ID, holderA), c3TotalN), payout(c3Funded, share(c3ID, holderB), c3TotalN), "content"},
		{c5Funded, payout(c5Funded, share(c5ID, holderA), c5TotalN), payout(c5Funded, share(c5ID, holderB), c5TotalN), "lp"},
		{c7Funded, payout(c7Funded, big.NewInt(600), big.NewInt(1000)), payout(c7Funded, big.NewInt(400), big.NewInt(1000)), "yield"},
	} {
		paid := new(big.Int).Add(c.a, c.b)
		if paid.Cmp(bigOf(c.funded)) > 0 {
			t.Fatalf("INVARIANT VIOLATED in %s: paid %s > funded %s", c.name, paid, c.funded)
		}
	}

	// token ownership must still sit with C2 after all of that
	if o := stateOf(tokenID, "owner"); o != "contract:"+c2ID {
		t.Fatalf("token owner drifted to %q, want contract:%s", o, c2ID)
	}

	// ---------------- outsider must be rejected everywhere ----------------
	before := map[string]string{
		"c3total": stateOf(c3ID, "totalShares|0"),
		"c5total": stateOf(c5ID, "totalShares|0"),
		"owner":   stateOf(tokenID, "owner"),
		"c1total": stateOf(c1ID, "total_staked"),
	}
	for _, a := range []struct{ id, action, payload, what string }{
		{tokenID, "mint", `{"amount":"999999"}`, "outsider mints"},
		{tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"hive:%s"}`, outsider), "outsider seizes token"},
		{c3ID, "submitShares", fmt.Sprintf(`{"epoch":"1","page":"0","entries":"hive:%s:999"}`, outsider), "outsider fakes content shares"},
		{c5ID, "submitShares", fmt.Sprintf(`{"epoch":"1","page":"0","entries":"hive:%s:999"}`, outsider), "outsider fakes LP shares"},
		{c6ID, "airdropBatch", fmt.Sprintf(`{"batchId":"9","entries":"hive:%s:1000"}`, outsider), "outsider airdrops to self"},
		{c2ID, "claimBucket", `{"epoch":"0"}`, "outsider impersonates a bucket"},
		{c7ID, "sweepResidual", `{"epoch":"0","amount":"2000"}`, "outsider sweeps yield"},
	} {
		if _, err := d.CallContract(ctx, 3, a.id, a.action, a.payload); err != nil {
			t.Logf("attack rejected at broadcast (%s): %v", a.what, err)
			continue
		}
		t.Logf("attack broadcast (must fail on-chain): %s", a.what)
		time.Sleep(9 * time.Second)
	}
	time.Sleep(45 * time.Second) // let any successful attack settle before asserting nothing moved
	for k, want := range before {
		var got string
		switch k {
		case "c3total":
			got = stateOf(c3ID, "totalShares|0")
		case "c5total":
			got = stateOf(c5ID, "totalShares|0")
		case "owner":
			got = stateOf(tokenID, "owner")
		case "c1total":
			got = stateOf(c1ID, "total_staked")
		}
		if got != want {
			t.Fatalf("OUTSIDER CHANGED STATE %s: %q -> %q", k, want, got)
		}
	}
	if s := stateOf(c3ID, "totalShares|1"); s != "" && s != "0" {
		t.Fatalf("outsider injected epoch-1 content shares: %s", s)
	}

	t.Logf("FULL SYSTEM DEVNET PASSED — 7 contracts + reporter, one emission split 3 ways")
	t.Logf("hive fixture calls: %v", fixture.hits)
}
