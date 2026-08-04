package itest_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Coverage tests for the previously-untested C1 unstake/cooldown path (user funds!)
// and the C7 two-point (min-over-epoch) yield rule with a REAL epochLen > 1.
//
// Everything here is read-only w.r.t. the contracts: no contract source is touched.

const (
	c17C1    = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	c17C2    = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	c17Owner = "hive:tibfox"
)

// ---- small local helpers (unique names) ----------------------------------

// c17Field pulls a flat field out of a contract return value.
func c17Field(t *testing.T, ret, name string) string {
	t.Helper()
	s := ret
	for i := 0; i < 3; i++ {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err != nil {
			break
		}
		s = inner
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("c17Field: ret %q is not an object: %v", ret, err)
	}
	raw, ok := m[name]
	if !ok {
		t.Fatalf("c17Field: field %q missing from %q", name, ret)
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return string(raw)
}

func c17Int(t *testing.T, ret, name string) *big.Int {
	t.Helper()
	v := c17Field(t, ret, name)
	n, ok := new(big.Int).SetString(v, 10)
	if !ok {
		t.Fatalf("c17Int: %q (field %q) is not a decimal integer", v, name)
	}
	return n
}

func c17I64(t *testing.T, ret, name string) int64 {
	t.Helper()
	return c17Int(t, ret, name).Int64()
}

func c17TokenBal(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) int64 {
	t.Helper()
	r := call(t, ct, tokenID, "balanceOf", fmt.Sprintf(`{"account":"%s"}`, acct), "hive:probe", h, true)
	return c17I64(t, r.Ret, "balance")
}

func c17StakeOf(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) int64 {
	t.Helper()
	r := call(t, ct, c17C1, "stakeOf", fmt.Sprintf(`{"account":"%s"}`, acct), "hive:probe", h, true)
	return c17I64(t, r.Ret, "stake")
}

func c17TotalStaked(t *testing.T, ct *test_utils.ContractTest, h uint64) int64 {
	t.Helper()
	r := call(t, ct, c17C1, "totalStaked", ``, "hive:probe", h, true)
	return c17I64(t, r.Ret, "total")
}

func c17StakeAt(t *testing.T, ct *test_utils.ContractTest, acct string, at, h uint64) int64 {
	t.Helper()
	r := call(t, ct, c17C1, "stakeAtHeight",
		fmt.Sprintf(`{"account":"%s","height":"%d"}`, acct, at), "hive:probe", h, true)
	return c17I64(t, r.Ret, "stake")
}

func c17TotalAt(t *testing.T, ct *test_utils.ContractTest, at, h uint64) int64 {
	t.Helper()
	r := call(t, ct, c17C1, "totalStakedAtHeight",
		fmt.Sprintf(`{"height":"%d"}`, at), "hive:probe", h, true)
	return c17I64(t, r.Ret, "total")
}

// c17NewTest boots a fresh engine with the token registered + initialised.
func c17NewTest(t *testing.T, extra map[string]string) test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, c17Owner, read(tokenWasmPath))
	for id, path := range extra {
		ct.RegisterContract(id, c17Owner, read(path))
	}
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, c17Owner, 0, true)
	return ct
}

