package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// C1 carries three roles that used to be three contracts: staking, staking yield, and
// the launch airdrop. These tests cover the seams the merge created — the places
// where one role could reach into another's money.

const (
	mgC1  = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	mgTre = "hive:mgtreasury"
)

// mgSetup wires token + C1 + C2 with epochLen 10 from genesis 0, the whole emission
// going to a `yield` bucket that pays C1.
func mgSetup(t *testing.T, extraC1 string) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(mgC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, mgC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"15","epochLen":"10","allow":"",`+
			`"treasury":"%s","guardianMode":"0","guardianAuth":"hive:mgguard",`+
			`"guardianThreshold":"1"%s}`, tokenID, mgTre, extraC1), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"yield","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"yield:contract:%s:10000"}`, tokenID, mgC1), owner, 0, true)
	call(t, &ct, mgC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 0, true)
	return &ct
}

func mgFund(t *testing.T, ct *test_utils.ContractTest, acct, amt string, h uint64) {
	t.Helper()
	call(t, ct, tokenID, "mint", fmt.Sprintf(`{"amount":"%s"}`, amt), owner, h, true)
	call(t, ct, tokenID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"%s"}`, acct, amt), owner, h, true)
	call(t, ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%s"}`, mgC1, amt), acct, h, true)
}

// The whole point of the merge, end to end: one contract stakes, funds itself from
// C2's bucket, and pays yield out of the stake history it already keeps.
func TestMerged_StakeAndYieldInOneContract(t *testing.T) {
	ct := mgSetup(t, "")
	mgFund(t, ct, "hive:alice", "600", 0)
	mgFund(t, ct, "hive:bob", "400", 0)
	call(t, ct, mgC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, ct, mgC1, "stake", `{"amount":"400"}`, "hive:bob", 0, true)
	call(t, ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, ct, mgC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)
	f := call(t, ct, mgC1, "fundedOf", `{"epoch":"0"}`, "hive:probe", 10, true)
	assert.EqualValues(t, 100000, c17I64(t, f.Ret, "funded"),
		"the whole emission goes to the yield bucket: "+f.Ret)

	pa := c17I64(t, call(t, ct, mgC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true).Ret, "claimed")
	pb := c17I64(t, call(t, ct, mgC1, "claimYield", `{"epoch":"0"}`, "hive:bob", 11, true).Ret, "claimed")
	assert.EqualValues(t, 60000, pa)
	assert.EqualValues(t, 40000, pb)
	assert.LessOrEqual(t, 100000-(pa+pb), int64(2), "exact denominator: dust only")

	// principal is untouched by paying yield
	assert.EqualValues(t, 1000, c17I64(t,
		call(t, ct, mgC1, "totalStaked", ``, "hive:probe", 11, true).Ret, "total"))
}

