package devnet

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestDevnetMagiRogueReporter attacks the framework's ONE trusted role.
//
// Every other adversarial suite attacks parties with no authority (an outsider) or
// no role (a staked holder). The reporter is different: it is *supposed* to submit
// share lists, so nothing stops it submitting a WRONG one. Its containment is
// three-fold, and none of it had ever been exercised on a live chain:
//
//  1. it can only submit numbers — it cannot mint, move funds, or change roles;
//
//  2. a guardian can cancel a fraudulent report during the challenge window, and
//     the funding rolls forward rather than being stolen or stranded;
//
//  3. in Attest mode a single rogue cannot reach threshold, cannot equivocate, and
//     cannot stop an honest majority from committing a different payload.
//
//     PHASE A  Single-mode reporter publishes a fraudulent report; guardian cancels
//     it in the window; the rogue claims nothing; funding is recovered
//     PHASE B  Attest mode (2-of-3): rogue attests alone, burns its vote, and the
//     two honest reporters still commit THEIR payload
//     PHASE C  everything the reporter role must not be able to do at all
//
// Run: go test -v -run TestDevnetMagiRogueReporter -timeout 60m ./tests/devnet/
func TestDevnetMagiRogueReporter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)
	requireDiskSpace(t)

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
	rogue := w(1)    // the reporter that turns malicious. Also the deployer/owner.
	honestB := w(2)  // second Attest reporter
	honestC := w(3)  // third Attest reporter
	treasury := w(4) // pinned sweep destination
	guardian := w(5) // may cancel a bad report — disjoint from every reporter

	// ---------------- PHASE 1: deploy + fund ----------------
	deploy := func(name, wasm string) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name, DeployerNode: 1})
			if err == nil {
				t.Logf("deployed %-16s = %s", name, id)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = err
			t.Logf("deploy %s attempt %d failed: %v", name, attempt, err)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}
	tokenID := deploy("rr-token", magiTokenWasm)
	c2ID := deploy("rr-c2", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c3ID := deploy("rr-c3-single", magiWasm(t, "c3-distributor/artifacts/main.wasm"))

	// Every reporter and the guardian must be able to transact freely. RC is
	// `ledger HBD + 10_000 free`, and the free tier alone is ~7 transactions — a
	// rogue that runs out of RC would look "contained" when it was merely broke.
	// Deposit in repeated descending rounds so each account moves as much L1 balance
	// into the L2 ledger as it has.
	for _, n := range []int{1, 2, 3, 5} {
		moved := 0
		for round := 0; round < 4; round++ {
			progressed := false
			for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
				if _, e := d.Deposit(ctx, n, amt, "hbd"); e == nil {
					t.Logf("node %d deposited %s HBD (round %d)", n, amt, round+1)
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
			t.Fatalf("node %d could not deposit — it would run on the free tier alone", n)
		}
	}
	// Deposits credit the L2 ledger asynchronously; a fixed sleep is not enough.
	// Poll, then confirm the rogue is funded past the 10k free tier — a rogue that
	// runs out of RC would look "contained" when it was merely broke.
	depDeadline := time.Now().Add(4 * time.Minute)
	for {
		ok := true
		for _, n := range []string{rogue, honestB, honestC, guardian} {
			b, e := d.GetAccountBalance(ctx, 1, "hive:"+n)
			if e != nil || b == nil || b.Hbd <= 0 {
				ok = false
				break
			}
		}
		if ok {
			break
		}
		if time.Now().After(depDeadline) {
			t.Fatal("deposits never credited the L2 ledger — actors would run on the free tier alone")
		}
		time.Sleep(6 * time.Second)
	}
	for _, n := range []string{rogue, honestB, honestC, guardian} {
		b, _ := d.GetAccountBalance(ctx, 1, "hive:"+n)
		t.Logf("ledger balance hive:%-14s hbd=%d -> RC ~%d", n, b.Hbd, b.Hbd+10000)
	}

	// ---------------- helpers ----------------
	callN := func(node int, id, action, payload, what string) {
		if _, err := d.CallContract(ctx, node, id, action, payload); err != nil {
			t.Fatalf("%s: broadcast failed: %v", what, err)
		}
		t.Logf("sent: %s", what)
		time.Sleep(9 * time.Second)
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
	bal := func(acct string) *big.Int {
		return stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|hive:"+acct)
	}

	const epochLen, window = 10, 30

	// ---------------- PHASE 2: init ----------------
	callN(1, tokenID, "init", `{"name":"Rogue","symbol":"ROG","decimals":0,"maxSupply":"100000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")
	// C2 no longer mints — it PULLS each epoch's emission from an account that has
	// approved it. Mint the pool and approve C2 BEFORE handing the token over, since
	// only the owner may mint. (C2 no longer needs to own the token at all; the
	// handover below is kept only so the guardian token-op passthrough stays live.)
	callN(1, tokenID, "mint", `{"amount":"1000000"}`, "mint the emission pool")
	callN(1, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2 to draw the pool")
	callN(1, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token -> C2")
	callN(1, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"content","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:5000,lp:contract:%s:5000"}`,
		tokenID, epochLen, guardian, treasury, c3ID, c3ID), "C2 init")
	genesis, _ := strconv.ParseUint(waitKey(c2ID, "cfg_genesis", "C2 genesis"), 10, 64)
	t.Logf("genesis=%d epochLen=%d", genesis, epochLen)

	// ONE distributor carrying TWO channels with DIFFERENT reporter policies — only
	// possible because reporter authority is per-channel. That contrast is this
	// suite's whole point: the same rogue account is trusted unilaterally on one
	// channel and is merely one of three attesters on the other.
	callN(1, c3ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, treasury, guardian), "distributor init")
	waitKey(c3ID, "cfg_funder", "distributor funder")

	// content: SINGLE-mode reporter — one account, trusted to publish unilaterally.
	callN(1, c3ID, "addChannel", fmt.Sprintf(
		`{"channel":"content","bucket":"content","window":"%d","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","role":"content"}`,
		window, rogue), "addChannel content (single reporter)")
	waitKey(c3ID, "ch_bucket|content", "content channel registered")

	// lp: ATTEST mode, 2-of-3 — the configuration that exists precisely so no single
	// reporter machine has to be trusted.
	callN(1, c3ID, "addChannel", fmt.Sprintf(
		`{"channel":"lp","bucket":"lp","window":"%d","reporterMode":"2",`+
			`"reporterAuth":"hive:%s,hive:%s,hive:%s","reporterThreshold":"2","role":"lp"}`,
		window, rogue, honestB, honestC), "addChannel lp (attest 2-of-3)")
	waitKey(c3ID, "ch_bucket|lp", "lp channel registered")

	// ---------------- PHASE 3: fund epoch 0 ----------------
	t.Logf("waiting for epoch 0 to close...")
	time.Sleep(time.Duration(epochLen+8) * 3 * time.Second)
	callN(1, c2ID, "distributeEpoch", `{}`, "keeper poke")
	callN(1, c3ID, "pullFunding", `{"channel":"content","epoch":"0"}`, "C3 pull")
	callN(1, c3ID, "pullFunding", `{"channel":"lp","epoch":"0"}`, "C5 pull")
	waitValue(c3ID, "funded|content|0", "5000", "C3 funded")
	waitValue(c3ID, "funded|lp|0", "5000", "C5 funded")

	// ================= PHASE A: rogue single reporter =================
	//
	// Nothing prevents the reporter publishing a lie — that is the point of the
	// role. What must hold is that the lie is *containable*.
	t.Logf("PHASE A: rogue reporter publishes a fraudulent report on C3")
	callN(1, c3ID, "submitShares", fmt.Sprintf(
		`{"channel":"content","epoch":"0","page":"0","entries":"hive:%s:1000000"}`, rogue), "rogue submits 100%% to itself")
	callN(1, c3ID, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "rogue finalizes its own fraud")
	waitValue(c3ID, "totalShares|content|0", "1000000", "C3 totalShares (fraudulent)")
	if st := waitKey(c3ID, "status|content|0", "C3 status"); st != "finalized" {
		t.Fatalf("expected the fraudulent report to be accepted (status=%q)", st)
	}
	t.Logf("  the fraud WAS accepted on-chain — containment is the guardian, not submission")

	rogueBefore := bal(rogue)

	// the guardian cancels inside the challenge window
	callN(5, c3ID, "cancelEpoch", `{"channel":"content","epoch":"0"}`, "guardian cancels the fraudulent epoch")
	waitValue(c3ID, "status|content|0", "cancelled", "C3 status after veto")

	// funding must roll forward, not vanish and not be payable
	waitValue(c3ID, "funded|content|0", "0", "C3 funded after cancel")
	waitValue(c3ID, "unallocated", "5000", "C3 unallocated (rolled forward)")

	// and the rogue must get nothing, now or after the window
	time.Sleep(time.Duration(window+5) * 3 * time.Second)
	callN(1, c3ID, "claim", `{"channel":"content","epoch":"0"}`, "rogue tries to claim its fraudulent share")
	time.Sleep(20 * time.Second)
	if got := bal(rogue); got.Cmp(rogueBefore) != 0 {
		t.Fatalf("ROGUE REPORTER GOT PAID: %s -> %s", rogueBefore, got)
	}
	t.Logf("  rogue claimed nothing (balance still %s)", rogueBefore)

	// the rolled-forward funding is recoverable by the guardian to the PINNED treasury
	callN(5, c3ID, "sweepUnallocated", `{"channel":"content","nonce":"1"}`, "guardian sweeps the rolled-forward pool")
	waitValue(c3ID, "unallocated", "0", "C3 unallocated after sweep")
	if got := bal(treasury); got.String() != "5000" {
		t.Fatalf("treasury should hold the recovered 5000, has %s", got)
	}
	t.Logf("  funding recovered to the treasury: 5000")

	// ================= PHASE B: rogue in Attest 2-of-3 =================
	t.Logf("PHASE B: rogue attests alone on C5 (2-of-3)")
	fraud := fmt.Sprintf(`{"channel":"lp","epoch":"0","page":"0","entries":"hive:%s:999999"}`, rogue)
	honest := fmt.Sprintf(`{"channel":"lp","epoch":"0","page":"0","entries":"hive:%s:600,hive:%s:400"}`, honestB, honestC)

	callN(1, c3ID, "submitShares", fraud, "rogue attests its fraudulent page")
	time.Sleep(15 * time.Second)
	if v := stateOf(c3ID, "totalShares|lp|0"); v != "" && v != "0" {
		t.Fatalf("a single attestation applied shares (%s) — threshold 2 was not enforced", v)
	}
	t.Logf("  one attestation of three: nothing applied")

	// the rogue has now SPENT its vote for this action and cannot also back the
	// honest payload — one vote per authority per action, not per payload
	callN(1, c3ID, "submitShares", honest, "rogue tries to ALSO attest the honest page (equivocation)")

	// two honest reporters agree on a DIFFERENT payload and reach threshold
	callN(2, c3ID, "submitShares", honest, "honest B attests")
	time.Sleep(12 * time.Second)
	if v := stateOf(c3ID, "totalShares|lp|0"); v != "" && v != "0" {
		t.Fatalf("two attestations of different payloads applied shares (%s)", v)
	}
	callN(3, c3ID, "submitShares", honest, "honest C attests -> threshold")
	waitValue(c3ID, "totalShares|lp|0", "1000", "C5 totalShares (honest payload)")

	if v := stateOf(c3ID, "share|lp|0|hive:"+rogue); v != "" && v != "0" {
		t.Fatalf("the rogue's fraudulent payload took effect: share=%s", v)
	}
	t.Logf("  honest majority committed THEIR payload; the rogue's never applied")

	// finalize also needs the threshold
	callN(2, c3ID, "finalizeEpoch", `{"channel":"lp","epoch":"0"}`, "honest B finalizes (1 of 2)")
	time.Sleep(12 * time.Second)
	if st := stateOf(c3ID, "status|lp|0"); st != "" {
		t.Fatalf("a single finalize attestation closed the epoch (status=%q)", st)
	}
	callN(3, c3ID, "finalizeEpoch", `{"channel":"lp","epoch":"0"}`, "honest C finalizes -> threshold")
	waitValue(c3ID, "status|lp|0", "finalized", "C5 status")

	// ================= PHASE C: what the role cannot do =================
	t.Logf("PHASE C: actions the reporter role must not reach")
	before := map[string]string{
		"token.owner":    stateOf(tokenID, "owner"),
		"c5.totalShares": stateOf(c3ID, "totalShares|lp|0"),
		"c5.funded":      stateOf(c3ID, "funded|lp|0"),
		"c3.unallocated": stateOf(c3ID, "unallocated"),
	}
	for k, v := range before {
		if v == "" {
			t.Fatalf("baseline %s is empty — the assertions below would be vacuous", k)
		}
	}
	for _, a := range []struct{ id, action, payload, what string }{
		{tokenID, "mint", `{"amount":"999999"}`, "reporter mints"},
		{tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"hive:%s"}`, rogue), "reporter seizes the token"},
		{c2ID, "claimBucket", `{"epoch":"0"}`, "reporter impersonates a bucket"},
		{c3ID, "cancelEpoch", `{"channel":"lp","epoch":"0"}`, "reporter vetoes (guardian-only)"},
		{c3ID, "sweepUnallocated", `{"channel":"lp","nonce":"7"}`, "reporter sweeps (guardian-only)"},
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"content","epoch":"0","page":"1","entries":"hive:%s:5"}`, rogue), "reporter reopens a cancelled epoch"},
		{c3ID, "submitShares", fmt.Sprintf(`{"channel":"lp","epoch":"0","page":"1","entries":"hive:%s:5"}`, rogue), "reporter adds shares after finalize"},
	} {
		if _, err := d.CallContract(ctx, 1, a.id, a.action, a.payload); err != nil {
			t.Logf("  rejected at broadcast: %s (%v)", a.what, err)
			continue
		}
		t.Logf("  sent (must abort on-chain): %s", a.what)
		time.Sleep(8 * time.Second)
	}
	time.Sleep(40 * time.Second)
	for k, want := range before {
		var got string
		switch k {
		case "token.owner":
			got = stateOf(tokenID, "owner")
		case "c5.totalShares":
			got = stateOf(c3ID, "totalShares|lp|0")
		case "c5.funded":
			got = stateOf(c3ID, "funded|lp|0")
		case "c3.unallocated":
			got = stateOf(c3ID, "unallocated")
		}
		if got != want {
			t.Fatalf("REPORTER CHANGED STATE %s: %q -> %q", k, want, got)
		}
	}
	if o := stateOf(tokenID, "owner"); o != "contract:"+c2ID {
		t.Fatalf("token owner drifted to %q", o)
	}

	// honest claims still work after all of that
	time.Sleep(time.Duration(window+5) * 3 * time.Second)
	callN(2, c3ID, "claim", `{"channel":"lp","epoch":"0"}`, "honest B claims")
	callN(3, c3ID, "claim", `{"channel":"lp","epoch":"0"}`, "honest C claims")
	// Wait for EVERY claimant's marker, not just the first. Reading a balance a few
	// seconds after broadcast is indistinguishable from the claim being rejected —
	// which is exactly what made this test report "honest C should hold 2000, has 0".
	for _, acct := range []string{honestB, honestC} {
		if !waitStateKeyPresent(t, d, ctx, 1, c3ID, "claimed|lp|0|hive:"+acct, 3*time.Minute) {
			t.Fatalf("%s could not claim", acct)
		}
	}
	// 5000 * 600/1000 = 3000 and 5000 * 400/1000 = 2000
	if got := bal(honestB); got.String() != "3000" {
		t.Fatalf("honest B should hold 3000, has %s", got)
	}
	if got := bal(honestC); got.String() != "2000" {
		t.Fatalf("honest C should hold 2000, has %s", got)
	}
	if got := bal(rogue); got.Cmp(rogueBefore) != 0 {
		t.Fatalf("rogue ended up with %s (started %s)", bal(rogue), rogueBefore)
	}

	t.Logf("ROGUE REPORTER DEVNET PASSED — fraud contained, funding recovered, attest quorum held")
}
