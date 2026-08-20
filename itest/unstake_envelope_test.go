package itest_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// A queued unstake is still the staker's money, and the envelope must say so.
//
// C1 holds three pools in ONE token balance and defends them with:
//
//	balance >= total_staked + (yield funded but unclaimed) + airdrop float
//
// `unstake` decrements total_staked IMMEDIATELY — it has to, or an account on its way
// out keeps earning weight — but moves no tokens. They stay in custody until
// claimUnstaked pays them after a cooldown that init requires to exceed a full
// emission epoch. For that whole window the amount is out of total_staked and still
// in the balance.
//
// If nothing else accounts for it, unobligated() reports a staker's principal as free
// float, and both things that spend float — airdropBatch and sweepUnobligated — will
// happily send it somewhere else.

func ueUnobligated(t *testing.T, ct *test_utils.ContractTest, h uint64) string {
	t.Helper()
	r := call(t, ct, mgC1, "airdropTotal", ``, "hive:probe", h, true)
	var out struct {
		Unobligated string `json:"unobligated"`
	}
	if err := json.Unmarshal([]byte(r.Ret), &out); err != nil {
		t.Fatalf("airdropTotal returned %q: %v", r.Ret, err)
	}
	return out.Unobligated
}

// THE FINDING, in one assertion. Nothing has left the contract and nobody has been
// paid, so there is no free float — every token here is Alice's.
func TestUnstakeEnvelope_QueuedWithdrawalIsNotFreeFloat(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000"`)
	mgFund(t, ct, "hive:alice", "1000", 1)
	call(t, ct, mgC1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)

	assert.Equal(t, "0", ueUnobligated(t, ct, 2),
		"staked principal must not be free float")

	// Alice unstakes. total_staked drops now; the tokens do not move until the
	// cooldown elapses and she claims them.
	call(t, ct, mgC1, "unstake", `{"amount":"1000"}`, "hive:alice", 3, true)

	assert.Equal(t, "0", ueUnobligated(t, ct, 4),
		"a QUEUED withdrawal is still owed to the staker — reporting it as unobligated "+
			"lets the airdrop and the sweep spend Alice's principal while she waits out "+
			"the cooldown she was forced to serve")
}

// What that costs, end to end: the owner sweeps what the contract calls free float,
// and Alice can no longer be paid.
//
// sweepUnobligated's own comment claims it "cannot reach anybody's stake or anybody's
// unclaimed reward". This is that claim, tested.
func TestUnstakeEnvelope_SweepCannotTakeAQueuedWithdrawal(t *testing.T) {
	ct := mgSetup(t, "")
	mgFund(t, ct, "hive:alice", "1000", 1)
	call(t, ct, mgC1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)
	call(t, ct, mgC1, "unstake", `{"amount":"1000"}`, "hive:alice", 3, true)

	// The owner sweeps leftover float to the treasury — an ordinary post-migration
	// housekeeping call, with no intent to touch anyone's stake.
	call(t, ct, mgC1, "sweepUnobligated", ``, owner, 4, false)

	// cooldown is 15, so the queued entry matures at height 18
	r := call(t, ct, mgC1, "claimUnstaked", ``, "hive:alice", 20, true)
	assert.Contains(t, r.Ret, `"claimed":"1000"`,
		"Alice must get her principal back; if the sweep took it, the transfer aborts "+
			"and she can never be paid without someone refunding the contract")

	bal := call(t, ct, tokenID, "balanceOf", `{"account":"hive:alice"}`, "hive:probe", 21, true)
	assert.Contains(t, bal.Ret, `"1000"`, "Alice's tokens must actually have arrived")
}

// The same money, reached by the other spender.
func TestUnstakeEnvelope_AirdropCannotSpendAQueuedWithdrawal(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000"`)
	mgFund(t, ct, "hive:alice", "1000", 1)
	call(t, ct, mgC1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)
	call(t, ct, mgC1, "unstake", `{"amount":"1000"}`, "hive:alice", 3, true)

	// An airdrop batch sized to exactly the "free float" the contract believes it has.
	r := call(t, ct, mgC1, "airdropBatch",
		`{"batchId":"hive:launch1","entries":"hive:bob:1000"}`, owner, 4, false)
	assert.Contains(t, r.Ret+r.ErrMsg, "unobligated",
		"the batch must be refused by the envelope check, naming it as the reason")

	r = call(t, ct, mgC1, "claimUnstaked", ``, "hive:alice", 20, true)
	assert.Contains(t, r.Ret, `"claimed":"1000"`, "Alice's principal must survive the airdrop")
}

// The envelope must come back DOWN when the withdrawal is actually paid, or the
// contract slowly freezes float that genuinely is free — the mirror mistake, and the
// one a fix is most likely to introduce.
func TestUnstakeEnvelope_PaidWithdrawalStopsBeingAnObligation(t *testing.T) {
	ct := mgSetup(t, `,"maxAirdrop":"1000000"`)
	mgFund(t, ct, "hive:alice", "1000", 1)
	call(t, ct, mgC1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)
	call(t, ct, mgC1, "unstake", `{"amount":"400"}`, "hive:alice", 3, true)

	// Genuinely free float: transferred in, owed to nobody.
	call(t, ct, tokenID, "mint", `{"amount":"500"}`, owner, 4, true)
	call(t, ct, tokenID, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"500"}`, mgC1), owner, 4, true)
	assert.Equal(t, "500", ueUnobligated(t, ct, 5),
		"the float transferred in is free; the 400 queued and the 600 still staked are not")

	call(t, ct, mgC1, "claimUnstaked", ``, "hive:alice", 20, true)
	assert.Equal(t, "500", ueUnobligated(t, ct, 21),
		"once the withdrawal is paid it must stop being an obligation — otherwise the "+
			"reserve only ever grows and real float becomes permanently unsweepable")
}
