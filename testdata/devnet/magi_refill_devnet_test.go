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

// TestDevnetMagiRefill proves on a live chain that a DRAINED EMISSION POOL CAN BE
// REFILLED — the property that makes minting the supply in batches (25% at a time,
// say) viable rather than a one-way trip.
//
// Until 2026-07-29 this did not work: `terminal` latched on exhaustion, so
// distributeEpoch short-circuited forever and never re-read the allowance. A second
// batch was dead on arrival. Only unit tests covered the fix, so this closes the gap.
//
//	batch 1   pool = EXACTLY one epoch's emission
//	epoch 0   funds normally, draining the pool to zero
//	epoch 1   hits the wall — the poke is a no-op, nothing is funded, nothing latches
//	refill    mint + increaseAllowance a further TWO epochs' worth
//	          the next poke pays the BACKLOG (epochs 1 and 2) at the FULL rate,
//	          then stops again at the new pool boundary
//
// Sizing the pool to exactly one epoch is what makes this timing-independent. Epochs
// are funded all-or-nothing, so epoch 0 funds and epoch 1 starves no matter how many
// epochs have really elapsed by the time each poke lands — which matters because
// devnet wall-clock timing is not controllable.
//
// Run: go test -v -run TestDevnetMagiRefill -timeout 60m ./tests/devnet/
func TestDevnetMagiRefill(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)

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
	owner := w(1) // deployer AND the pool holder
	treasury := w(4)
	guardian := w(5)

	// ---------------- deploy + fund ----------------
	// Only the token and C2 are needed: the bucket pays a plain hive account, so no
	// distributor has to exist for the emission accounting to be observable.
	deploy := func(name, wasm string, node int) string {
		var lastErr error
		for attempt := 1; attempt <= 3; attempt++ {
			id, e := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: name, DeployerNode: node})
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
	tokenID := deploy("rf-token", magiTokenWasm, 1)
	c2ID := deploy("rf-c2", magiWasm(t, "c2-emission/artifacts/main.wasm"), 1)

	for round := 0; round < 4; round++ {
		ok := false
		for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
			if _, e := d.Deposit(ctx, 1, amt, "hbd"); e == nil {
				t.Logf("node 1 deposited %s HBD", amt)
				ok = true
				break
			}
		}
		if !ok {
			break
		}
	}
	dl := time.Now().Add(4 * time.Minute)
	for {
		b, e := d.GetAccountBalance(ctx, 1, "hive:"+owner)
		if e == nil && b != nil && b.Hbd > 0 {
			t.Logf("RC ready: %s hbd=%d", owner, b.Hbd)
			break
		}
		if time.Now().After(dl) {
			t.Fatal("deposits never credited — the run cannot fit the free tier")
		}
		time.Sleep(6 * time.Second)
	}

	// ---------------- helpers ----------------
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
	// mustStayAbsent gives the chain ample time to disprove it: an assertion that a
	// key is missing is worthless if it merely outran block production.
	mustStayAbsent := func(id, key, what string) {
		deadline := time.Now().Add(75 * time.Second)
		for time.Now().Before(deadline) {
			if v := stateOf(id, key); v != "" {
				t.Fatalf("%s must NOT be funded, but %s[%s] = %q", what, id, key, v)
			}
			time.Sleep(6 * time.Second)
		}
		t.Logf("%s is absent, as required", what)
	}
	c2Balance := func() *big.Int {
		return stateBigHex(t, d, d.GQLEndpoint(1), tokenID, "bal|contract:"+c2ID)
	}
	mustDraw := func(want int64, what string) {
		deadline := time.Now().Add(4 * time.Minute)
		var last *big.Int
		for time.Now().Before(deadline) {
			last = c2Balance()
			if last != nil && last.Cmp(big.NewInt(want)) == 0 {
				t.Logf("%s: C2 has drawn %d from the pool", what, want)
				return
			}
			time.Sleep(6 * time.Second)
		}
		t.Fatalf("%s: C2 drew %v, want %d", what, last, want)
	}

	const epochLen = 30
	// emission = baseAnnual * epochLen / blocksPerYear = 1000000 * 30 / 1000
	const emission = 30000

	// ---------------- init ----------------
	call(tokenID, "init", `{"name":"Refill","symbol":"RFL","decimals":0,"maxSupply":"100000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")

	// BATCH 1 — exactly one epoch's worth.
	//
	// Token ownership is deliberately NOT handed to C2. Under the allowance model C2
	// needs no authority over the token, and `mint` is owner-only: handing it over
	// would make the refill below impossible without a timelocked changeOwner
	// round-trip. This test is therefore also the executable form of that warning.
	call(tokenID, "mint", fmt.Sprintf(`{"amount":"%d"}`, emission), "batch 1: mint ONE epoch of pool")
	call(tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%d"}`, c2ID, emission), "approve C2 for batch 1")

	call(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"content","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"content:hive:%s:10000"}`,
		tokenID, epochLen, guardian, treasury, treasury), "C2 init")
	genesis, _ := strconv.ParseUint(waitKey(c2ID, "cfg_genesis", "genesis"), 10, 64)
	t.Logf("genesis=%d epochLen=%d emission=%d/epoch; pool holds exactly 1 epoch",
		genesis, epochLen, emission)

	owedKey := func(ep int) string { return fmt.Sprintf("owed|hive:%s|%d", treasury, ep) }
	waitEpochClosed := func(ep uint64) {
		t.Logf("waiting for epoch %d to close (block %d)...", ep, genesis+(ep+1)*epochLen)
		time.Sleep(time.Duration(epochLen+6) * 3 * time.Second)
	}

	// ================= EPOCH 0: funds, and drains the pool dead =================
	waitEpochClosed(0)
	call(c2ID, "distributeEpoch", `{}`, "poke epoch 0")
	waitValue(c2ID, owedKey(0), strconv.Itoa(emission), "epoch 0 owed")
	mustDraw(emission, "epoch 0")
	t.Logf("EPOCH 0 OK: funded in full and the pool is now empty")

	// ================= EPOCH 1: the wall =================
	// The pool cannot cover another epoch. The poke must be a harmless no-op: nothing
	// funded, nothing drawn — and crucially nothing LATCHED, which the refill proves.
	waitEpochClosed(1)
	call(c2ID, "distributeEpoch", `{}`, "poke epoch 1 — pool is empty, must starve")
	mustStayAbsent(c2ID, owedKey(1), "epoch 1")
	mustDraw(emission, "after the starved poke (unchanged)")

	// Poke again to be sure repeated pokes against an empty pool stay harmless.
	call(c2ID, "distributeEpoch", `{}`, "poke again while starved")
	mustStayAbsent(c2ID, owedKey(1), "epoch 1 after a second starved poke")
	t.Logf("WALL OK: epoch 1 unfunded, nothing drawn, no partial payment")

	// ================= REFILL: batch 2 = TWO epochs' worth =================
	// increaseAllowance, NOT approve: approve OVERWRITES the allowance, so re-approving
	// would silently discard any unspent remainder. (Here the remainder is zero, but
	// the operational habit is what this test is documenting.)
	//
	// This mint is only possible because ownership was never handed to C2.
	call(tokenID, "mint", fmt.Sprintf(`{"amount":"%d"}`, 2*emission), "batch 2: mint TWO epochs of pool")
	call(tokenID, "increaseAllowance",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%d"}`, c2ID, 2*emission), "increaseAllowance for batch 2")

	// ================= RESUME: the backlog is paid, at the FULL rate =================
	call(c2ID, "distributeEpoch", `{}`, "poke after refill — must resume and pay the backlog")
	waitValue(c2ID, owedKey(1), strconv.Itoa(emission), "epoch 1 owed (backlog)")
	waitValue(c2ID, owedKey(2), strconv.Itoa(emission), "epoch 2 owed (backlog)")
	mustDraw(3*emission, "after the refill")
	t.Logf("REFILL OK: epochs 1 and 2 paid the FULL %d each — a starved schedule resumed", emission)

	// The pool holds exactly two epochs, so it must stop again at the new boundary
	// rather than overdrawing the allowance.
	mustStayAbsent(c2ID, owedKey(3), "epoch 3 (beyond the refilled pool)")
	t.Logf("BOUNDARY OK: stopped at the refilled pool, no overdraw")

	t.Logf("REFILL DEVNET PASSED — pool drained at epoch 1, refilled, backlog paid in full")
}