// c17FundAccount mints to the owner and forwards `amt` to `acct`, then has `acct`
// approve C1 as spender for `approve` units.
func c17FundAccount(t *testing.T, ct *test_utils.ContractTest, acct string, amt, approve int64, h uint64) {
	t.Helper()
	call(t, ct, tokenID, "mint", fmt.Sprintf(`{"amount":"%d"}`, amt), c17Owner, h, true)
	call(t, ct, tokenID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"%d"}`, acct, amt), c17Owner, h, true)
	if approve > 0 {
		call(t, ct, tokenID, "approve",
			fmt.Sprintf(`{"spender":"contract:%s","amount":"%d"}`, c17C1, approve), acct, h, true)
	}
}

const c17C1Only = "../c1-staking/artifacts/main.wasm"

// =========================================================================
// C1 — unstake / cooldown / claim
// =========================================================================

// Partial unstake reduces stake AND total immediately; full unstake empties the
// position; the queued amounts drain FIFO and only after the cooldown matures.
func TestCovStake_UnstakeCooldownAndFIFOClaim(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, tokenID), c17Owner, 0, true)

	c17FundAccount(t, &ct, "hive:alice", 1000, 1000, 0)
	call(t, &ct, c17C1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)
	assert.EqualValues(t, 1000, c17StakeOf(t, &ct, "hive:alice", 1))
	assert.EqualValues(t, 1000, c17TotalStaked(t, &ct, 1))
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "contract:"+c17C1, 1))
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:alice", 1))

	// --- partial unstake at h=2 (matures at 2+5=7) --------------------------
	call(t, &ct, c17C1, "unstake", `{"amount":"300"}`, "hive:alice", 2, true)
	assert.EqualValues(t, 700, c17StakeOf(t, &ct, "hive:alice", 2), "partial unstake must reduce stake immediately")
	assert.EqualValues(t, 700, c17TotalStaked(t, &ct, 2), "partial unstake must reduce total immediately")
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "contract:"+c17C1, 2), "tokens stay in custody during cooldown")
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:alice", 2), "no payout before the cooldown matures")

	// --- full unstake of the remainder at h=3 (matures at 8) ---------------
	call(t, &ct, c17C1, "unstake", `{"amount":"700"}`, "hive:alice", 3, true)
	assert.EqualValues(t, 0, c17StakeOf(t, &ct, "hive:alice", 3))
	assert.EqualValues(t, 0, c17TotalStaked(t, &ct, 3))

	// --- unstaking more than staked must fail ------------------------------
	call(t, &ct, c17C1, "unstake", `{"amount":"1"}`, "hive:alice", 4, false)

	// --- claim BEFORE maturity pays nothing --------------------------------
	r := call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 4, true)
	assert.EqualValues(t, 0, c17I64(t, r.Ret, "claimed"), "claim before cooldown must pay nothing")
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:alice", 4))
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "contract:"+c17C1, 4))
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 6, true)
	assert.EqualValues(t, 0, c17I64(t, r.Ret, "claimed"), "one block short of maturity must still pay nothing")

	// --- FIFO: at h=7 only the FIRST entry (300) is mature ------------------
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 7, true)
	assert.EqualValues(t, 300, c17I64(t, r.Ret, "claimed"), "exactly the matured entry, FIFO")
	assert.EqualValues(t, 300, c17TokenBal(t, &ct, "hive:alice", 7))
	assert.EqualValues(t, 700, c17TokenBal(t, &ct, "contract:"+c17C1, 7))

	// --- at h=8 the second entry matures -----------------------------------
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 8, true)
	assert.EqualValues(t, 700, c17I64(t, r.Ret, "claimed"))
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "hive:alice", 8), "all principal returned, nothing extra")
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "contract:"+c17C1, 8))

	// --- queue is drained: further claims are no-ops -----------------------
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 9, true)
	assert.EqualValues(t, 0, c17I64(t, r.Ret, "claimed"), "drained queue must not pay twice")
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "hive:alice", 9))
}

// Staking requires a prior (and sufficient) allowance on the token.
func TestCovStake_StakeRequiresApproval(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, tokenID), c17Owner, 0, true)

	// funded but NOT approved
	c17FundAccount(t, &ct, "hive:alice", 1000, 0, 0)
	call(t, &ct, c17C1, "stake", `{"amount":"100"}`, "hive:alice", 1, false)
	assert.EqualValues(t, 0, c17TotalStaked(t, &ct, 1))
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "hive:alice", 1), "failed stake must not move tokens")

	// approving less than the stake still fails
	call(t, &ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"100"}`, c17C1), "hive:alice", 1, true)
	call(t, &ct, c17C1, "stake", `{"amount":"200"}`, "hive:alice", 1, false)
	assert.EqualValues(t, 0, c17TotalStaked(t, &ct, 1))

	// exactly the approved amount works
	call(t, &ct, c17C1, "stake", `{"amount":"100"}`, "hive:alice", 1, true)
	assert.EqualValues(t, 100, c17StakeOf(t, &ct, "hive:alice", 1))
	assert.EqualValues(t, 900, c17TokenBal(t, &ct, "hive:alice", 1))

	// allowance is now spent — a second stake of the same size fails again
	call(t, &ct, c17C1, "stake", `{"amount":"100"}`, "hive:alice", 2, false)
	assert.EqualValues(t, 100, c17TotalStaked(t, &ct, 2))
}