// THE SEAM THAT MATTERS. The airdrop shares a balance with staked principal, so a
// batch larger than the unobligated float must be refused — not paid out of somebody
// else's stake.
func TestMerged_AirdropCannotSpendStakedPrincipal(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000"`)
	// alice stakes 600. That is principal: the contract holds it but owes it to her.
	mgFund(t, ct, "hive:alice", "600", 0)
	call(t, ct, mgC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)

	// No airdrop float has been transferred in, so nothing is unobligated...
	at := call(t, ct, mgC1, "airdropTotal", ``, "hive:probe", 1, true)
	assert.Contains(t, at.Ret, `"unobligated":"0"`, "alice's stake must not read as spendable: "+at.Ret)

	// ...and a batch must therefore be refused, even though it is far under maxAirdrop.
	call(t, ct, mgC1, "airdropBatch",
		`{"batchId":"steal","entries":"hive:mallory:500"}`, owner, 1, false)
	assert.EqualValues(t, 0, c17TokenBal(t, ct, "hive:mallory", 1),
		"a batch with no float behind it must move nothing")
	assert.EqualValues(t, 600, c17I64(t,
		call(t, ct, mgC1, "totalStaked", ``, "hive:probe", 1, true).Ret, "total"),
		"alice's principal must be intact")

	// Fund the float properly and the SAME batch id now works (it was never applied).
	call(t, ct, tokenID, "mint", `{"amount":"500"}`, owner, 1, true)
	call(t, ct, tokenID, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"500"}`, mgC1), owner, 1, true)
	call(t, ct, mgC1, "airdropBatch",
		`{"batchId":"steal","entries":"hive:mallory:500"}`, owner, 2, true)
	assert.EqualValues(t, 500, c17TokenBal(t, ct, "hive:mallory", 2))
	assert.EqualValues(t, 600, c17I64(t,
		call(t, ct, mgC1, "totalStaked", ``, "hive:probe", 2, true).Ret, "total"),
		"and it still must not have touched principal")
}

// Funded-but-unclaimed yield is an obligation too: an airdrop must not spend the pool
// a staker has not got round to claiming.
func TestMerged_AirdropCannotSpendUnclaimedYield(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000"`)
	mgFund(t, ct, "hive:alice", "600", 0)
	call(t, ct, mgC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, ct, mgC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)

	// 100,000 of yield is now sitting here, funded and unclaimed. It is not float.
	at := call(t, ct, mgC1, "airdropTotal", ``, "hive:probe", 11, true)
	assert.Contains(t, at.Ret, `"unobligated":"0"`, "funded yield must not read as spendable: "+at.Ret)
	call(t, ct, mgC1, "airdropBatch",
		`{"batchId":"b1","entries":"hive:mallory:50000"}`, owner, 11, false)

	// alice's yield is still fully payable
	assert.EqualValues(t, 100000, c17I64(t,
		call(t, ct, mgC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true).Ret, "claimed"))
}

// Airdropping straight into stake: nothing leaves the contract, recipients are staked
// immediately, and the checkpoint history is correct so they earn the next epoch.
func TestMerged_AirdropCanCreditStakeDirectly(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000","airdropStaked":"1"`)
	// float for the airdrop
	call(t, ct, tokenID, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, ct, tokenID, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, mgC1), owner, 0, true)

	r := call(t, ct, mgC1, "airdropBatch",
		`{"batchId":"snap","entries":"hive:alice:600,hive:bob:400"}`, owner, 0, true)
	assert.Contains(t, r.Ret, `"staked":true`)

	// credited as STAKE, not as liquid balance
	assert.EqualValues(t, 0, c17TokenBal(t, ct, "hive:alice", 0), "nothing should be transferred out")
	assert.EqualValues(t, 1000, c17I64(t,
		call(t, ct, mgC1, "totalStaked", ``, "hive:probe", 0, true).Ret, "total"))

	// and they earn the epoch they were staked across, through the normal path
	call(t, ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, ct, mgC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)
	assert.EqualValues(t, 60000, c17I64(t,
		call(t, ct, mgC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true).Ret, "claimed"),
		"an airdropped staker earns from the first epoch, pro-rata")
}

// Capability follows config: a staking-only deployment supplies no yield or airdrop
// settings and gets neither, rather than a half-wired feature that fails later.
func TestMerged_UnconfiguredCapabilitiesRefuse(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(mgC1, owner, read("../c1-staking/artifacts/main.wasm"))
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// staking only — no treasury, no guardian, no maxAirdrop, no schedule
	call(t, &ct, mgC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"15","epochLen":"10","allow":""}`, tokenID), owner, 0, true)

	call(t, &ct, mgC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 20, false)
	call(t, &ct, mgC1, "airdropBatch", `{"batchId":"b","entries":"hive:a:1"}`, owner, 20, false)
	call(t, &ct, mgC1, "sweepEmptyEpoch", `{"epoch":"0"}`, "hive:mgguard", 20, false)

	// ...but staking itself works fine
	mgFund(t, &ct, "hive:alice", "600", 0)
	call(t, &ct, mgC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	assert.EqualValues(t, 600, c17I64(t,
		call(t, &ct, mgC1, "totalStaked", ``, "hive:probe", 1, true).Ret, "total"))
}
