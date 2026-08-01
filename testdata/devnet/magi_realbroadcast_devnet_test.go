package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestDevnetMagiRealBroadcast drives the reporter's OWN signing path against a live
// chain: it signs the transactions itself with an active key instead of handing
// payloads to the test harness to broadcast.
//
// Every other suite computes with the reporter and then broadcasts through
// d.CallContract, so broadcast.HiveBroadcaster — the code that builds the vsc.call
// envelope, sets the auth arrays and signs — has only ever been unit-tested. A
// mismatch in any of that would never surface, because nothing exercised it.
//
// It works in LP MODE specifically, and that is the whole trick. Content mode needs
// Hive post data, which must come from a fixture server — and the reporter uses ONE
// endpoint list for both reads and broadcasts, so a fixture endpoint cannot also
// accept transactions. LP mode reads its data from the indexer and touches Hive only
// for the head block, which the devnet's real Hive node answers on the same endpoint
// it accepts broadcasts on. So hive.api points at the real node and everything lines
// up.
//
// Run: go test -v -run TestDevnetMagiRealBroadcast -timeout 60m ./tests/devnet/
func TestDevnetMagiRealBroadcast(t *testing.T) {
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
	owner := w(1)
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
	tokenID := deploy("rb-token", magiTokenWasm)
	c2ID := deploy("rb-c2", magiWasm(t, "c2-emission/artifacts/main.wasm"))
	c5ID := deploy("rb-c5", magiWasm(t, "c5-lp/artifacts/main.wasm"))

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
			t.Logf("RC ready: %s hbd=%d", owner, b.Hbd)
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
		t.Logf("sent (harness, setup only): %s", what)
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
	const emission = 30000

	call(tokenID, "init", `{"name":"RB","symbol":"RB","decimals":0,"maxSupply":"100000000"}`, "token init")
	waitKey(tokenID, "owner", "token owner")
	call(tokenID, "mint", `{"amount":"1000000"}`, "mint the pool")
	call(tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, c2ID), "approve C2")
	call(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"%d","maxCatch":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"lp","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:%s","vetoThreshold":"1",`+
			`"buckets":"lp:contract:%s:10000"}`,
		tokenID, epochLen, guardian, treasury, c5ID), "C2 init")
	genesis, _ := strconv.ParseUint(waitKey(c2ID, "cfg_genesis", "genesis"), 10, 64)
	call(c5ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:%s","reporterThreshold":"1","treasury":"hive:%s",`+
			`"guardianMode":"0","guardianAuth":"hive:%s","guardianThreshold":"1","role":"lp"}`,
		tokenID, c2ID, owner, treasury, guardian), "C5 init")
	waitKey(c5ID, "cfg_funder", "C5 ready")

	// LP fixture: two providers in from the start, so epoch 0 has a real report.
	fx := &lpFixture{pool: "vsc1rbpool"}
	fx.set([]lpEvent{
		{"hive:" + owner, "1000", genesis, true},
		{"hive:" + w(2), "3000", genesis, true},
	})
	idx := fx.serve(t)
	defer idx.Close()

	// Let epoch 0 close, then tell the fake indexer it has indexed past it.
	epEnd := genesis + epochLen - 1
	time.Sleep(time.Duration(epochLen+6) * 3 * time.Second)
	fx.setHealth(epEnd + 50)

	// hive.api is the REAL devnet node: the reporter reads the head block from it and
	// broadcasts to it. That is only possible in LP mode — see the note above.
	const wifEnv = "MAGI_DEVNET_ACTIVE_WIF"
	t.Setenv(wifEnv, d.cfg.InitminerWIF)

	cfgBody, err := json.Marshal(map[string]any{
		"hive": map[string]any{"api": []string{d.HiveRPCEndpoint()}},
		"vsc":  map[string]any{"api": d.GQLEndpoint(1), "net_id": "vsc-devnet"},
		"indexer": map[string]any{
			"api": idx.URL, "secret": "", "pool": fx.pool, "page_size": 1000,
		},
		"contracts": map[string]any{"distributor": c5ID, "funder": c2ID, "stake": ""},
		"epoch":     map[string]any{"genesis": genesis, "len": epochLen},
		"source":    map[string]any{"kind": "lp"},
		"page":      map[string]any{"max_entries": 12, "max_bytes": 3500},
		"submit": map[string]any{
			"rc_limit": 60000, "keeper": true, "pull_funding": true, "finalize": true,
			"account": owner, "wif_env": wifEnv,
			"confirm_tries": 8, "confirm_interval_sec": 15,
			"progress_file": t.TempDir() + "/progress.json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := t.TempDir() + "/rb.json"
	if err := os.WriteFile(cfgPath, cfgBody, 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, reporterBin, append(args, "-config", cfgPath)...)
		cmd.Env = append(os.Environ(), wifEnv+"="+d.cfg.InitminerWIF)
		out, rerr := cmd.CombinedOutput()
		t.Logf("reporter %v:\n%s", args, out)
		if rerr != nil {
			t.Fatalf("reporter %v failed: %v", args, rerr)
		}
		return string(out)
	}

	t.Logf("reporter epoch state:\n%s", run("epoch"))

	// THE POINT: -broadcast, so the reporter signs and submits every call itself.
	run("run", "-epoch", "0", "-broadcast")

	// A reporter that printed success proves nothing; the chain acting on its
	// self-signed transactions does.
	for _, c := range []struct{ key, want, what string }{
		{"funded|0", strconv.Itoa(emission), "C5 funded by the reporter's own poke+pull"},
		{"totalShares|0", "4000", "shares applied from the reporter's own submitShares"},
		{"status|0", "finalized", "epoch finalized by the reporter's own tx"},
	} {
		deadline := time.Now().Add(4 * time.Minute)
		for {
			if got := stateOf(c5ID, c.key); got == c.want {
				t.Logf("REAL BROADCAST OK: %s (%s = %s)", c.what, c.key, got)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never reached %q (last %q) — the reporter reported success but the "+
					"chain did not act on its self-signed transaction, so the envelope, auth "+
					"arrays or payload shape it builds does not match what the runtime expects",
					c.key, c.want, stateOf(c5ID, c.key))
			}
			time.Sleep(6 * time.Second)
		}
	}

	t.Logf("REAL BROADCAST DEVNET PASSED — the reporter signed and submitted its own epoch")
}