// stakeFor: only an allowlisted caller may use it, the TARGET is credited, and the
// tokens come out of the CALLER's balance (R7 conservation).
func TestCovStake_StakeForAllowlistAndCustody(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"hive:operator"}`, tokenID),
		c17Owner, 0, true)

	c17FundAccount(t, &ct, "hive:operator", 500, 500, 0)
	c17FundAccount(t, &ct, "hive:eve", 500, 500, 0)
	c17FundAccount(t, &ct, "hive:carol", 70, 0, 0)

	// allowlisted operator stakes on carol's behalf
	call(t, &ct, c17C1, "stakeFor", `{"acct":"hive:carol","amount":"500"}`, "hive:operator", 1, true)
	assert.EqualValues(t, 500, c17StakeOf(t, &ct, "hive:carol", 1), "target must be credited")
	assert.EqualValues(t, 0, c17StakeOf(t, &ct, "hive:operator", 1), "caller must NOT be credited")
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:operator", 1), "tokens must be pulled from the CALLER")
	assert.EqualValues(t, 70, c17TokenBal(t, &ct, "hive:carol", 1), "target's own liquid balance is untouched")
	assert.EqualValues(t, 500, c17TokenBal(t, &ct, "contract:"+c17C1, 1))
	assert.EqualValues(t, 500, c17TotalStaked(t, &ct, 1))

	// eve is funded + approved but not allowlisted → rejected
	call(t, &ct, c17C1, "stakeFor", `{"acct":"hive:eve","amount":"100"}`, "hive:eve", 2, false)
	call(t, &ct, c17C1, "stakeFor", `{"acct":"hive:carol","amount":"100"}`, "hive:eve", 2, false)
	assert.EqualValues(t, 500, c17TotalStaked(t, &ct, 2), "rejected stakeFor must not change custody")
	assert.EqualValues(t, 500, c17TokenBal(t, &ct, "hive:eve", 2))

	// carol owns the position she was credited with: she can unstake + claim it
	call(t, &ct, c17C1, "unstake", `{"amount":"500"}`, "hive:carol", 3, true)
	r := call(t, &ct, c17C1, "claimUnstaked", ``, "hive:carol", 8, true)
	assert.EqualValues(t, 500, c17I64(t, r.Ret, "claimed"))
	assert.EqualValues(t, 570, c17TokenBal(t, &ct, "hive:carol", 8))
}

// Height-indexed queries: before any stake, between checkpoints, and multiple
// mutations inside the SAME block (the search must return the LAST value at h).
func TestCovStake_HeightQueries(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, tokenID), c17Owner, 0, true)

	c17FundAccount(t, &ct, "hive:alice", 1000, 1000, 0)
	c17FundAccount(t, &ct, "hive:bob", 1000, 1000, 0)

	// two alice stakes in the SAME block h=5 → 100 then 150
	call(t, &ct, c17C1, "stake", `{"amount":"100"}`, "hive:alice", 5, true)
	call(t, &ct, c17C1, "stake", `{"amount":"50"}`, "hive:alice", 5, true)
	// bob stakes and alice unstakes in the SAME block h=10
	call(t, &ct, c17C1, "stake", `{"amount":"200"}`, "hive:bob", 10, true)
	call(t, &ct, c17C1, "unstake", `{"amount":"30"}`, "hive:alice", 10, true)

	const now = 50
	// before ANY stake
	assert.EqualValues(t, 0, c17StakeAt(t, &ct, "hive:alice", 0, now), "height before the first stake must be 0")
	assert.EqualValues(t, 0, c17StakeAt(t, &ct, "hive:alice", 4, now))
	assert.EqualValues(t, 0, c17TotalAt(t, &ct, 4, now))
	assert.EqualValues(t, 0, c17StakeAt(t, &ct, "hive:bob", 9, now), "bob had no stake before h=10")

	// same-block: must be the LATEST value written at that height
	assert.EqualValues(t, 150, c17StakeAt(t, &ct, "hive:alice", 5, now), "same-block: last write wins")
	assert.EqualValues(t, 150, c17TotalAt(t, &ct, 5, now))

	// between checkpoints
	assert.EqualValues(t, 150, c17StakeAt(t, &ct, "hive:alice", 7, now), "value carries forward between checkpoints")
	assert.EqualValues(t, 150, c17TotalAt(t, &ct, 9, now))

	// same-block again, two different accounts mutating
	assert.EqualValues(t, 120, c17StakeAt(t, &ct, "hive:alice", 10, now))
	assert.EqualValues(t, 200, c17StakeAt(t, &ct, "hive:bob", 10, now))
	assert.EqualValues(t, 320, c17TotalAt(t, &ct, 10, now), "150+200-30")

	// after the last checkpoint
	assert.EqualValues(t, 120, c17StakeAt(t, &ct, "hive:alice", 9999, now))
	assert.EqualValues(t, 320, c17TotalAt(t, &ct, 9999, now))

	// live queries agree with the latest checkpoint
	assert.EqualValues(t, 120, c17StakeOf(t, &ct, "hive:alice", now))
	assert.EqualValues(t, 320, c17TotalStaked(t, &ct, now))
}

// R15: cooldown must strictly exceed epochLen, else a 1-block stake could capture a
// whole epoch of C7 yield and leave before the cooldown bites.
func TestCovStake_InitRejectsShortCooldown(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	// cooldown == epochLen → rejected
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"10","epochLen":"10","allow":""}`, tokenID), c17Owner, 0, false)
	// cooldown < epochLen → rejected
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"10","allow":""}`, tokenID), c17Owner, 0, false)
	// the contract must still be uninitialised
	call(t, &ct, c17C1, "totalStaked", ``, "hive:probe", 0, false)
	// cooldown > epochLen → accepted
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"11","epochLen":"10","allow":""}`, tokenID), c17Owner, 0, true)
	assert.EqualValues(t, 0, c17TotalStaked(t, &ct, 0))
	// and it is immutable afterwards
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"99","epochLen":"10","allow":""}`, tokenID), c17Owner, 0, false)
}

// CRITICAL INVARIANT: after a mixed stake/stakeFor/unstake/claimUnstaked sequence,
//
//	Σ(per-account stake) == totalStaked, and
//	C1's token balance   == totalStaked + still-queued (unclaimed) unstake amounts.
func TestCovStake_ConservationAfterMixedSequence(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"hive:operator"}`, tokenID),
		c17Owner, 0, true)

	c17FundAccount(t, &ct, "hive:alice", 1200, 1200, 0)
	c17FundAccount(t, &ct, "hive:bob", 500, 500, 0)
	c17FundAccount(t, &ct, "hive:operator", 300, 300, 0)

	call(t, &ct, c17C1, "stake", `{"amount":"1000"}`, "hive:alice", 1, true)
	call(t, &ct, c17C1, "stake", `{"amount":"500"}`, "hive:bob", 1, true)
	call(t, &ct, c17C1, "stakeFor", `{"acct":"hive:carol","amount":"300"}`, "hive:operator", 2, true)
	call(t, &ct, c17C1, "unstake", `{"amount":"400"}`, "hive:alice", 3, true) // ready h=8
	call(t, &ct, c17C1, "unstake", `{"amount":"500"}`, "hive:bob", 3, true)   // ready h=8 (full exit)
	call(t, &ct, c17C1, "unstake", `{"amount":"100"}`, "hive:carol", 4, true) // ready h=9
	call(t, &ct, c17C1, "stake", `{"amount":"200"}`, "hive:alice", 5, true)

	// only alice claims so far
	r := call(t, &ct, c17C1, "claimUnstaked", ``, "hive:alice", 8, true)
	assert.EqualValues(t, 400, c17I64(t, r.Ret, "claimed"))

	// --- invariant #1: Σ stake == totalStaked ------------------------------
	a := c17StakeOf(t, &ct, "hive:alice", 8)
	b := c17StakeOf(t, &ct, "hive:bob", 8)
	c := c17StakeOf(t, &ct, "hive:carol", 8)
	o := c17StakeOf(t, &ct, "hive:operator", 8)
	total := c17TotalStaked(t, &ct, 8)
	assert.EqualValues(t, 800, a)
	assert.EqualValues(t, 0, b)
	assert.EqualValues(t, 200, c)
	assert.EqualValues(t, 0, o)
	assert.EqualValues(t, total, a+b+c+o, "Σ(per-account stake) must equal totalStaked")
	assert.EqualValues(t, 1000, total)

	// --- invariant #2: custody == totalStaked + still-queued unstakes -------
	const queued = 500 + 100 // bob's 500 + carol's 100, both unclaimed
	custody := c17TokenBal(t, &ct, "contract:"+c17C1, 8)
	assert.EqualValues(t, total+queued, custody,
		"C1 custody must equal totalStaked + queued-but-unclaimed unstakes")

	// drain the rest of the queue; custody must fall back to exactly totalStaked
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:bob", 8, true)
	assert.EqualValues(t, 500, c17I64(t, r.Ret, "claimed"))
	r = call(t, &ct, c17C1, "claimUnstaked", ``, "hive:carol", 9, true)
	assert.EqualValues(t, 100, c17I64(t, r.Ret, "claimed"))

	total = c17TotalStaked(t, &ct, 9)
	custody = c17TokenBal(t, &ct, "contract:"+c17C1, 9)
	assert.EqualValues(t, 1000, total)
	assert.EqualValues(t, total, custody, "with an empty queue, custody == totalStaked exactly")

	// no value was created or destroyed overall
	sum := custody +
		c17TokenBal(t, &ct, "hive:alice", 9) +
		c17TokenBal(t, &ct, "hive:bob", 9) +
		c17TokenBal(t, &ct, "hive:carol", 9) +
		c17TokenBal(t, &ct, "hive:operator", 9)
	assert.EqualValues(t, 2000, sum, "total minted (1200+500+300) must be conserved")
}

