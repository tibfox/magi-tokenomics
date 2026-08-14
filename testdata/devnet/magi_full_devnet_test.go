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
	"strings"
	"testing"
	"time"
)

// TestDevnetMagiFull exercises EVERY current component in ONE devnet run.
//
// The other suites each cover a slice, so this is the one that proves the whole system
// works together: one token, one emission controller splitting three ways, staking
// underneath it, and the real reporter driving the content channel.
//
// Four contracts now, not seven — yield and the airdrop live inside C1, and content
// and LP are two CHANNELS on one distributor rather than two deployments.
//
//	PHASE 1  deploy token + C1 + C2 + distributor (spread across nodes so no single
//	         account carries every 10 HBD deploy fee)
//	PHASE 2  bootstrap: mint, fund C1's airdrop float, init C1 (one init, all three
//	         roles), airdrop to holders, hand the token to C2
//	PHASE 3  STAKE — this must happen before C2 sets genesis, or epoch-0 yield is
//	         unclaimable
//	PHASE 4  init C2 (splitting 50/30/20 into content/LP/yield) and the distributor's
//	         two channels; C1 then adopts the schedule and its yield bucket
//	PHASE 5  one keeper poke funds all three buckets from a single emission
//	PHASE 6  content via the REAL reporter binary; LP direct; yield trustless
//	PHASE 7  claims + conservation invariants
//	PHASE 7b a malicious STAKED HOLDER (real stake, real share, already claimed)
//	PHASE 8  a pure outsider sweeps every privileged action on all of them
//
// Run: go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/
func TestDevnetMagiFull(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	requireDiskSpace(t)
	if _, err := os.Stat(reporterBin); err != nil {
		t.Fatalf("reporter binary missing at %s — build it first", reporterBin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	cfg := magiDevnetConfig()
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
	c3ID := dep("magi-distributor", magiWasm(t, "c3-distributor/artifacts/main.wasm"), 1)
	c1ID := dep("magi-c1-staking", magiWasm(t, "c1-staking/artifacts/main.wasm"), 2)
	t.Logf("all 4 deployed: token=%s c1=%s c2=%s distributor=%s", tokenID, c1ID, c2ID, c3ID)

	// Deposit AFTER deploying — depositing first drains the L1 balance the deploy
	// fee comes out of.
	//
	// Fund every actor as heavily as L1 allows, the ATTACKER included. RC is
	// `ledger HBD + 10_000 free`, and the free tier alone is only ~7 transactions —
	// an attack that dies of RC exhaustion proves nothing about authorisation, so a
	// thinly-funded attacker turns the whole adversarial phase into a false pass.
	// Deposit in repeated descending rounds so each account moves as much L1 balance
	// into the L2 ledger as it has, rather than stopping at the first success.
	fundNode := func(node int) {
		moved := 0
		for round := 0; round < 4; round++ {
			progressed := false
			for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
				if _, ferr := d.Deposit(ctx, node, amt, "hbd"); ferr == nil {
					t.Logf("node %d deposited %s HBD (round %d)", node, amt, round+1)
					moved++
					progressed = true
					break
				}
			}
			if !progressed {
				break
			}
		}
		if moved == 0 {
			t.Fatalf("node %d could not deposit anything — it would run on the 10k free tier alone", node)
		}
	}
	// Node 5 is the C2 guardian and drives the token-op passthrough in PHASE 9. An
	// unfunded guardian would abort on RC rather than on policy, which proves nothing.
	for _, node := range []int{1, 2, 3, 5} {
		fundNode(node)
	}
	// Deposits are L1 transfers to the gateway and take a while to credit the L2
	// ledger — a fixed sleep is not enough (25s produced hbd=0 for every account and
	// tripped the assertion below). Poll until they land.
	//
	// Then PROVE the RC headroom rather than assume it: the attacker in particular
	// must be funded well past the 10_000 free tier, or PHASE 7b/8 are not testing
	// what they claim to.
	type acct struct {
		node int
		name string
	}
	funded := []acct{{1, owner}, {2, holderB}, {3, outsider}}
	deadline := time.Now().Add(4 * time.Minute)
	for {
		allCredited := true
		for _, a := range funded {
			b, berr := d.GetAccountBalance(ctx, 1, "hive:"+a.name)
			if berr != nil || b == nil || b.Hbd <= 0 {
				allCredited = false
				break
			}
		}
		if allCredited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deposits never credited the L2 ledger — every actor would run on " +
				"the 10k free tier (~7 txs), so the adversarial phases would fail on RC " +
				"rather than on authorisation")
		}
		time.Sleep(6 * time.Second)
	}
	for _, a := range funded {
		b, _ := d.GetAccountBalance(ctx, 1, "hive:"+a.name)
		t.Logf("ledger balance hive:%-14s hbd=%d  -> RC ~%d", a.name, b.Hbd, b.Hbd+10000)
		if a.node == 3 && b.Hbd < 10000 {
			t.Fatalf("attacker ledger hbd=%d is too thin: the PHASE 8 outsider sweep needs RC "+
				"well past the free tier or it fails on RC, not on authority", b.Hbd)
		}
	}

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
	// The airdrop pays out of C1's OWN balance, so C1 must be funded while the
	// deployer still owns the token — i.e. before ownership moves to C2.
	initAs(tokenID, `{"name":"MagiFull","symbol":"MFULL","decimals":0,"maxSupply":"100000000"}`,
		"owner", "token init")
	// MintPayload carries only {amount}: mint credits the OWNER, it cannot target an
	// address. Funding C1 is therefore mint-then-transfer, and both must happen
	// while the deployer still owns the token.
	callOwner(tokenID, "mint", `{"amount":"1000"}`, "mint 1000 to owner")
	callOwner(tokenID, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, c1ID), "fund C1 with the airdrop float")

	// ONE init for all three of C1's roles, and it has to happen HERE — the airdrop
	// below needs the contract live while the deployer still owns the token, and a
	// contract may only be initialised once.
	//
	// Staking is the base role, so cooldown and epochLen are mandatory; yield and the
	// airdrop are configured rather than switched on, which is why treasury/guardian
	// (for sweepEmptyEpoch) and maxAirdrop appear in the same payload. `funder` is
	// deliberately ABSENT: C2 does not exist yet, and C1's init cross-checks epochLen
	// against the funder's schedule when one is named. The funder and yield bucket are
	// adopted in PHASE 4 instead, once C2 is up.
	initAs(c1ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"%d","epochLen":"%d","allow":"",`+
			`"maxAirdrop":"1000","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, epochLen*3, epochLen, treasury, guardian), "cfg_token", "C1 init (staking+yield+airdrop)")

	callOwner(c1ID, "airdropBatch", fmt.Sprintf(
		`{"batchId":"1","entries":"hive:%s:600,hive:%s:400"}`, holderA, holderB), "C1 airdrop")
	// C2 no longer mints — it PULLS each epoch's emission from an account that has
	// approved it. Mint the pool and approve C2 BEFORE handing the token over, since
	// only the owner may mint. (C2 no longer needs to own the token at all; the
	// handover below is kept only so the guardian token-op passthrough stays live.)
	callOwner(tokenID, "mint", `{"amount":"1000000"}`, "mint the emission pool")
	callOwner(tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2 to draw the pool")

	callOwner(tokenID, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token ownership -> C2")

	// ---------------- PHASE 3: stake (BEFORE C2 init) ----------------
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

	// ---------------- PHASE 4: init C2 + the distributor's channels ----------
	//
	// One emission, split three ways: content 50%, LP 30%, yield 20%.
	initAs(c2ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"content","timelock":"60",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:5000,lp:contract:%s:3000,yield:contract:%s:2000"}`,
		tokenID, epochLen, guardian, treasury, c3ID, c3ID, c1ID), "cfg_genesis", "C2 init")
	genesis := bigOf(waitKey(c2ID, "cfg_genesis", "C2 genesis")).Uint64()
	t.Logf("C2 genesis=%d epochLen=%d -> epoch 0 = blocks %d..%d",
		genesis, epochLen, genesis, genesis+epochLen-1)

	// ONE distributor, TWO channels. Content and LP used to be two deployed copies of
	// the same contract; they are now two channels on one, each with its own funding
	// bucket, share book and reporter authority.
	initAs(c3ID, fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","genesis":"%d","epochLen":"%d",`+
			`"treasury":"hive:%s","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, genesis, epochLen, treasury, guardian), "cfg_funder", "distributor init")
	for _, ch := range []struct{ name, role string }{{"content", "content"}, {"lp", "lp"}} {
		call(c3ID, "addChannel", fmt.Sprintf(
			`{"channel":"%s","bucket":"%s","window":"1","reporterMode":"0",`+
				`"reporterAuth":"hive:%s","reporterThreshold":"1","role":"%s"}`,
			ch.name, ch.name, owner, ch.role), "distributor addChannel "+ch.name)
		waitKey(c3ID, "ch_bucket|"+ch.name, ch.name+" channel registered")
	}
	// C1 adopts the emission schedule now that C2 exists. This is what arms the
	// per-epoch drawdown accumulator that gives yield an EXACT denominator; dividing
	// by min(Σa,Σb) instead strands part of every epoch, which is what would force a
	// claim deadline. It also names the bucket pullFunding draws from — until this
	// call lands, C1 has staking and an airdrop but no way to fund yield.
	// callOwner, NOT call: adoptSchedule is owner-only, and C1 is the one contract
	// deployed from node 2 rather than node 1. Anchoring the schedule is exactly the
	// kind of thing an outsider must not be able to do, so the generic node-1 caller
	// gets refused here — see the C1 re-init row in PHASE 8 for the same guard.
	callOwner(c1ID, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), "C1 adopt schedule + yield bucket")
	waitKey(c1ID, "cfg_genesis", "C1 schedule adopted")
	waitKey(c1ID, "cfg_bucket", "C1 yield bucket adopted")

	// ---------------- PHASE 5: one poke funds three buckets ----------------
	t.Logf("waiting for epoch 0 to close (block %d)...", genesis+epochLen)
	time.Sleep(time.Duration(epochLen+8) * 3 * time.Second)
	call(c2ID, "distributeEpoch", `{}`, "keeper poke")
	waitKey(c2ID, "cfg_lastEpoch", "C2 lastEpoch")

	// emission = baseAnnual*epochLen/blocksPerYear = 1000000*10/1000 = 10000
	// content 5000bps=5000, lp 3000bps=3000, yield 2000bps=2000
	//
	// NB: C2 keys allocations by bucket NAME, not by target. It has to: content and lp
	// are two channels on the SAME distributor, so a target-keyed ledger would collapse
	// both into one entry and neither channel could tell what it was owed.
	for _, b := range []struct{ name, want string }{
		{"content", "5000"}, {"lp", "3000"}, {"yield", "2000"},
	} {
		waitValue(c2ID, "owed|"+b.name+"|0", b.want, "C2 owed["+b.name+"]")
	}

	// ---------------- PHASE 6: three distributors, three mechanisms ----------
	call(c3ID, "pullFunding", `{"channel":"content","epoch":"0"}`, "C3 pull")
	call(c3ID, "pullFunding", `{"channel":"lp","epoch":"0"}`, "C5 pull")
	call(c1ID, "pullFunding", `{"epoch":"0"}`, "C7 pull")

	// -- content: driven by the REAL reporter binary over injected Hive data --
	fixture := buildHiveFixture(genesis, epochLen, time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), holderA, holderB)
	hive := fixture.serve(t)
	defer hive.Close()

	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "reporter.json")
	blob, _ := json.MarshalIndent(map[string]any{
		"hive":      map[string]any{"api": []string{hive.URL}},
		"vsc":       map[string]any{"api": d.GQLEndpoint(1), "net_id": "vsc-devnet"},
		"contracts": map[string]any{"distributor": c3ID, "channel": "content", "funder": c2ID, "stake": c1ID},
		"epoch":     map[string]any{"genesis": genesis, "len": epochLen},
		"source": map[string]any{"tags": []string{"magitribe"}, "limit": 100,
			"weight": "hive_rshares", "exclude": []string{}},
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

	// -- LP: shares pushed directly. Same contract and same code as content above,
	//    just a different channel — the reporter path is already proven, so this
	//    covers the direct-push mode. --
	call(c3ID, "submitShares", fmt.Sprintf(
		`{"channel":"lp","epoch":"0","page":"0","entries":"hive:%s:70,hive:%s:30"}`,
		holderA, holderB), "lp shares")
	// The LP channel is pushed BY HAND, not through the reporter's Hive pipeline, so
	// nothing computed a commitment for it — and finalizeEpoch refuses an epoch with
	// no root. `reporter root` does that arithmetic over the same entries string,
	// which is what an operator submitting a list by hand has to do.
	lpEntries := fmt.Sprintf("hive:%s:70,hive:%s:30", holderA, holderB)
	var lpRoot struct {
		Root        string `json:"root"`
		TotalShares string `json:"total_shares"`
		Accounts    int    `json:"accounts"`
	}
	if err := json.Unmarshal(runReporter("root", "-entries", lpEntries, "-json"), &lpRoot); err != nil {
		t.Fatalf("reporter root for the lp channel: %v", err)
	}
	call(c3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"lp","epoch":"0","root":"%s","totalShares":"%s","accounts":"%d"}`,
		lpRoot.Root, lpRoot.TotalShares, lpRoot.Accounts), "lp root")
	call(c3ID, "finalizeEpoch", `{"channel":"lp","epoch":"0"}`, "lp finalize")

	// ---------------- PHASE 7: claims, invariants, outsider ----------------
	if st := waitKey(c3ID, "status|content|0", "C3 status"); st != "finalized" {
		t.Fatalf("C3 status=%q", st)
	}
	if st := waitKey(c3ID, "status|lp|0", "C5 status"); st != "finalized" {
		t.Fatalf("C5 status=%q", st)
	}
	// each bucket's slice must have fully arrived before anything is compared
	waitValue(c3ID, "funded|content|0", "5000", "C3 funded")
	waitValue(c3ID, "funded|lp|0", "3000", "C5 funded")
	waitValue(c1ID, "y_funded|0", "2000", "C7 funded")
	c3Funded, c3Total := stateOf(c3ID, "funded|content|0"), waitKey(c3ID, "totalShares|content|0", "C3 totalShares")
	c5Funded := stateOf(c3ID, "funded|lp|0")
	c7Funded := stateOf(c1ID, "y_funded|0")
	t.Logf("funded: content=%s lp=%s yield=%s", c3Funded, c5Funded, c7Funded)

	// the reporter and the chain must agree — the seam, on a live chain.
	// totalShares grows per page, so wait for the final value first.
	waitValue(c3ID, "totalShares|content|0", expected.TotalShares, "C3 totalShares")
	c3Total = stateOf(c3ID, "totalShares|content|0")
	if expected.TotalShares != c3Total {
		t.Fatalf("SEAM BROKEN: reporter totalShares=%s, chain=%s", expected.TotalShares, c3Total)
	}
	time.Sleep(15 * time.Second) // challenge window

	// Snapshot before claiming. An ABSOLUTE balance check no longer works: holderA
	// is also the emission pool holder, so its balance carries ~990k of undrawn
	// pool. What must hold is that each holder GAINS exactly content+lp+yield.
	preClaim := map[string]*big.Int{
		holderA: stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+holderA),
		holderB: stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+holderB),
	}
	// A claim is per (contract, channel), and one shared helper cannot express that:
	// content and lp are two CHANNELS on one distributor, while yield is a different
	// contract with a different entrypoint. The single `claim(node, id, what)` helper
	// that used to be here sent {"epoch":"0"} to everything, which meant the two
	// distributor claims were the same call issued twice and the yield claim named an
	// action C1 does not export (it has claimYield, not claim).
	// Every claim now carries a proof. Content comes from `reporter proof`, which
	// recomputes the epoch from Hive; LP comes from `reporter root -account`, because
	// that list never went through the Hive pipeline.
	claimParts := func(ch, acct string) (share, proof string) {
		var pf struct {
			Share string   `json:"share"`
			Proof []string `json:"proof"`
		}
		var raw []byte
		if ch == "content" {
			raw = runReporter("proof", "-epoch", "0", "-account", "hive:"+acct, "-json")
		} else {
			raw = runReporter("root", "-entries", lpEntries, "-account", "hive:"+acct, "-json")
		}
		if err := json.Unmarshal(raw, &pf); err != nil {
			t.Fatalf("proof for %s on %s: %v\n%s", acct, ch, err, raw)
		}
		return pf.Share, strings.Join(pf.Proof, ",")
	}
	claimPayloadEp := func(ch, acct, ep string) string {
		share, proof := claimParts(ch, acct)
		return fmt.Sprintf(`{"channel":"%s","epoch":"%s","share":"%s","proof":"%s"}`, ch, ep, share, proof)
	}
	claimPayload := func(ch, acct string) string { return claimPayloadEp(ch, acct, "0") }
	claimDist := func(node int, ch, acct, what string) {
		callN(node, c3ID, "claim", claimPayload(ch, acct), what)
	}
	claimDist(1, "content", holderA, "A claims content")
	claimDist(2, "content", holderB, "B claims content")
	claimDist(1, "lp", holderA, "A claims LP")
	claimDist(2, "lp", holderB, "B claims LP")
	callN(1, c1ID, "claimYield", `{"epoch":"0"}`, "A claims yield")
	callN(2, c1ID, "claimYield", `{"epoch":"0"}`, "B claims yield")

	// Every claim must be CONFIRMED on chain before anything is compared. A single
	// immediate read here is what failed run 4: the last claim had been broadcast
	// only ~9s earlier and its state was not queryable yet, which looked identical
	// to "the claim was rejected".
	// Each of the three carries its OWN key shape: the distributor scopes by channel
	// (claimed|<ch>|<ep>|<acct>) and C1's yield does not (y_claimed|<ep>|<acct>). The
	// shared "claimed|0|hive:" key used here before matched neither, so all six waits
	// polled a key nothing ever writes.
	for _, c := range []struct{ id, prefix, name string }{
		{c3ID, "claimed|content|0|hive:", "content"},
		{c3ID, "claimed|lp|0|hive:", "LP"},
		{c1ID, "y_claimed|0|hive:", "yield"},
	} {
		for _, acct := range []string{holderA, holderB} {
			if !waitStateKeyPresent(t, d, ctx, 1, c.id, c.prefix+acct, 3*time.Minute) {
				t.Fatalf("%s never claimed %s", acct, c.name)
			}
		}
	}
	t.Logf("all six claims confirmed on chain")

	// CONSERVATION — end to end, against real token balances.
	//
	// C3/C5 record only `claimed|<ep>|<acct>` = "1" (no per-account amount), so the
	// only honest measure is the holder's actual token balance — as a DELTA across
	// the claims, since holderA also holds the undrawn emission pool.
	c3TotalN := bigOf(c3Total)
	c5TotalN := bigOf(waitKey(c3ID, "totalShares|lp|0", "C5 totalShares"))
	// Shares are per CHANNEL, and the chain no longer stores them — it stores a root.
	// The reporter is the authority on what an account earned, so ask IT: `reporter
	// proof` recomputes the epoch from Hive and returns the share it committed to.
	// That is a stronger check than reading state was, because it re-derives the
	// number rather than reading back whatever the contract happened to record.
	share := func(ch, acct string) *big.Int {
		if ch != "content" {
			// lp shares were pushed directly by this suite (70/30), not by the
			// reporter, so the suite already knows them.
			if acct == holderA {
				return big.NewInt(70)
			}
			return big.NewInt(30)
		}
		var pf struct {
			Share string `json:"share"`
		}
		out := runReporter("proof", "-epoch", "0", "-account", "hive:"+acct, "-json")
		if err := json.Unmarshal(out, &pf); err != nil {
			t.Fatalf("reporter proof for %s: %v\n%s", acct, err, out)
		}
		return bigOf(pf.Share)
	}
	payout := func(funded string, sh, total *big.Int) *big.Int {
		if total.Sign() == 0 {
			return new(big.Int)
		}
		return new(big.Int).Div(new(big.Int).Mul(bigOf(funded), sh), total)
	}
	stakeOf := map[string]*big.Int{holderA: big.NewInt(600), holderB: big.NewInt(400)}
	for _, acct := range []string{holderA, holderB} {
		content := payout(c3Funded, share("content", acct), c3TotalN)
		lp := payout(c5Funded, share("lp", acct), c5TotalN)
		// C7 is trustless pro-rata over C1 stake, not over submitted shares
		yield := payout(c7Funded, stakeOf[acct], big.NewInt(1000))
		want := new(big.Int).Add(content, new(big.Int).Add(lp, yield))

		now := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+acct)
		got := new(big.Int).Sub(now, preClaim[acct])
		t.Logf("%s: content=%s lp=%s yield=%s -> want %s, gained %s",
			acct, content, lp, yield, want, got)
		if got.Cmp(want) != 0 {
			t.Fatalf("%s gained %s from its three claims, want content+lp+yield = %s",
				acct, got, want)
		}
	}

	// and no distributor may pay out more than it was funded
	for _, c := range []struct {
		funded string
		a, b   *big.Int
		name   string
	}{
		{c3Funded, payout(c3Funded, share("content", holderA), c3TotalN), payout(c3Funded, share("content", holderB), c3TotalN), "content"},
		{c5Funded, payout(c5Funded, share("lp", holderA), c5TotalN), payout(c5Funded, share("lp", holderB), c5TotalN), "lp"},
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

	// ---------------- PHASE 7b: a malicious STAKED HOLDER ----------------
	//
	// The sweep below is a pure outsider: no stake, no share, no standing. That is
	// the easy case — most of those calls die on the first authority check. The more
	// realistic insider is a holder who is legitimately IN the system: real stake in
	// C1, a real share in C3/C5, and a claim they have already collected. Their
	// attacks reach much deeper into the logic before anything says no.
	//
	// holderB has exactly that position at this point: 400 staked, shares in C3 and
	// C5, and all three claims already taken.
	holderBefore := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+holderB)
	holderStake := stateOf(c1ID, "stake|hive:"+holderB)
	if holderBefore.Sign() == 0 || holderStake == "" || holderStake == "0" {
		t.Fatalf("holder-attacker has no standing (balance %s, stake %q) — this phase would prove nothing",
			holderBefore, holderStake)
	}
	t.Logf("holder-attacker hive:%s holds %s tokens and %s stake", holderB, holderBefore, holderStake)

	// The double-claim rows carry the holder's REAL proof, so the double-claim guard
	// is what refuses them. Without it they would be refused for having no proof —
	// which is a different guard, and would leave the one under test unexercised.
	holderAttacks := []struct{ id, action, payload, what string }{
		// they DID have a valid share and already claimed it — the second must fail
		{c3ID, "claim", claimPayload("content", holderB), "double-claim content (has a real share)"},
		{c3ID, "claim", claimPayload("lp", holderB), "double-claim LP (has a real share)"},
		{c1ID, "claimYield", `{"epoch":"0"}`, "double-claim yield (is really staked)"},
		// canonicalisation: "00" must not alias epoch 0 into a second payout
		{c3ID, "claim", claimPayloadEp("content", holderB, "00"),
			"re-claim content via a non-canonical epoch alias"},
		{c1ID, "claimYield", `{"epoch":"00"}`, "re-claim yield via a non-canonical epoch alias"},
		// an epoch they have no share in
		{c3ID, "claim", `{"channel":"content","epoch":"1"}`, "claim an epoch with no share"},
		// funding is write-once, and a future epoch owes nothing
		{c3ID, "pullFunding", `{"channel":"content","epoch":"0"}`, "pull epoch-0 funding a second time"},
		{c1ID, "pullFunding", `{"epoch":"5"}`, "pull funding for a future/unfunded epoch"},
		// staking: they have stake, so these get past the "no position" checks
		{c1ID, "unstake", `{"amount":"999999"}`, "unstake far more than they staked"},
		{c1ID, "claimUnstaked", `{}`, "withdraw an unstake before cooldown"},
		{c1ID, "stakeFor", fmt.Sprintf(`{"account":"hive:%s","amount":"100"}`, holderB), "stakeFor while not allowlisted"},
		// roles they do not hold
		{c3ID, "finalizeEpoch", `{"channel":"content","epoch":"1"}`, "finalize as a mere shareholder"},
		{c3ID, "sweepUnallocated", `{"channel":"content","nonce":"2"}`, "sweep the content pot (valid nonce)"},
		{c1ID, "sweepEmptyEpoch", `{"epoch":"0"}`, "sweep a yield epoch that has stakers (refused on that, before auth)"},
	}
	t.Logf("holder sweep: %d attacks from a legitimately staked holder", len(holderAttacks))
	hsent := 0
	for _, a := range holderAttacks {
		if _, err := d.CallContract(ctx, 2, a.id, a.action, a.payload); err != nil {
			t.Logf("  rejected at broadcast: %s (%v)", a.what, err)
			continue
		}
		hsent++
		time.Sleep(7 * time.Second)
	}
	t.Logf("  %d/%d holder attacks reached the chain; all must have aborted", hsent, len(holderAttacks))
	time.Sleep(30 * time.Second)

	if got := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+holderB); got.Cmp(holderBefore) != 0 {
		t.Fatalf("HOLDER GAINED TOKENS: %s -> %s", holderBefore, got)
	}
	if got := stateOf(c1ID, "stake|hive:"+holderB); got != holderStake {
		t.Fatalf("HOLDER CHANGED THEIR STAKE: %q -> %q", holderStake, got)
	}
	t.Logf("holder sweep clean: balance and stake both unmoved")

	// ---------------- PHASE 8: adversarial sweep ----------------
	//
	// Every privileged action on EVERY contract, attempted by an outsider who owns
	// nothing and holds no role. The threat model is deliberate: the deployer is
	// trusted (they funded the token), so what must hold is that an outsider — or a
	// token holder who is not the owner — cannot move anything.
	//
	// Node 3 is funded (see PHASE 1) precisely so each of these reaches the contract
	// and is rejected on AUTHORISATION rather than dying of RC exhaustion.
	before := map[string]string{
		"token.owner":     stateOf(tokenID, "owner"),
		"token.supply":    stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "supply").String(),
		"c1.total_staked": stateOf(c1ID, "total_staked"),
		"c2.lastEpoch":    stateOf(c2ID, "cfg_lastEpoch_v"),
		"c3.totalShares":  stateOf(c3ID, "totalShares|content|0"),
		"c3.funded":       stateOf(c3ID, "funded|content|0"),
		"c3.status":       stateOf(c3ID, "status|content|0"),
		"c5.totalShares":  stateOf(c3ID, "totalShares|lp|0"),
		"c5.funded":       stateOf(c3ID, "funded|lp|0"),
		"c7.funded":       stateOf(c1ID, "y_funded|0"),
		"c6.airdropTotal": stateOf(c1ID, "airdrop_total"),
	}
	readBack := func(k string) string {
		switch k {
		case "token.owner":
			return stateOf(tokenID, "owner")
		case "token.supply":
			return stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "supply").String()
		case "c1.total_staked":
			return stateOf(c1ID, "total_staked")
		case "c2.lastEpoch":
			return stateOf(c2ID, "cfg_lastEpoch_v")
		case "c3.totalShares":
			return stateOf(c3ID, "totalShares|content|0")
		case "c3.funded":
			return stateOf(c3ID, "funded|content|0")
		case "c3.status":
			return stateOf(c3ID, "status|content|0")
		case "c5.totalShares":
			return stateOf(c3ID, "totalShares|lp|0")
		case "c5.funded":
			return stateOf(c3ID, "funded|lp|0")
		case "c7.funded":
			return stateOf(c1ID, "y_funded|0")
		case "c6.airdropTotal":
			return stateOf(c1ID, "airdrop_total")
		}
		return ""
	}

	// A "before" snapshot full of empty strings would compare equal to anything and
	// turn the whole adversarial phase into a vacuous pass. Every one of these keys
	// must already hold a value.
	for k, v := range before {
		if v == "" || v == "0" {
			t.Fatalf("pre-attack snapshot %s is %q — the assertion below would be vacuous", k, v)
		}
	}

	distInitPayload := fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, outsider, outsider, outsider)

	attacks := []struct{ id, action, payload, what string }{
		// --- C0 token: the framework handed ownership to C2, nobody else may act ---
		{tokenID, "mint", `{"amount":"999999"}`, "C0 mint"},
		{tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"hive:%s"}`, outsider), "C0 seize ownership"},
		{tokenID, "pause", `{}`, "C0 pause (griefing: would freeze all payouts)"},
		{tokenID, "unpause", `{}`, "C0 unpause"},
		{tokenID, "transferFrom", fmt.Sprintf(
			`{"from":"hive:%s","to":"hive:%s","amount":"1000"}`, holderA, outsider),
			"C0 transferFrom without allowance (steal a holder's balance)"},

		// --- C1: staking custody, yield, and the airdrop are all one contract now ---
		//
		// ONE re-init row covers all three roles. The payload below would, if it landed,
		// reset the stakeFor allowlist, raise the airdrop cap to near-infinity AND
		// repoint the sweep treasury at the attacker — a single "already initialized"
		// guard is what stops all of it, so testing it three times proved nothing extra.
		{c1ID, "init", fmt.Sprintf(
			`{"token":"%s","kind":"0","cooldown":"1","epochLen":"1","allow":"hive:%s",`+
				`"maxAirdrop":"99999999","treasury":"hive:%s",`+
				`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
			tokenID, outsider, outsider, outsider),
			"C1 re-init (would reset the allowlist, raise the airdrop cap and seize the treasury)"},
		{c1ID, "stakeFor", fmt.Sprintf(`{"account":"hive:%s","amount":"100"}`, outsider), "C1 stakeFor while not allowlisted"},
		{c1ID, "unstake", `{"amount":"1000"}`, "C1 unstake stake they never made"},
		{c1ID, "claimUnstaked", `{}`, "C1 claimUnstaked with nothing queued"},
		{c1ID, "airdropBatch", fmt.Sprintf(`{"batchId":"9","entries":"hive:%s:1000"}`, outsider), "C1 airdrop to self"},
		{c1ID, "airdropBatch", fmt.Sprintf(`{"batchId":"1","entries":"hive:%s:1000"}`, outsider), "C1 replay the already-applied batch 1"},
		{c1ID, "sweepEmptyEpoch", `{"epoch":"0"}`, "C1 sweep a yield epoch with stakers (aborts on the stakers check, not on auth)"},
		{c1ID, "claimYield", `{"epoch":"0"}`, "C1 claim yield with no stake"},

		// --- C2 emission: the mint authority ---
		{c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","epochLen":"1","baseAnnual":"9999999","blocksPerYear":"1","dustBucket":"x","timelock":"1","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1","buckets":"x:hive:%s:10000"}`, tokenID, outsider, treasury, outsider), "C2 re-init (would redirect every bucket)"},
		{c2ID, "claimBucket", `{"epoch":"0"}`, "C2 claimBucket impersonating a bucket target"},
		{c2ID, "claimBucket", `{"epoch":"00"}`, "C2 claimBucket with a non-canonical epoch alias"},
		{c2ID, "queueTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"hive:%s"}`, outsider), "C2 queue a token takeover"},
		// NB: this aborts on the "not queued" guard, BEFORE the authority check, so it
		// proves only that an unqueued op cannot execute. PHASE 9 covers the real
		// authority and timelock path with an op that genuinely exists.
		{c2ID, "executeTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"hive:%s"}`, outsider), "C2 execute a never-queued takeover (aborts early — see PHASE 9)"},
		{c2ID, "cancelTokenOp", `{"op":"pause","nonce":"1"}`, "C2 cancel a token op"},

		// --- C3 content distributor ---
		{c3ID, "init", distInitPayload, "C3 re-init (would repoint reporter+treasury)"},
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"content","epoch":"1","page":"0","entries":"hive:%s:999999"}`, outsider), "C3 push fraudulent shares"},
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"content","epoch":"0","page":"00","entries":"hive:%s:999999"}`, outsider), "C3 re-apply page 0 via a non-canonical alias"},
		{c3ID, "finalizeEpoch", `{"channel":"content","epoch":"1"}`, "C3 finalize an epoch they do not control"},
		{c3ID, "cancelEpoch", `{"channel":"content","epoch":"0"}`, "C3 veto a legitimate epoch (griefing)"},
		{c3ID, "sweepUnallocated", `{"channel":"content","nonce":"1"}`, "C3 sweep the pot (valid nonce, so this reaches the guardian gate)"},
		{c3ID, "claim", `{"channel":"content","epoch":"0"}`, "C3 claim with no share"},

		// --- the LP channel: same contract, same surface, DIFFERENT channel ---
		//
		// This is the half of channel-scoping that matters adversarially: holding a
		// reporter authority on `content` must buy nothing on `lp`. The rows are
		// duplicated per channel deliberately, unlike the re-init above, because each
		// one exercises a distinct per-channel authority record rather than a single
		// contract-wide guard.
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"lp","epoch":"1","page":"0","entries":"hive:%s:999999"}`, outsider), "lp push fraudulent shares"},
		{c3ID, "finalizeEpoch", `{"channel":"lp","epoch":"1"}`, "lp finalize"},
		{c3ID, "cancelEpoch", `{"channel":"lp","epoch":"0"}`, "lp veto a legitimate epoch"},
		{c3ID, "sweepUnallocated", `{"channel":"lp","nonce":"1"}`, "lp sweep the pot (valid nonce, so this reaches the guardian gate)"},
		{c3ID, "claim", `{"channel":"lp","epoch":"0"}`, "lp claim with no share"},
		{c3ID, "addChannel", fmt.Sprintf(
			`{"channel":"pirate","bucket":"content","window":"1","reporterMode":"0",`+
				`"reporterAuth":"hive:%s","reporterThreshold":"1","role":"content"}`, outsider),
			"C3 register a channel of their own (would siphon the content bucket)"},
	}

	t.Logf("adversarial sweep: %d attacks from outsider hive:%s", len(attacks), outsider)
	sent := 0
	for _, a := range attacks {
		if _, err := d.CallContract(ctx, 3, a.id, a.action, a.payload); err != nil {
			t.Logf("  rejected at broadcast: %s (%v)", a.what, err)
			continue
		}
		sent++
		time.Sleep(7 * time.Second)
	}
	t.Logf("  %d/%d attacks reached the chain; all must have aborted", sent, len(attacks))

	time.Sleep(45 * time.Second) // let anything that DID land settle before asserting
	for k, want := range before {
		if got := readBack(k); got != want {
			t.Fatalf("OUTSIDER CHANGED STATE %s: %q -> %q", k, want, got)
		}
	}

	// Nothing may have been created for the attacker anywhere.
	//
	// The loop is over CHANNELS, not contract ids: content and lp share one distributor
	// now, so every key needs the channel component or it addresses nothing.
	for _, ch := range []string{"content", "lp"} {
		if v := stateOf(c3ID, "claimed|"+ch+"|0|hive:"+outsider); v != "" && v != "0" {
			t.Fatalf("%s channel granted the outsider a share: %s", ch, v)
		}
		if v := stateOf(c3ID, "totalShares|"+ch+"|1"); v != "" && v != "0" {
			t.Fatalf("%s channel accepted epoch-1 shares from the outsider: %s", ch, v)
		}
		if v := stateOf(c3ID, "status|"+ch+"|1"); v != "" {
			t.Fatalf("%s channel epoch 1 was finalized/cancelled by the outsider: %s", ch, v)
		}
	}
	// The outsider also tried to register a channel of their own. A channel is what
	// authorises a reporter and names a funding bucket, so one they control would let
	// them mint shares legitimately — check no trace of it exists.
	if v := stateOf(c3ID, "ch_bucket|pirate"); v != "" {
		t.Fatalf("the outsider registered their own channel, bucket=%s", v)
	}
	// C1 keeps no share book, so its equivalent is a yield payout against an epoch the
	// outsider never staked in.
	if v := stateOf(c1ID, "y_claimed|0|hive:"+outsider); v != "" && v != "0" {
		t.Fatalf("C1 paid yield to the outsider: %s", v)
	}
	if v := stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+outsider); v.Sign() != 0 {
		t.Fatalf("outsider ended up holding %s tokens", v)
	}
	if v := stateOf(c1ID, "stake|hive:"+outsider); v != "" && v != "0" {
		t.Fatalf("outsider acquired stake: %s", v)
	}
	// The queued-op table must be empty. The key is tl|<op>:<nonce>[:<newOwner>]
	// (opKeyOf), NOT tl|<op>|<nonce> — a wrong key here would read empty and pass
	// vacuously, which is worse than no assertion at all.
	tlKey := fmt.Sprintf("tl|changeOwner:1:hive:%s", outsider)
	if v := stateOf(c2ID, tlKey); v != "" {
		t.Fatalf("outsider queued a token op (%s = %s)", tlKey, v)
	}
	t.Logf("adversarial sweep clean: no state moved, outsider holds nothing")

	// ---------------- PHASE 9: the guardian token-op passthrough ----------
	//
	// Until now this path had NEVER been exercised successfully anywhere. Both devnet
	// suites only ever attempted executeTokenOp for an op that was never queued, so
	// they aborted on the "not queued" guard — an earlier check — and proved nothing
	// about the timelock or the authority. C2 holds the token here, so the passthrough
	// is the only route to pause/changeOwner, and it is the framework's single largest
	// retained power. Drive it end to end.
	//
	// The timelock is 60 blocks (~180s), and that is a TEST REQUIREMENT rather than a
	// policy preference. It has to satisfy BOTH ends of a narrow target:
	//
	//   too short and "execute early" is not early. Each devnet call sleeps 9s and
	//     broadcast adds more, so against a 5-block (15s) timelock the early call
	//     landed 19s after the queue, already matured — the guardian legitimately
	//     executed it and the assertion reported a bypass that never happened.
	//   too long a WAIT and the op expires. executeTokenOp accepts only
	//     [ready, ready+timelock] (c2 HIGH-2: a stale approval must not fire years
	//     later), so a 40-block timelock gave a 120s window that ~222s of test
	//     sequence overshot.
	//
	// 60 blocks puts maturity at ~180s and expiry at ~360s, so a ~210-265s execute
	// sits mid-window with margin at both ends.
	// NB: every read here goes through waitKey/waitValue. A bare read straight after
	// callN's 9s sleep is not enough for the state to be visible, and reading too
	// early is the single most common way to write a devnet assertion that reports a
	// contract bug that does not exist.
	const pauseOp = `{"op":"pause","nonce":"9"}`
	callN(5, c2ID, "queueTokenOp", pauseOp, "guardian queues pause")
	t.Logf("queued: tl|pause:9 = %s", waitKey(c2ID, "tl|pause:9", "queued pause op"))

	// EARLY execution must be refused. This is the assertion the old negative could
	// not make: the op genuinely exists, so reaching the timelock check is guaranteed.
	callN(5, c2ID, "executeTokenOp", pauseOp, "guardian executes BEFORE the timelock")

	// and a non-guardian must not be able to execute a legitimately queued op
	if _, err := d.CallContract(ctx, 3, c2ID, "executeTokenOp", pauseOp); err == nil {
		time.Sleep(9 * time.Second)
	}

	// Give both of those ample time to land before concluding they did nothing —
	// asserting an absence that merely outran block production proves nothing.
	notPausedUntil := time.Now().Add(45 * time.Second)
	for time.Now().Before(notPausedUntil) {
		if v := stateOf(tokenID, "paused"); v == "1" {
			t.Fatal("TIMELOCK BYPASSED: the token paused before the delay elapsed, " +
				"or an outsider executed the guardian's queued op")
		}
		time.Sleep(6 * time.Second)
	}
	t.Logf("early execute and outsider execute both correctly left the token unpaused")

	// Wait past maturity (~180s at timelock 60) while staying well inside expiry
	// (~360s). The window is [ready, ready+timelock] and ONE parameter sets both ends.
	time.Sleep(200 * time.Second)
	callN(5, c2ID, "executeTokenOp", pauseOp, "guardian executes AFTER the timelock")
	waitValue(tokenID, "paused", "1", "token paused via the C2 passthrough")
	t.Logf("PASSTHROUGH OK: guardian paused the token through C2 after the timelock")

	// Restore, proving the round trip both ways rather than leaving the token wedged.
	const unpauseOp = `{"op":"unpause","nonce":"10"}`
	callN(5, c2ID, "queueTokenOp", unpauseOp, "guardian queues unpause")
	waitKey(c2ID, "tl|unpause:10", "queued unpause op")
	time.Sleep(200 * time.Second)
	callN(5, c2ID, "executeTokenOp", unpauseOp, "guardian executes unpause")
	waitValue(tokenID, "paused", "0", "token unpaused via the C2 passthrough")
	t.Logf("PASSTHROUGH OK: and unpaused again — the retained power works in both directions")

	t.Logf("FULL SYSTEM DEVNET PASSED — the token + all three contracts + reporter, one emission split 3 ways")
	t.Logf("hive fixture calls: %v", fixture.hits)
}
