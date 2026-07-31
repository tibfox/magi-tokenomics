package itest_test

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"os"
	"strconv"
	"testing"

	"vsc-node/lib/test_utils"
)

// Measures the REAL metered RC cost of distributeEpoch as a function of how many
// epochs a single poke has to catch up. Uses the same metering the chain uses
// (RcUsed = ceil(gas / CYCLE_GAS_PER_RC), floor 100).
//
// Context: the free tier is 10,000 RC (params.RC_HIVE_FREE_AMOUNT).
func TestRC_DistributeEpochCatchUpCost(t *testing.T) {
	const (
		rcTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		rcC2  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
		rcB1  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
		rcB2  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
		rcB3  = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
	)
	// (epochsToCatchUp, bucketCount)
	cases := []struct {
		epochs  uint64
		buckets int
	}{
		{1, 1}, {5, 1}, {10, 1}, {25, 1}, {50, 1},
		{1, 3}, {10, 3}, {50, 3},
	}
	fmt.Println("\n=== distributeEpoch metered RC (free tier = 10,000) ===")
	fmt.Printf("%8s %8s %10s %12s %s\n", "epochs", "buckets", "RC used", "gas used", "fits free tier?")
	for _, c := range cases {
		os.RemoveAll("data/badger")
		ct := test_utils.NewContractTest()
		ct.RegisterContract(rcTok, owner, read(tokenWasmPath))
		ct.RegisterContract(rcC2, owner, read("../c2-emission/artifacts/main.wasm"))

		buckets := fmt.Sprintf("author:contract:%s:10000", rcB1)
		if c.buckets == 3 {
			buckets = fmt.Sprintf("author:contract:%s:5000,yield:contract:%s:3000,lp:contract:%s:2000", rcB1, rcB2, rcB3)
		}
		call(t, &ct, rcTok, "init", `{"name":"R","symbol":"R","decimals":0,"maxSupply":"100000000000"}`, owner, 0, true)
		call(t, &ct, rcC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"1000","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:g","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:v","vetoThreshold":"1","buckets":"%s"}`,
			rcTok, buckets), owner, 0, true)
		// C2 DRAWS from an approved pool; it no longer mints. Without a funded pool
		// every poke below returns {"distributed":"0","starved":true} and this whole
		// table silently measures a no-op (271 RC) instead of real emission.
		// emission = baseAnnual*epochLen/blocksPerYear = 1000000*1/1000 = 1000/epoch,
		// so 1000000 covers the 50-epoch worst case many times over.
		call(t, &ct, rcTok, "mint", `{"amount":"1000000"}`, owner, 0, true)
		call(t, &ct, rcTok, "approve",
			fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000"}`, rcC2), owner, 0, true)
		call(t, &ct, rcTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, rcC2), owner, 0, true)

		// epochLen=1, genesis=0 → poking at height N catches up N epochs (capped by maxCatch)
		res := call(t, &ct, rcC2, "distributeEpoch", ``, "hive:keeper", c.epochs, true)
		// ASSERT what was measured. Without this the table is not a measurement, it
		// is a printout: this test previously recorded 271 RC for a starved no-op for
		// weeks because nothing checked that any epoch was actually distributed. A
		// number nobody asserts is a number nobody can trust.
		want := c.epochs
		if want > 50 {
			want = 50 // maxCatch default
		}
		assert.Equal(t, strconv.FormatUint(want, 10), cvField(res.Ret, "distributed"),
			"RC figures are meaningless unless the poke actually distributed: %s", res.Ret)
		assert.NotContains(t, res.Ret, "starved",
			"a starved poke measures nothing — fund the pool in this fixture")

		fits := "YES"
		if res.RcUsed > 10000 {
			fits = "NO  <-- exceeds free tier"
		}
		fmt.Printf("%8d %8d %10d %12d %s   ret=%s\n", c.epochs, c.buckets, res.RcUsed, res.GasUsed, fits, res.Ret)
		ct.DataLayer.Stop()
	}
	fmt.Println()
}

// The leaking bump allocator (runtime/gc_leaking_exported.go never frees) meets
// distributeEpoch's maxCatch, which init allows up to 1000. Nobody had bounded the
// worst case, and the failure mode under -panic=trap would be a MESSAGE-LESS wasm
// trap — indistinguishable from any other failure.
//
// This does not prove a bound; it establishes empirically that the extreme
// configuration fails (if it fails) by exhausting GAS with a diagnosable result,
// rather than by trapping. Printed, not asserted, except for the one thing that
// matters: no crash without a result.
func TestRC_MaxCatchWorstCaseDoesNotTrap(t *testing.T) {
	const (
		rcTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		rcC2  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
		rcB1  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
		rcB2  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
		rcB3  = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
	)
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(rcTok, owner, read(tokenWasmPath))
	ct.RegisterContract(rcC2, owner, read("../c2-emission/artifacts/main.wasm"))

	buckets := fmt.Sprintf("author:contract:%s:5000,yield:contract:%s:3000,lp:contract:%s:2000",
		rcB1, rcB2, rcB3)
	call(t, &ct, rcTok, "init", `{"name":"R","symbol":"R","decimals":0,"maxSupply":"100000000000"}`, owner, 0, true)
	call(t, &ct, rcTok, "mint", `{"amount":"100000000"}`, owner, 0, true)
	call(t, &ct, rcTok, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"100000000"}`, rcC2), owner, 0, true)
	call(t, &ct, rcC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"author","timelock":"5","maxCatch":"1000",`+
			`"guardianMode":"0","guardianAuth":"hive:g","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:v","vetoThreshold":"1","buckets":"%s"}`,
		rcTok, buckets), owner, 0, true)

	// 1000 epochs x 3 buckets in ONE call — the worst case the config permits.
	res := call(t, &ct, rcC2, "distributeEpoch", ``, "hive:keeper", 1000, false)
	fmt.Printf("\n=== maxCatch worst case: 1000 epochs x 3 buckets ===\n")
	fmt.Printf("ok=%v rc=%d gas=%d ret=%q err=%q\n", res.Success, res.RcUsed, res.GasUsed, res.Ret, res.ErrMsg)

	// Whatever happened, the engine must have returned a RESULT rather than dying:
	// a bare trap would surface as an empty error with no gas accounting.
	assert.True(t, res.GasUsed > 0, "the call must have consumed gas and returned a result, "+
		"not vanished into a message-less trap")
}