// =========================================================================
// C7 — yield with epochLen > 1 (two-point min-over-epoch rule)
// =========================================================================

// c17BootYield wires token + C1 + C2 + C7 with epochLen=10 (so hStart != hEnd) and
// an emission of exactly 100000 per epoch. C2 is initialised BEFORE C7 because C7
// cross-checks genesis/epochLen against C2.scheduleInfo.
func c17BootYield(t *testing.T) test_utils.ContractTest {
	t.Helper()
	ct := c17NewTest(t, map[string]string{
		c17C1: c17C1Only,
		c17C2: "../c2-emission/artifacts/main.wasm",
	})
	// cooldown (11) > epochLen (10), as R15 requires
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"11","epochLen":"10","allow":""}`, tokenID), c17Owner, 0, true)
	// baseAnnual*epochLen/blocksPerYear = 1000000*10/100 = 100000 per epoch
	fundC2Pool(t, &ct, tokenID, c17C2, "500000000", 0)
	call(t, &ct, c17C2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"yield","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`,
		tokenID, c17C1), c17Owner, 0, true)
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, c17C1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c17C2), c17Owner, 0, true)
	return ct
}

// Two full-epoch stakers split the epoch pro-rata; double claims, unfunded epochs
// and not-yet-elapsed epochs are all rejected. Σclaims must not exceed funded.
func TestCovStake_YieldProRataEpochLen10(t *testing.T) {
	ct := c17BootYield(t)

	c17FundAccount(t, &ct, "hive:alice", 600, 600, 0)
	c17FundAccount(t, &ct, "hive:bob", 400, 400, 0)
	call(t, &ct, c17C1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, &ct, c17C1, "stake", `{"amount":"400"}`, "hive:bob", 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c17C2), c17Owner, 0, true)

	// epoch 0 spans h=0..9 → first fully-elapsed at h=10
	call(t, &ct, c17C2, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, &ct, c17C1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)

	f := call(t, &ct, c17C1, "fundedOf", `{"epoch":"0"}`, "hive:probe", 10, true)
	funded := c17I64(t, f.Ret, "funded")
	assert.EqualValues(t, 100000, funded, "fundedOf must report the pulled slice")

	// the two-point rule is genuinely exercised: hStart=0, hEnd=9
	assert.EqualValues(t, 600, c17StakeAt(t, &ct, "hive:alice", 0, 10))
	assert.EqualValues(t, 600, c17StakeAt(t, &ct, "hive:alice", 9, 10))

	ca := call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true)
	cb := call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:bob", 11, true)
	pa, pb := c17I64(t, ca.Ret, "claimed"), c17I64(t, cb.Ret, "claimed")
	assert.EqualValues(t, 60000, pa)
	assert.EqualValues(t, 40000, pb)
	assert.LessOrEqual(t, pa+pb, funded, "Σclaims must not exceed funded")

	// double claim
	call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, false)
	// never-funded epoch
	call(t, &ct, c17C1, "claimYield", `{"epoch":"1"}`, "hive:alice", 11, false)
	f = call(t, &ct, c17C1, "fundedOf", `{"epoch":"1"}`, "hive:probe", 11, true)
	assert.EqualValues(t, 0, c17I64(t, f.Ret, "funded"))

	// payouts landed on-chain
	assert.EqualValues(t, 60000, c17TokenBal(t, &ct, "hive:alice", 11))
	assert.EqualValues(t, 40000, c17TokenBal(t, &ct, "hive:bob", 11))

	// --- epoch 1: fund it, then prove the "not fully elapsed" guard ---------
	call(t, &ct, c17C2, "distributeEpoch", ``, "hive:keeper", 20, true)
	call(t, &ct, c17C1, "pullFunding", `{"epoch":"1"}`, "hive:anyone", 20, true)
	f = call(t, &ct, c17C1, "fundedOf", `{"epoch":"1"}`, "hive:probe", 20, true)
	assert.EqualValues(t, 100000, c17I64(t, f.Ret, "funded"))
	// epoch 1 spans h=10..19; a claim evaluated at h=19 must be refused even
	// though the epoch is funded (harness re-evaluates at an earlier height).
	call(t, &ct, c17C1, "claimYield", `{"epoch":"1"}`, "hive:alice", 19, false)
	// at h=20 the epoch has fully elapsed and the claim goes through
	ca = call(t, &ct, c17C1, "claimYield", `{"epoch":"1"}`, "hive:alice", 20, true)
	assert.EqualValues(t, 60000, c17I64(t, ca.Ret, "claimed"))
}

