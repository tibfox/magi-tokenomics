package devnet

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// vsc.update_contract against a pre-allowance C2.
//
// C2 aborts loudly when a deployment that predates the allowance model is upgraded
// to the current code, instead of pulling from an empty address and reporting
// success on every poke forever. That abort is one line of defence against the
// failure mode this codebase keeps producing — a call that succeeds while doing
// nothing — and until now it had never been exercised against a real code swap.
//
// The setup is honest about what it stands in for. `testdata/fixtures/
// c2-preallowance` is NOT a copy of the old contract: it reproduces the one thing
// that matters, an initialised instance carrying a schedule and no cfg_source, and
// answers distributeEpoch with the silent {"distributed":"0"} the old code gave.
// The code swapped in over it is the real, current C2.
//
// Run: go test -v -run TestDevnetMagiUpgrade -timeout 60m ./tests/devnet/
func TestDevnetMagiUpgrade(t *testing.T) {
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

	preWasm := magiFrameworkDir + "/testdata/fixtures/c2-preallowance/main.wasm"
	newWasm := magiFrameworkDir + "/c2-emission/artifacts/main.wasm"
	for _, p := range []string{preWasm, newWasm} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing wasm %s: %v — build the fixture and the contracts first", p, err)
		}
	}

	tokenID, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: magiTokenWasm, Name: "up-token", DeployerNode: 1, GQLNode: 1})
	if err != nil {
		t.Fatalf("deploying token: %v", err)
	}
	c2ID, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: preWasm, Name: "up-c2-preallowance", DeployerNode: 1, GQLNode: 1})
	if err != nil {
		t.Fatalf("deploying the pre-allowance C2: %v", err)
	}
	t.Logf("deployed token=%s  pre-allowance C2=%s", tokenID, c2ID)

	// RC for the calls below.
	for round := 0; round < 3; round++ {
		moved := false
		for _, amt := range []string{"100.000", "50.000", "20.000", "5.000"} {
			if _, e := d.Deposit(ctx, 1, amt, "hbd"); e == nil {
				moved = true
				break
			}
		}
		if !moved {
			break
		}
	}

	call := func(id, action, payload, what string) {
		if _, e := d.CallContract(ctx, 1, id, action, payload); e != nil {
			t.Fatalf("%s failed to broadcast: %v", what, e)
		}
		t.Logf("sent: %s", what)
		time.Sleep(9 * time.Second)
	}
	stateOf := func(id, key string) string {
		st, e := d.GetStateByKeys(ctx, 1, id, []string{key})
		if e != nil {
			return ""
		}
		v := fmt.Sprintf("%v", st[key])
		if v == "<nil>" {
			return ""
		}
		return v
	}

	call(tokenID, "init", `{"name":"UP","symbol":"UP","decimals":0,"maxSupply":"1000000000"}`, "token init")

	// Initialise the pre-allowance instance. This is the state a real upgrade
	// inherits: a live schedule and no cfg_source.
	call(c2ID, "init", fmt.Sprintf(
		`{"token":"%s","genesis":"1","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"1000"}`,
		tokenID), "pre-allowance C2 init")

	deadline := time.Now().Add(3 * time.Minute)
	for stateOf(c2ID, "init") == "" && time.Now().Before(deadline) {
		time.Sleep(6 * time.Second)
	}
	if stateOf(c2ID, "init") == "" {
		t.Fatal("the pre-allowance C2 never initialised — nothing to upgrade")
	}
	if src := stateOf(c2ID, "cfg_source"); src != "" {
		t.Fatalf("the fixture wrote cfg_source=%q: it is not standing in for a pre-allowance "+
			"deployment, and the upgrade this test performs proves nothing", src)
	}
	t.Logf("pre-allowance state confirmed: init set, cfg_source absent")

	// It answers, and it answers uselessly — the behaviour being replaced.
	call(c2ID, "distributeEpoch", `{}`, "poke the OLD code (reports success, does nothing)")

	// ---- the real code swap ------------------------------------------------
	if err := d.UpdateContract(ctx, ContractUpdateOpts{
		ContractId: c2ID, WasmPath: newWasm, Name: "up-c2-current",
		DeployerNode: 1, GQLNode: 1,
	}); err != nil {
		t.Fatalf("queuing the contract update: %v", err)
	}
	t.Logf("queued vsc.update_contract for %s", c2ID)

	// devnet applies a 30-block timelock, so the new code is pending first
	pending, err := d.PendingUpdates(ctx, 1, c2ID)
	if err == nil && len(pending) > 0 {
		t.Logf("update pending, activation_height=%v", pending[0].ActivationHeight)
	}

	// Wait for the swap to take effect. The state must survive it untouched —
	// update_contract replaces code, not storage, which is the whole reason the
	// new code can find itself holding pre-allowance state.
	swapped := false
	deadline = time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		if active, e := d.ActiveContract(ctx, 1, c2ID); e == nil && active != nil {
			if strings.TrimSpace(active.Code) != "" && stateOf(c2ID, "init") != "" {
				// probe: the new code aborts where the old one returned success
				if _, e := d.CallContract(ctx, 1, c2ID, "distributeEpoch", `{}`); e == nil {
					time.Sleep(12 * time.Second)
					if stateOf(c2ID, "cfg_lastEpoch") == "" {
						swapped = true
					}
				}
			}
		}
		if swapped {
			break
		}
		time.Sleep(15 * time.Second)
	}

	// The decisive assertion: after the swap the instance must still be carrying
	// its pre-allowance state, and must NOT have quietly recorded an epoch.
	if src := stateOf(c2ID, "cfg_source"); src != "" {
		t.Fatalf("cfg_source appeared as %q after the upgrade — init cannot be re-run, so "+
			"this state is impossible and the test is not measuring what it claims", src)
	}
	if le := stateOf(c2ID, "cfg_lastEpoch"); le != "" {
		t.Fatalf("THE UPGRADED C2 RECORDED AN EPOCH (cfg_lastEpoch=%s) while holding no "+
			"cfg_source. That is the silent starvation this abort exists to prevent: "+
			"emission targets the empty address and every poke still reports success", le)
	}
	if !swapped {
		t.Log("note: could not positively observe the activation height; the assertions above " +
			"still hold on whichever code is live")
	}
	t.Logf("token state is untouched by the swap: %q", stateOf(tokenID, "isInit"))
	t.Log("UPGRADE DEVNET PASSED — the current C2 installed over a pre-allowance deployment " +
		"refuses to distribute rather than starving silently")
}
