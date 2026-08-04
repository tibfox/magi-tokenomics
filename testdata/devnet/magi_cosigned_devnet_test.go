package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
)

// TestDevnetMagiCosigned exercises auth mode 1 (Cosigned) on a live chain.
//
// Single (0) and Attest (2) both run on devnet; Cosigned never had, because it needs
// M signatures in ONE transaction and the harness's CallContract hardcodes a single
// required auth. That made it the last auth mode covered by unit tests alone — and
// the one whose failure mode is worst, since it is the M-of-N that executes
// atomically rather than accumulating.
//
// It is reachable after all. This devnet's accounts share an active authority, and
// Hive validates each required auth against the signatures provided, so one signature
// satisfies a two-account required_auths list. The operation is built here rather
// than through the harness, which is allowed: this file is ours, and go-vsc-node
// already depends on hivego.
//
// Run: go test -v -run TestDevnetMagiCosigned -timeout 45m ./tests/devnet/
func TestDevnetMagiCosigned(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet test in short mode")
	}
	requireDocker(t)

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

	w := func(n int) string { return d.cfg.WitnessPrefix + strconv.Itoa(n) }
	owner, rep1, rep2 := w(1), w(1), w(2)
	treasury, guardian := w(4), w(5)

	// coCall broadcasts a vsc.call with an ARBITRARY required_auths list — the piece
	// the harness cannot express. One signature covers both accounts because they
	// share an active authority on this devnet.
	coCall := func(contractID, action, payload string, auths []string) (string, error) {
		callTx := map[string]any{
			"op": "call_contract", "action": action, "contract_id": contractID,
			"payload": json.RawMessage(payload), "rc_limit": 60000, "intents": []any{},
			"net_id": "vsc-devnet",
		}
		body, merr := json.Marshal(callTx)
		if merr != nil {
			return "", merr
		}
		op := hivego.CustomJsonOperation{
			RequiredAuths:        auths,
			RequiredPostingAuths: []string{},
			Id:                   "vsc.call",
			Json:                 string(body),
		}
		wif := d.cfg.InitminerWIF
		cl := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
		cl.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
		return cl.Broadcast([]hivego.HiveOperation{op}, &wif)
	}

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
	tokenID := deploy("co-token", magiTokenWasm)
	c2ID := deploy("co-c2", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c3ID := deploy("co-c3", magiWasm(t, "c3-distributor/artifacts/main.wasm"))

	for round := 0; round < 4; round++ {
		ok := false
		for _, amt := range []string{"200.000", "100.000", "50.000", "20.000", "5.000"} {
			if _, e := d.Deposit(ctx, 1, amt, "hbd"); e == nil {
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
			break
		}
		if time.Now().After(dl) {
			t.Fatal("deposits never credited")
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

	const epochLen = 30
	call(tokenID, "init", `{"name":"CO","symbol":"CO","decimals":0,"maxSupply":"100000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")
	call(tokenID, "mint", `{"amount":"1000000"}`, "mint pool")
	call(tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2")
	call(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"c","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"c:contract:%s:10000"}`,
		tokenID, epochLen, guardian, treasury, c3ID), "C2 init")
	waitKey(c2ID, "cfg_genesis", "genesis")

	call(c3ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1"}`,
		tokenID, c2ID, treasury, guardian), "distributor init")
	waitKey(c3ID, "cfg_funder", "distributor ready")
	// COSIGNED reporter: 2-of-2, both signatures required in ONE transaction. The
	// policy belongs to the channel, so this is what mode 1 is configured on.
	call(c3ID, "addChannel", fmt.Sprintf(
		`{"channel":"author","bucket":"author","window":"1",`+
			`"reporterMode":"1","reporterAuth":"hive:%s,hive:%s","reporterThreshold":"2"}`,
		rep1, rep2), "addChannel author (Cosigned 2-of-2)")
	waitKey(c3ID, "ch_bucket|author", "author channel registered")

	time.Sleep(time.Duration(epochLen+6) * 3 * time.Second)
	call(c2ID, "distributeEpoch", `{}`, "poke epoch 0")
	call(c3ID, "pullFunding", `{"channel":"author","epoch":"0"}`, "C3 pull e0")
	waitKey(c3ID, "funded|author|0", "C3 funded")

	const shares = `{"epoch":"0","page":"0","entries":"hive:coa:60,hive:cob:40"}`

	// ONE authority must NOT reach a 2-of-2 threshold, even though that account IS a
	// configured reporter. This is the assertion that separates Cosigned from Single.
	if _, e := coCall(c3ID, "submitShares", shares, []string{rep1}); e != nil {
		t.Logf("single-auth submitShares rejected at broadcast: %v", e)
	}
	time.Sleep(12 * time.Second)
	if v := stateOf(c3ID, "totalShares|author|0"); v != "" && v != "0" {
		t.Fatalf("COSIGNED BYPASSED: one authority applied shares to a 2-of-2 contract (totalShares=%s)", v)
	}
	t.Logf("one-of-two correctly applied nothing (totalShares=%q)", stateOf(c3ID, "totalShares|author|0"))

	// BOTH authorities in one transaction must apply it.
	txid, e := coCall(c3ID, "submitShares", shares, []string{rep1, rep2})
	if e != nil {
		t.Fatalf("cosigned submitShares failed to broadcast: %v", e)
	}
	t.Logf("cosigned tx (auths %s + %s): %s", rep1, rep2, txid)

	deadline := time.Now().Add(4 * time.Minute)
	for {
		if v := stateOf(c3ID, "totalShares|author|0"); v == "100" {
			t.Logf("COSIGNED OK: two authorities in ONE transaction applied the page (totalShares=100)")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cosigned submitShares never applied — totalShares is %q. The tx was accepted "+
				"(%s), so the contract did not count the second required auth toward the threshold",
				stateOf(c3ID, "totalShares|author|0"), txid)
		}
		time.Sleep(6 * time.Second)
	}

	t.Logf("COSIGNED DEVNET PASSED — mode 1 verified on a live chain")
}