// The min-over-epoch rule: a staker who leaves mid-epoch gets a REDUCED share, and
// a staker who joins mid-epoch gets NOTHING for that epoch.
func TestCovStake_YieldMinOverEpochRule(t *testing.T) {
	ct := c17BootYield(t)

	c17FundAccount(t, &ct, "hive:alice", 600, 600, 0)
	c17FundAccount(t, &ct, "hive:bob", 400, 400, 0)
	c17FundAccount(t, &ct, "hive:carol", 200, 200, 0)

	// alice + bob stake for the whole epoch start; carol is not in yet
	call(t, &ct, c17C1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, &ct, c17C1, "stake", `{"amount":"400"}`, "hive:bob", 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c17C2), c17Owner, 0, true)

	// mid-epoch (h=5): alice halves her position, carol joins
	call(t, &ct, c17C1, "unstake", `{"amount":"300"}`, "hive:alice", 5, true)
	call(t, &ct, c17C1, "stake", `{"amount":"200"}`, "hive:carol", 5, true)

	// snapshots the contract will read: hStart=0, hEnd=9
	assert.EqualValues(t, 600, c17StakeAt(t, &ct, "hive:alice", 0, 10))
	assert.EqualValues(t, 300, c17StakeAt(t, &ct, "hive:alice", 9, 10))
	assert.EqualValues(t, 0, c17StakeAt(t, &ct, "hive:carol", 0, 10))
	assert.EqualValues(t, 200, c17StakeAt(t, &ct, "hive:carol", 9, 10))
	assert.EqualValues(t, 1000, c17TotalAt(t, &ct, 0, 10))
	assert.EqualValues(t, 900, c17TotalAt(t, &ct, 9, 10)) // 300+400+200

	call(t, &ct, c17C2, "distributeEpoch", ``, "hive:keeper", 10, true)
	call(t, &ct, c17C1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 10, true)
	f := call(t, &ct, c17C1, "fundedOf", `{"epoch":"0"}`, "hive:probe", 10, true)
	funded := c17I64(t, f.Ret, "funded")
	assert.EqualValues(t, 100000, funded)

	// THE EXACT DENOMINATOR: Σ min(aᵢ,bᵢ) = alice 300 + bob 400 + carol 0 = 700, which
	// C1 reports as totalAt(0) − drawdown = 1000 − 300.
	//
	// This used to divide by min(totalAt(0), totalAt(9)) = 900 — the closest figure C7
	// could compute unaided — which paid out only 77,777 of 100,000 and left 22,223
	// belonging to nobody. 22% of one epoch. Recovering that is what the guardian
	// sweep was for, and the sweep is why claims had to close after ~10 days.
	ca := call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:alice", 11, true)
	cb := call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:bob", 11, true)
	pa, pb := c17I64(t, ca.Ret, "claimed"), c17I64(t, cb.Ret, "claimed")
	assert.EqualValues(t, 42857, pa, "alice is paid on min(600,300)=300 / 700")
	assert.EqualValues(t, 57142, pb, "bob is paid on 400/700")
	assert.Less(t, pa, pb, "leaving mid-epoch must cost alice her lead over bob")

	// carol joined mid-epoch → min(0,200)=0 → nothing for this epoch
	call(t, &ct, c17C1, "claimYield", `{"epoch":"0"}`, "hive:carol", 11, false)

	assert.LessOrEqual(t, pa+pb, funded, "Σclaims must not exceed funded")
	// THE POINT OF THE WHOLE CHANGE: what stays behind is now per-claimant truncation
	// dust, not a structural hole. Two claimants ⇒ at most 2 units lost to integer
	// division. If this ever grows again, the deadline pressure comes back with it.
	assert.LessOrEqual(t, funded-(pa+pb), int64(2),
		"the epoch must be fully distributed apart from truncation dust")
	assert.EqualValues(t, 42857, c17TokenBal(t, &ct, "hive:alice", 11))
	assert.EqualValues(t, 57142, c17TokenBal(t, &ct, "hive:bob", 11))
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:carol", 11))

	// carol IS entitled for the NEXT epoch, which she holds start-to-end
	call(t, &ct, c17C2, "distributeEpoch", ``, "hive:keeper", 20, true)
	call(t, &ct, c17C1, "pullFunding", `{"epoch":"1"}`, "hive:anyone", 20, true)
	cc := call(t, &ct, c17C1, "claimYield", `{"epoch":"1"}`, "hive:carol", 20, true)
	assert.EqualValues(t, 200*100000/900, c17I64(t, cc.Ret, "claimed"),
		"carol earns pro-rata once she is staked for a whole epoch")
}

