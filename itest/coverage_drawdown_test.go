package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Tests for C1's per-epoch drawdown accumulator — the number that lets C7 divide by
// the EXACT Σ min(stake@start, stake@end) instead of the over-counting min(Σa,Σb).
//
// The accumulator is what removed C7's claim deadline: an exact denominator leaves no
// unclaimable residue, so there is nothing for a guardian to sweep, so claims never
// have to close. These tests pin the arithmetic that argument rests on.

const ddC1 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"

// ddSetup wires token + C1 + C2 + C7 with epochLen 10 from genesis 0, so epoch 0 spans
// blocks 0..9 and the whole emission (100000) goes to yield.
func ddSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(ddC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// cooldown must exceed epochLen (R15)
	call(t, &ct, ddC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"15","epochLen":"10","allow":"",`+
			`"treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1"}`, tokenID), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"yield","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"yield:contract:%s:10000"}`, tokenID, ddC1), owner, 0, true)
	call(t, &ct, ddC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 0, true)
	return &ct
}

// ddFund gives an account liquid tokens and an allowance on C1.
func ddFund(t *testing.T, ct *test_utils.ContractTest, acct, amt string, h uint64) {
	t.Helper()
	call(t, ct, tokenID, "mint", fmt.Sprintf(`{"amount":"%s"}`, amt), owner, h, true)
	call(t, ct, tokenID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"%s"}`, acct, amt), owner, h, true)
	call(t, ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%s"}`, ddC1, amt), acct, h, true)
}

// THE INSOLVENCY GUARD. The accumulator must track each account's NET position against
// its epoch-start level, never the GROSS flow through it.
//
// Alice ends the epoch exactly where she started, having dipped in between. Her
// drawdown is therefore 0 and the denominator is the full 1000.
//
// Counting gross unstakes instead would record 300, give a denominator of 700, and pay
// out 100000*(600+400)/700 = 142,857 against 100,000 of funding — the contract would
// promise half again as much as it holds and the last claimant would find it empty.
// That is the single most dangerous way to get this wrong, because it only shows up
// when someone re-stakes within an epoch.
func TestCovDrawdown_UnstakeThenRestakeIsNotDoubleCounted(t *testing.T) {
	ct := ddSetup(t)
	ddFund(t, ct, "hive:alice", "900", 0)
	ddFund(t, ct, "hive:bob", "400", 0)

	call(t, ct, ddC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, ct, ddC1, "stake", `{"amount":"400"}`, "hive:bob", 0, true)
	call(t, ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	// alice dips to 300 and comes back to 600, all inside epoch 0
	call(t, ct, ddC1, "unstake", `{"amount":"300"}`, "hive:alice", 3, true)
	call(t, ct, ddC1, "stake", `{"amount":"300"}`, "hive:alice", 6, true)

	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, ct, ddC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)

	// denominator = min(600,600) + min(400,400) = 1000, NOT 700
	pa := c17I64(t, call(t, ct, ddC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true).Ret, "claimed")
	pb := c17I64(t, call(t, ct, ddC1, "claimYield", `{"epoch":"0"}`, "hive:bob", 11, true).Ret, "claimed")
	assert.EqualValues(t, 60000, pa, "alice returned to her starting stake, so she is not penalised")
	assert.EqualValues(t, 40000, pb)
	assert.EqualValues(t, 100000, pa+pb,
		"Σclaims must equal funded exactly — a gross-flow accumulator would over-promise")
}

// The accumulator must TELESCOPE: many moves inside one epoch collapse to the single
// final max(0, start-end), not the sum of the individual steps.
//
// Alice steps 600→500→400→350 (three unstakes totalling 250) and carol exits entirely.
// Drawdown must be 250 + 200, so the denominator is 1000 - 450 = 550.
func TestCovDrawdown_TelescopesAcrossManyMutations(t *testing.T) {
	ct := ddSetup(t)
	ddFund(t, ct, "hive:alice", "600", 0)
	ddFund(t, ct, "hive:bob", "200", 0)
	ddFund(t, ct, "hive:carol", "200", 0)

	call(t, ct, ddC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, ct, ddC1, "stake", `{"amount":"200"}`, "hive:bob", 0, true)
	call(t, ct, ddC1, "stake", `{"amount":"200"}`, "hive:carol", 0, true)
	call(t, ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	call(t, ct, ddC1, "unstake", `{"amount":"100"}`, "hive:alice", 2, true)
	call(t, ct, ddC1, "unstake", `{"amount":"100"}`, "hive:alice", 5, true)
	call(t, ct, ddC1, "unstake", `{"amount":"50"}`, "hive:alice", 8, true)
	call(t, ct, ddC1, "unstake", `{"amount":"200"}`, "hive:carol", 4, true) // full exit

	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, ct, ddC1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)

	// Σ min = alice 350 + bob 200 + carol 0 = 550
	pa := c17I64(t, call(t, ct, ddC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true).Ret, "claimed")
	pb := c17I64(t, call(t, ct, ddC1, "claimYield", `{"epoch":"0"}`, "hive:bob", 11, true).Ret, "claimed")
	call(t, ct, ddC1, "claimYield", `{"epoch":"0"}`, "hive:carol", 11, false) // exited → min 0

	assert.EqualValues(t, 63636, pa, "alice earns on her LOWEST point, 350/550")
	assert.EqualValues(t, 36363, pb, "bob held 200 throughout, 200/550")
	assert.LessOrEqual(t, 100000-(pa+pb), int64(2),
		"only truncation dust may remain — the epoch is otherwise fully distributed")
}

// The schedule anchor decides which epoch every drawdown is filed under, so a wrong or
// re-pointed genesis would silently corrupt the denominator for live epochs. It is
// owner-only and once-only, and both halves are load-bearing rather than ceremonial.
func TestCovDrawdown_AdoptScheduleIsOwnerOnlyAndOnce(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(ddC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, ddC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"15","epochLen":"10","allow":"",`+
			`"treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1"}`, tokenID), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"yield","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"yield:contract:%s:10000"}`, tokenID, ddC1), owner, 0, true)

	// An outsider must not be able to anchor the schedule — pointing C1 at a fabricated
	// funder would shift every epoch boundary.
	call(t, &ct, ddC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s"}`, c2ID), "hive:mallory", 0, false)

	call(t, &ct, ddC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s"}`, c2ID), owner, 0, true)

	// ...and not twice, even by the owner: re-anchoring would retroactively invalidate
	// every drawdown already recorded against the old boundaries.
	call(t, &ct, ddC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s"}`, c2ID), owner, 1, false)
}

// minStakeSum must refuse an epoch that is still open. The drawdown is still moving,
// so an answer now can be larger than the final one — and paying an early claimant
// against a too-large denominator under-pays them permanently, while paying against a
// too-small one over-promises the epoch.
func TestCovDrawdown_MinStakeSumRefusesAnOpenEpoch(t *testing.T) {
	ct := ddSetup(t)
	ddFund(t, ct, "hive:alice", "600", 0)
	call(t, ct, ddC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)

	// epoch 0 spans 0..9; at h=5 it is still open
	call(t, ct, ddC1, "minStakeSum", `{"epoch":"0"}`, "hive:probe", 5, false)
	// at h=10 it has closed
	r := call(t, ct, ddC1, "minStakeSum", `{"epoch":"0"}`, "hive:probe", 10, true)
	assert.Contains(t, r.Ret, `"600"`, "a closed epoch reports the exact Σ min: "+r.Ret)
}
