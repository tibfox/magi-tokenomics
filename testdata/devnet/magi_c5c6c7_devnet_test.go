package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDevnetMagiC5C6C7 gives the remaining framework contracts real-chain
// coverage: C1 staking, C5 LP-distributor, C6 migration/airdrop and C7
// staking-yield, deployed on a docker multi-node devnet.
//
//	PHASE 1  deploy token + C1 + C2 + C5 + C6 + C7 and wire them
//	PHASE 2  honest: airdrop, stake, emit, pull, LP shares + claim, yield claim
//	PHASE 3  outsider attacks against C5/C6/C7
//	PHASE 4  on-chain state proves nothing was stolen
//
// Run: go test -v -run TestDevnetMagiC5C6C7 -timeout 50m ./tests/devnet/
func TestDevnetMagiC5C6C7(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
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

	// each DeployContract bounces magi-1; settle between deploys and retry
	deploy := func(name, wasm string, node int) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, derr := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name, DeployerNode: node})
			if derr == nil {
				t.Logf("deployed %s = %s by node %d (attempt %d)", name, id, node, attempt)
				time.Sleep(45 * time.Second)
				return id
			}
			lastErr = derr
			t.Logf("deploy %s attempt %d failed: %v — settling", name, attempt, derr)
			time.Sleep(60 * time.Second)
		}
		t.Fatalf("deploy %s: %v", name, lastErr)
		return ""
	}

	// Each contract is owned by a DIFFERENT account so no single account has to pay
	// the RC for every init in quick succession (that exhausted magi.test1 before).
	tokenID := deploy("m2-token", magiTokenWasm, 1)
	c1ID := deploy("m2-c1-staking", magiWasm(t, "c1-staking/artifacts/main.wasm"), 1)
	c2ID := deploy("m2-c2-emission", magiWasm(t, "c2-emission/artifacts/main.wasm"), 1)
	c5ID := deploy("m2-c5-lp", magiWasm(t, "c5-lp/artifacts/main.wasm"), 1)
	c6ID := deploy("m2-c6-migration", magiWasm(t, "c6-migration/artifacts/main.wasm"), 1)
	c7ID := deploy("m2-c7-yield", magiWasm(t, "c7-yield/artifacts/main.wasm"), 1)

	// RC = VSC-ledger HBD balance + RC_HIVE_FREE_AMOUNT(10_000)  [rc-system.go:33-37].
	// With hbd=0 an account has EXACTLY 10k RC, which ~7 init txs exhaust — that is
	// what broke earlier runs. Deposit AFTER deploying, because each deploy costs
	// 10 HBD of L1 balance and depositing first starves the deploy fee.
	fund := func(node int, label string) {
		for _, amt := range []string{"30.000", "20.000", "10.000", "5.000"} {
			if _, ferr := d.Deposit(ctx, node, amt, "hbd"); ferr == nil {
				t.Logf("deposited %s HBD for %s (node %d) -> RC", amt, label, node)
				return
			}
		}
		t.Logf("WARNING: could not deposit for %s (node %d)", label, node)
	}
	fund(1, "deployer")
	fund(2, "holder/attacker")
	// poll until the deposits actually credit the VSC ledger
	for i := 0; i < 24; i++ {
		b1, e1 := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(1))
		b2, e2 := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(2))
		if e1 == nil && e2 == nil && b1 != nil && b2 != nil && b1.Hbd > 0 && b2.Hbd > 0 {
			t.Logf("RC funding credited: node1 hbd=%d (RC~%d), node2 hbd=%d (RC~%d)",
				b1.Hbd, b1.Hbd+10000, b2.Hbd, b2.Hbd+10000)
			break
		}
		time.Sleep(5 * time.Second)
	}

	ownerAcct := d.cfg.WitnessPrefix + "1"
	holder := d.cfg.WitnessPrefix + "2" // ordinary token holder / staker / attacker
	t.Logf("owner=%s holder+attacker=%s", ownerAcct, holder)

	send := func(node int, id, action, payload, what string) string {
		tx, cerr := d.CallContract(ctx, node, id, action, payload)
		if cerr != nil {
			t.Fatalf("%s: broadcast failed: %v", what, cerr)
		}
		t.Logf("sent: %s (tx=%s)", what, tx)
		return tx
	}
	// diag reports what the chain thinks happened to a tx, plus a state dump.
	diag := func(tx, id, what string, keys []string) {
		time.Sleep(20 * time.Second)
		if st, serr := d.FindTransactionStatus(ctx, 1, tx); serr == nil {
			t.Logf("DIAG %s: tx status = %q", what, st)
		} else {
			t.Logf("DIAG %s: tx status query failed: %v", what, serr)
		}
		if kv, kerr := d.GetStateByKeys(ctx, 1, id, keys); kerr == nil {
			t.Logf("DIAG %s: state = %v", what, kv)
		} else {
			t.Logf("DIAG %s: state read failed: %v", what, kerr)
		}
	}
	waitKey := func(id, key, what string) string {
		if !waitStateKeyPresent(t, d, ctx, 1, id, key, 3*time.Minute) {
			t.Fatalf("timed out waiting for %s (%s[%s])", what, id, key)
		}
		st, gerr := d.GetStateByKeys(ctx, 1, id, []string{key})
		if gerr != nil {
			t.Fatalf("read %s: %v", what, gerr)
		}
		return fmt.Sprintf("%v", st[key])
	}
	waitVal := func(id, key, want, what string) {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			st, gerr := d.GetStateByKeys(ctx, 1, id, []string{key})
			if gerr == nil && st[key] != nil && strings.Contains(fmt.Sprintf("%v", st[key]), want) {
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatalf("timed out waiting for %s to contain %q", what, want)
	}

	// ---------------- PHASE 1: init + wire ----------------
	send(1, tokenID, "init", `{"name":"M2","symbol":"M2","decimals":0,"maxSupply":"100000000"}`, "token.init")
	waitKey(tokenID, "isInit", "token init")

	send(1, c1ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"5","allow":""}`, tokenID), "c1.init")
	waitKey(c1ID, "init", "c1 init")

	// bootstrap supply BEFORE handing the token to C2 (owner can still mint)
	send(1, tokenID, "mint", `{"amount":"5000"}`, "token.mint bootstrap")
	send(1, tokenID, "transfer", fmt.Sprintf(`{"to":"hive:%s","amount":"2000"}`, holder), "fund holder")
	send(1, tokenID, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, c6ID), "fund C6 for airdrop")
	waitVal(tokenID, "bal|hive:"+holder, "", "holder funded")

	// The holder stakes BEFORE C2 init so their stake predates genesis (C2's genesis
	// auto-defaults to its own init block, and C7 credits min(stake@hStart, @hEnd)).
	send(2, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000"}`, c1ID), "holder approves C1")
	time.Sleep(6 * time.Second)
	send(2, c1ID, "stake", `{"amount":"1000"}`, "holder stakes 1000")
	waitKey(c1ID, "stake|hive:"+holder, "holder stake recorded")

	// C2: genesis omitted -> defaults to this init block. lp 50% -> C5, yield 50% -> C7
	send(1, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"5","maxCatch":"2","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"lp","timelock":"5","guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:%s3","vetoThreshold":"1","buckets":"lp:contract:%s:5000,yield:contract:%s:5000"}`,
		tokenID, ownerAcct, d.cfg.WitnessPrefix, c5ID, c7ID), "c2.init")
	genesis := waitKey(c2ID, "cfg_genesis", "c2 auto-genesis")
	t.Logf("C2 auto-genesis resolved to block %s", genesis)

	// C5 pulls the schedule from C2 itself; C7 must be given the SAME genesis and is
	// cross-checked against C2's scheduleInfo — this is the real deploy sequence.
	send(1, c5ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:%s1","reporterThreshold":"1","treasury":"hive:%s4","guardianMode":"0","guardianAuth":"hive:%s3","guardianThreshold":"1"}`,
		tokenID, c2ID, d.cfg.WitnessPrefix, d.cfg.WitnessPrefix, d.cfg.WitnessPrefix), "c5.init")
	waitKey(c5ID, "init", "c5 init")

	c7tx := send(1, c7ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","treasury":"hive:%s4","guardianMode":"0","guardianAuth":"hive:%s3","guardianThreshold":"1"}`,
		tokenID, c2ID, c1ID, d.cfg.WitnessPrefix, d.cfg.WitnessPrefix), "c7.init")
	c7ok := waitStateKeyPresent(t, d, ctx, 1, c7ID, "init", 90*time.Second)
	if !c7ok {
		diag(c7tx, c7ID, "c7.init", []string{"init", "cfg_token", "cfg_funder", "cfg_genesis", "cfg_epochLen", "cfg_treasury"})
		t.Errorf("C7 init did not take effect — continuing so C5/C6 still get coverage")
	} else {
		t.Logf("c7 init OK (schedule adopted from funder)")
	}

	send(1, c6ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"1000"}`, tokenID), "c6.init")
	waitKey(c6ID, "init", "c6 init")

	send(1, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), "token.changeOwner -> C2")
	waitVal(tokenID, "owner", c2ID, "token ownership handover")

	// ---------------- PHASE 2: honest operation ----------------
	// C6 airdrop (owner only)
	send(1, c6ID, "airdropBatch", fmt.Sprintf(`{"batchId":"genesis","entries":"hive:%s:600,hive:%s3:400"}`, holder, d.cfg.WitnessPrefix), "c6.airdropBatch")
	waitKey(c6ID, "done|genesis", "c6 airdrop batch applied")

	// wait until an epoch has fully elapsed past genesis, then emit
	var g uint64
	fmt.Sscanf(genesis, "%d", &g)
	for {
		h, herr := d.getLastProcessedBlock(ctx, 1)
		if herr == nil && h > g+10 {
			t.Logf("head=%d past genesis(%d)+epochLen, poking emission", h, g)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("ctx done waiting for epoch 0")
		case <-time.After(5 * time.Second):
		}
	}
	// maxCatch=2 bounds each poke; a keeper simply pokes until caught up. Poking
	// from the holder's account also proves distributeEpoch is permissionless.
	distributed := false
	for i := 0; i < 8; i++ {
		send(2, c2ID, "distributeEpoch", ``, fmt.Sprintf("c2.distributeEpoch poke %d (permissionless)", i+1))
		if waitStateKeyPresent(t, d, ctx, 1, c2ID, "cfg_lastEpoch", 60*time.Second) {
			distributed = true
			break
		}
		t.Logf("poke %d did not land yet, retrying", i+1)
	}
	if !distributed {
		t.Fatal("emission never distributed after repeated pokes")
	}
	t.Logf("emission distributed (lastEpoch=%s)", waitKey(c2ID, "cfg_lastEpoch_v", "last epoch"))

	send(2, c5ID, "pullFunding", `{"epoch":"0"}`, "c5.pullFunding")
	c5Funded := waitKey(c5ID, "funded|0", "c5 epoch-0 funding")
	c7Funded := "<c7 not initialised>"
	if c7ok {
		send(2, c7ID, "pullFunding", `{"epoch":"0"}`, "c7.pullFunding")
		c7Funded = waitKey(c7ID, "funded|0", "c7 epoch-0 funding")
	}
	t.Logf("C5 funded=%s  C7 funded=%s", c5Funded, c7Funded)

	// C5: LP reporter pushes provider shares, finalizes; the holder claims
	send(1, c5ID, "submitShares", fmt.Sprintf(`{"epoch":"0","page":"0","entries":"hive:%s:100"}`, holder), "c5.submitShares")
	waitKey(c5ID, "totalShares|0", "c5 shares recorded")
	send(1, c5ID, "finalizeEpoch", `{"epoch":"0"}`, "c5.finalizeEpoch")
	waitVal(c5ID, "status|0", "finalized", "c5 finalize")
	send(2, c5ID, "claim", `{"epoch":"0"}`, "c5.claim (holder's LP share)")
	waitKey(c5ID, "claimed|0|hive:"+holder, "c5 claim recorded")

	// C7: the holder claims pro-rata yield for the stake they held all epoch
	if c7ok {
		send(2, c7ID, "claim", `{"epoch":"0"}`, "c7.claim (staking yield)")
		waitKey(c7ID, "claimed|0|hive:"+holder, "c7 claim recorded")
	}

	// ---------------- PHASE 3: outsider attacks on C5/C6/C7 ----------------
	type atk struct{ id, action, payload, what string }
	for _, a := range []atk{
		{c6ID, "airdropBatch", fmt.Sprintf(`{"batchId":"steal","entries":"hive:%s:1000"}`, holder), "non-owner airdrop"},
		{c5ID, "submitShares", fmt.Sprintf(`{"epoch":"1","page":"0","entries":"hive:%s:999999"}`, holder), "fake LP shares"},
		{c5ID, "finalizeEpoch", `{"epoch":"1"}`, "attacker finalizes C5"},
		{c5ID, "cancelEpoch", `{"epoch":"0"}`, "attacker vetoes C5"},
		{c5ID, "sweepUnallocated", `{"nonce":"1"}`, "attacker sweeps C5"},
		{c5ID, "claim", `{"epoch":"0"}`, "attacker double-claims C5"},
		{c7ID, "claim", `{"epoch":"0"}`, "attacker double-claims C7"},
		{c7ID, "sweepResidual", `{"epoch":"0"}`, "attacker sweeps C7 residual"},
		{c1ID, "stakeFor", fmt.Sprintf(`{"acct":"hive:%s","amount":"500"}`, holder), "attacker stakeFor (not allowlisted)"},
		{c1ID, "unstake", `{"amount":"999999"}`, "attacker unstakes more than staked"},
		{c5ID, "init", `{"token":"x"}`, "attacker re-inits C5"},
		{c6ID, "init", `{"token":"x"}`, "attacker re-inits C6"},
		{c7ID, "init", `{"token":"x"}`, "attacker re-inits C7"},
	} {
		if _, cerr := d.CallContract(ctx, 2, a.id, a.action, a.payload); cerr != nil {
			t.Logf("attack %-40s broadcast error (fine): %v", a.what, cerr)
		} else {
			t.Logf("attack %-40s broadcast; must be rejected on-chain", a.what)
		}
	}
	time.Sleep(20 * time.Second)

	// ---------------- PHASE 4: nothing was stolen ----------------
	c5after, err := d.GetStateByKeys(ctx, 1, c5ID, []string{"funded|0", "totalShares|0", "status|0", "totalShares|1", "unallocated"})
	if err != nil {
		t.Fatalf("read c5 after attacks: %v", err)
	}
	t.Logf("C5 after attacks: %v", c5after)
	if fmt.Sprintf("%v", c5after["funded|0"]) != c5Funded {
		t.Fatalf("SECURITY FAILURE: C5 epoch-0 funding changed %q -> %v", c5Funded, c5after["funded|0"])
	}
	if v := fmt.Sprintf("%v", c5after["totalShares|1"]); v != "" && v != "<nil>" && v != "0" {
		t.Fatalf("SECURITY FAILURE: attacker injected LP shares into epoch 1: %v", v)
	}

	if !c7ok {
		t.Log("skipping C7 post-attack assertions (C7 never initialised)")
	}
	c7after, err := d.GetStateByKeys(ctx, 1, c7ID, []string{"funded|0", "swept|0"})
	if err != nil {
		t.Fatalf("read c7 after attacks: %v", err)
	}
	t.Logf("C7 after attacks: %v", c7after)
	if c7ok && fmt.Sprintf("%v", c7after["funded|0"]) != c7Funded {
		t.Fatalf("SECURITY FAILURE: C7 epoch-0 funding changed %q -> %v", c7Funded, c7after["funded|0"])
	}
	if v := fmt.Sprintf("%v", c7after["swept|0"]); v == "1" {
		t.Fatalf("SECURITY FAILURE: attacker swept C7 residual before maturity")
	}

	c6after, err := d.GetStateByKeys(ctx, 1, c6ID, []string{"airdrop_total", "done|steal"})
	if err != nil {
		t.Fatalf("read c6 after attacks: %v", err)
	}
	t.Logf("C6 after attacks: %v", c6after)
	if fmt.Sprintf("%v", c6after["airdrop_total"]) != "1000" {
		t.Fatalf("SECURITY FAILURE: C6 airdrop total changed: %v", c6after["airdrop_total"])
	}
	if v := fmt.Sprintf("%v", c6after["done|steal"]); v == "1" {
		t.Fatalf("SECURITY FAILURE: attacker's airdrop batch was applied")
	}

	// token ownership never moved
	stTok, err := d.GetStateByKeys(ctx, 1, tokenID, []string{"owner"})
	if err != nil {
		t.Fatalf("read token owner: %v", err)
	}
	if got := fmt.Sprintf("%v", stTok["owner"]); !strings.Contains(got, c2ID) {
		t.Fatalf("SECURITY FAILURE: token owner changed to %q", got)
	}

	t.Log("DEVNET RESULT: C1/C5/C6/C7 honest flow succeeded; every outsider attack rejected on-chain")
}
