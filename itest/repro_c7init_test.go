package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
)

// Reproduces the devnet C7-init failure: C2 initialised at a NON-ZERO height so
// its auto-genesis is that height, then C7 initialised with that same genesis.
// On devnet this aborted; find out why.
func TestRepro_C7InitWithAutoGenesis(t *testing.T) {
	const (
		rTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		rC1  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
		rC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	)
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(rTok, owner, read(tokenWasmPath))
	ct.RegisterContract(rC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(rC2, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, rTok, "init", `{"name":"R","symbol":"R","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	call(t, &ct, rC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"5","allow":""}`, rTok), owner, 0, true)

	// C2 init at height 204 -> auto-genesis should be 204 (genesis omitted)
	call(t, &ct, rC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","epochLen":"5","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"lp","timelock":"5","guardianMode":"0","guardianAuth":"hive:g1","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:v1","vetoThreshold":"1","buckets":"lp:contract:%s:5000,yield:contract:%s:5000"}`,
		rTok, rC1, rC1), owner, 204, true)

	// what did C2 actually store, and what does scheduleInfo report?
	t.Logf("C2 cfg_genesis  state = %q", ct.StateGet(rC2, "cfg_genesis"))
	t.Logf("C2 cfg_epochLen state = %q", ct.StateGet(rC2, "cfg_epochLen"))
	si := call(t, &ct, rC2, "scheduleInfo", ``, "hive:anyone", 210, true)
	t.Logf("C2 scheduleInfo ret  = %s", si.Ret)

	// now C7 init with that exact genesis — this is what failed on devnet
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, rC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, rC2), owner, 210, true)
}