// A Hive POSTING key must not be able to move someone's stake.
//
// Posting keys are routinely delegated to third-party front-ends — that is what they
// are for — and the VSC runtime derives msg.caller from RequiredPostingAuths[0] when
// a tx carries no active auth. C1 custody is the ONLY layer that can stop a leaked
// posting key reaching staked funds: the token contract authorizes transfers on
// msg.caller alone, so once tokens are liquid they are gone. Before this guard,
// unstake + claimUnstaked was a complete drain path.
func TestCovStake_PostingKeyCannotMoveStake(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, tokenID), c17Owner, 0, true)
	c17FundAccount(t, &ct, "hive:victim", 1000, 1000, 0)

	// posting-only STAKE is refused, and moves nothing
	pvCallPosting(t, &ct, c17C1, "stake", `{"amount":"1000"}`, "hive:victim", 1, false)
	assert.EqualValues(t, 0, c17StakeOf(t, &ct, "hive:victim", 1))
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "hive:victim", 1), "tokens must not have left the account")

	// with an ACTIVE auth the same call works
	call(t, &ct, c17C1, "stake", `{"amount":"1000"}`, "hive:victim", 2, true)
	assert.EqualValues(t, 1000, c17StakeOf(t, &ct, "hive:victim", 2))

	// posting-only UNSTAKE is refused, and the position is untouched
	pvCallPosting(t, &ct, c17C1, "unstake", `{"amount":"1000"}`, "hive:victim", 3, false)
	assert.EqualValues(t, 1000, c17StakeOf(t, &ct, "hive:victim", 3), "stake must survive a posting-key unstake")
	assert.EqualValues(t, 1000, c17TotalStaked(t, &ct, 3))

	// posting-only CLAIMUNSTAKED is refused too — the second half of the drain
	call(t, &ct, c17C1, "unstake", `{"amount":"1000"}`, "hive:victim", 4, true)
	pvCallPosting(t, &ct, c17C1, "claimUnstaked", `{}`, "hive:victim", 20, false)
	assert.EqualValues(t, 0, c17TokenBal(t, &ct, "hive:victim", 20),
		"a posting key must not be able to land unstaked tokens in the liquid balance")

	// the owner, with an active auth, still can
	call(t, &ct, c17C1, "claimUnstaked", `{}`, "hive:victim", 21, true)
	assert.EqualValues(t, 1000, c17TokenBal(t, &ct, "hive:victim", 21))
}

