package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// C1.init's `funder` shortcut must not lock the yield bucket out forever.
//
// init treats `funder` as a convenience: when supplied it reads the funder's
// scheduleInfo and writes cfg_genesis, on the stated reasoning that "a funder at init
// means C2 already exists, so its genesis is knowable NOW and the separate
// adoptSchedule step is unnecessary".
//
// It is not unnecessary. adoptSchedule is the ONLY writer of cfg_bucket, and it opens
// with `if present(kGenesis) { abort }` — so init having written the genesis refuses
// the very call that arms the bucket. pullFunding then aborts on "no yield bucket
// adopted" forever, while C2 keeps accruing owed|yield|<ep> that only C1 may claim.
//
// The guard is not wrong to exist, it is too broad: its own abort text says the
// danger is RE-ANCHORING ("would invalidate every drawdown already recorded"), which
// is about the genesis, not about naming a bucket.

const fiC1 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"

// fiSetup inits C2 with a yield bucket paying C1, then inits C1 WITH the funder
// shortcut — the configuration under test.
func fiSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(fiC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"yield","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"yield:contract:%s:10000"}`, tokenID, fiC1), owner, 0, true)
	// THE CONFIGURATION UNDER TEST: the funder shortcut.
	call(t, &ct, fiC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":"","funder":"%s"}`,
		tokenID, c2ID), owner, 0, true)
	return &ct
}

// THE FINDING. init wrote the genesis; the bucket still has to be adoptable.
func TestFunderAtInit_BucketIsStillAdoptable(t *testing.T) {
	ct := fiSetup(t)
	assert.Equal(t, "0", stateOfKey(t, ct, fiC1, "cfg_genesis"), "init's shortcut must have anchored the genesis")
	assert.Empty(t, stateOfKey(t, ct, fiC1, "cfg_bucket"), "init does not name a bucket — only adoptSchedule does")

	call(t, ct, fiC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 1, true)
	assert.Equal(t, "yield", stateOfKey(t, ct, fiC1, "cfg_bucket"),
		"the bucket must be adoptable after the funder shortcut, or yield is stranded in C2 forever")
}

// What the block actually costs, end to end.
func TestFunderAtInit_YieldReachesTheStakers(t *testing.T) {
	ct := fiSetup(t)
	call(t, ct, fiC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 1, true)

	mgFundFor(t, ct, fiC1, "hive:alice", "1000", 2)
	call(t, ct, fiC1, "stake", `{"amount":"1000"}`, "hive:alice", 2, true)

	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 21, true)
	call(t, ct, fiC1, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 21, true)

	r := call(t, ct, tokenID, "balanceOf", fmt.Sprintf(`{"account":"contract:%s"}`, fiC1),
		"hive:probe", 22, true)
	assert.NotContains(t, r.Ret, `"balance":"1000"`,
		"C1 must hold more than the staked principal — the epoch's yield must have arrived")
}

// The guard still has to do its real job: the genesis anchor is immutable, because
// re-anchoring would invalidate every drawdown already recorded.
func TestFunderAtInit_ReAnchoringIsStillRefused(t *testing.T) {
	ct := fiSetup(t)
	call(t, ct, fiC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 1, true)

	// a second adoption naming the same funder must not re-run
	r := call(t, ct, fiC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 2, false)
	caFailedFor(t, r, "already adopted")

	// and one naming a DIFFERENT funder must never be able to re-anchor
	r = call(t, ct, fiC1, "adoptSchedule",
		`{"funder":"vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj","bucket":"yield"}`, owner, 3, false)
	assert.False(t, r.Success, "a different funder must never re-anchor the schedule")
}

// Without the shortcut nothing changed: the ordinary two-step deploy still works.
func TestFunderAtInit_PlainAdoptStillWorks(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(fiC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, fiC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, tokenID), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"yield","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"yield:contract:%s:10000"}`, tokenID, fiC1), owner, 0, true)
	call(t, &ct, fiC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 1, true)
	assert.Equal(t, "yield", stateOfKey(t, &ct, fiC1, "cfg_bucket"))
	assert.Equal(t, "0", stateOfKey(t, &ct, fiC1, "cfg_genesis"))
}

// mgFundFor mints, transfers and approves against an arbitrary C1 instance.
func mgFundFor(t *testing.T, ct *test_utils.ContractTest, c1, acct, amt string, h uint64) {
	t.Helper()
	call(t, ct, tokenID, "mint", fmt.Sprintf(`{"amount":"%s"}`, amt), owner, h, true)
	call(t, ct, tokenID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"%s"}`, acct, amt), owner, h, true)
	call(t, ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%s"}`, c1, amt), acct, h, true)
}