// The guard exempts contract: callers, and it MUST. On a nested call the runtime sets
// Caller to "contract:<id>" while forwarding the outer tx's required_auths verbatim,
// so a contract can never appear in that list. An unconditional RequireActive would
// therefore let a contract stake and then never unstake — its position stranded
// forever. This test fails the moment someone "simplifies" the helper.
func TestCovStake_ContractCallerCanStillUnstake(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":""}`, tokenID), c17Owner, 0, true)

	// A contract holds tokens and approves C1, exactly as a hive account would. It must
	// be a DIFFERENT id from C1 itself — the token refuses a self-approval, and since
	// yield merged into C1 the old stand-in id is now this very contract.
	holder := "contract:" + c17C2 // any other contract id; it never executes here
	call(t, &ct, tokenID, "mint", `{"amount":"500"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", fmt.Sprintf(`{"to":"%s","amount":"500"}`, holder), owner, 0, true)
	call(t, &ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"500"}`, c17C1), holder, 0, true)

	call(t, &ct, c17C1, "stake", `{"amount":"500"}`, holder, 1, true)
	assert.EqualValues(t, 500, c17StakeOf(t, &ct, holder, 1))

	// the point of the test: the contract can get its money back out
	call(t, &ct, c17C1, "unstake", `{"amount":"500"}`, holder, 2, true)
	assert.EqualValues(t, 0, c17StakeOf(t, &ct, holder, 2), "contract stake must not be strandable")
	call(t, &ct, c17C1, "claimUnstaked", `{}`, holder, 10, true)
	assert.EqualValues(t, 500, c17TokenBal(t, &ct, holder, 10))
}

// R15 ("cooldown must exceed an epoch") is only as strong as the epochLen it was
// checked against, and that value is supplied by the caller. C7 already pulls
// scheduleInfo from its funder and rejects a mismatch; C1 had no such check, so a
// typo'd epochLen silently produced a WEAKER cooldown guarantee than the operator
// believed — R15 passing against a fictional epoch length.
func TestCovStake_EpochLenIsCrossCheckedAgainstTheFunder(t *testing.T) {
	ct := c17NewTest(t, map[string]string{
		c17C1: c17C1Only,
		c17C2: "../c2-emission/artifacts/main.wasm",
	})
	fundC2Pool(t, &ct, tokenID, c17C2, "1000000", 0)
	call(t, &ct, c17C2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"y","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:g","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:v","vetoThreshold":"1",`+
			`"buckets":"y:hive:ybucket:10000"}`, tokenID), c17Owner, 0, true)

	// epochLen 5 does not match the funder's 10 — reject, even though cooldown > 5
	// would satisfy R15 arithmetically against the wrong number
	call(t, &ct, c17C1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"7","epochLen":"5","funder":"%s","allow":""}`,
		tokenID, c17C2), c17Owner, 0, false)

	// the truthful epochLen is accepted, and R15 is then judged against it
	call(t, &ct, c17C1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","funder":"%s","allow":""}`,
		tokenID, c17C2), c17Owner, 0, true)
}

// A standalone C1 — staking deployed before any emission contract exists — must still
// init. The cross-check is opt-in precisely so that stays possible.
func TestCovStake_FunderIsOptional(t *testing.T) {
	ct := c17NewTest(t, map[string]string{c17C1: c17C1Only})
	call(t, &ct, c17C1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, tokenID),
		c17Owner, 0, true)
}

// R15's cooldown check happens inside C1 against an epochLen the OPERATOR supplies,
// and C1 cannot verify it at init: the deploy order puts C1 before C2, because stake
// must exist before C2's genesis or epoch 0's yield is funded-but-unclaimable.
// adoptSchedule is the first moment it can compare against the real schedule.
func TestCovStake_AdoptRejectsTheWrongSchedule(t *testing.T) {
	ct := c17NewTest(t, map[string]string{
		c17C1: c17C1Only,
		c17C2: "../c2-emission/artifacts/main.wasm",
	})
	fundC2Pool(t, &ct, tokenID, c17C2, "1000000", 0)
	call(t, &ct, c17C2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000",`+
			`"blocksPerYear":"100","dustBucket":"y","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"hive:g","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:v","vetoThreshold":"1",`+
			`"buckets":"y:contract:%s:10000"}`, tokenID, c17C1), c17Owner, 0, true)

	// C1 deployed against a DIFFERENT epoch length than the emission schedule. Its own
	// R15 check passed (cooldown 7 > epochLen 5), but against a fiction.
	call(t, &ct, c17C1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"7","epochLen":"5","allow":""}`, tokenID),
		c17Owner, 0, true)

	// Adoption must refuse: cooldown 7 does NOT exceed the real epoch of 10, so a
	// staker could capture a full epoch of yield and exit.
	call(t, &ct, c17C1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"y"}`, c17C2), c17Owner, 0, false)

	// The failed adoption leaves C1 with no schedule, so it can supply neither an R15
	// guarantee nor an exact yield denominator — and yield funding stays shut.
	call(t, &ct, c17C1, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 100, false)
}
