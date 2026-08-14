package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// SCOT's staked_reward_percentage: part of every claim arrives as STAKE rather than
// liquid tokens, tying an earner to the token instead of handing them something
// sellable the moment it lands.
//
// The split has to happen on-chain at claim, not in the reporter: the reporter emits
// one share list and never touches tokens, so the distributor is the only place the
// payout exists to be divided. That makes this a two-contract path — the distributor
// approves the staking contract and calls stakeFor, which PULLS what it was told
// about — and these tests cover the seam between them.

const (
	spDist = "vsc1BhQpMhK3T9Y5aRxBBLmvVpz1sgcJ98BQD"
	spC1   = "vsc1BwtsK1Y1eK16k5o4qejjHK4qJHVsbSKqxc"
)

// spSetup wires token + C2 + C1 + a distributor whose claims are split `bps` staked.
// The distributor is put in C1's stakeFor allowlist, which is what authorises a
// CONTRACT to credit stake — a contract carries no key and can never satisfy the
// active-authority check.
func spSetup(t *testing.T, bps string) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(spC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(spDist, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// C1 first: the distributor's init cross-checks the token against it, and the
	// allowlist naming the distributor is fixed here and never changes.
	call(t, &ct, spC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"contract:%s",`+
			`"treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1"}`, tokenID, spDist), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:10000"}`, tokenID, spDist), owner, 0, true)

	// Fund an outsider so the allowlist test isolates the ALLOWLIST. Without tokens
	// and an approval, stakeFor aborts in the token pull long before authorisation
	// is reached, and a test expecting "it fails" passes whether the allowlist works
	// or not — which is exactly what the first version of this file did.
	call(t, &ct, tokenID, "mint", `{"amount":"5000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:mallory","amount":"5000"}`, owner, 0, true)

	stakeCfg := ""
	if bps != "" {
		stakeCfg = fmt.Sprintf(`,"stakeContract":"%s","stakedBps":"%s"`, spC1, bps)
	}
	call(t, &ct, spDist, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"%s}`,
		tokenID, c2ID, stakeCfg), owner, 0, true)
	call(t, &ct, spDist, "addChannel", `{"channel":"content","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:creporter","reporterThreshold":"1","role":"content"}`,
		owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	return &ct
}

// spRunEpoch emits, funds, publishes a share book for one earner, finalizes, and
// returns the book so the caller can build that earner's proof.
func spRunEpoch(t *testing.T, ct *test_utils.ContractTest, earner string) *book {
	t.Helper()
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, ct, spDist, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:anyone", 1, true)
	b := publishEntries(t, ct, spDist, "content", "0",
		fmt.Sprintf("%s:100", earner), "hive:creporter", 1)
	call(t, ct, spDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, true)
	return b
}

// THE CORE CASE. Half the payout arrives as stake, half as liquid tokens, and the
// two halves sum to exactly what the claimant was owed — nothing is minted and
// nothing is lost between the two contracts.
func TestStakedPayout_SplitsBetweenLiquidAndStake(t *testing.T) {
	ct := spSetup(t, "5000") // 50%
	bk := spRunEpoch(t, ct, "hive:earner")

	r := call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"), "hive:earner", 3, true)
	total := c17I64(t, r.Ret, "claimed")
	liquid := c17I64(t, r.Ret, "liquid")
	staked := c17I64(t, r.Ret, "staked")

	assert.EqualValues(t, total, liquid+staked, "the split must conserve the payout")
	assert.EqualValues(t, total/2, staked, "50%% of the payout should be staked")

	// the stake is real, held BY the staking contract FOR the claimant
	st := call(t, ct, spC1, "stakeOf", `{"account":"hive:earner"}`, "hive:probe", 3, true)
	assert.Contains(t, st.Ret, fmt.Sprint(staked),
		"the staking contract must hold the staked half for the earner: "+st.Ret)

	// and the liquid half really landed in their wallet
	bal := call(t, ct, tokenID, "balanceOf", `{"account":"hive:earner"}`, "hive:probe", 3, true)
	assert.Contains(t, bal.Ret, fmt.Sprint(liquid),
		"the liquid half must be spendable: "+bal.Ret)
}

// Capability follows config. A distributor with no stakeContract pays entirely
// liquid — the setting is absent, not switched off, so nothing about the old
// behaviour changes for a pool that never asked for this.
func TestStakedPayout_UnconfiguredPaysFullyLiquid(t *testing.T) {
	ct := spSetup(t, "") // no stakeContract at all
	bk := spRunEpoch(t, ct, "hive:earner")

	r := call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"), "hive:earner", 3, true)
	assert.EqualValues(t, c17I64(t, r.Ret, "claimed"), c17I64(t, r.Ret, "liquid"),
		"with no stake contract the whole payout is liquid")
	assert.EqualValues(t, 0, c17I64(t, r.Ret, "staked"))
}

// 100% staked is a legitimate configuration and must not strand the payout: every
// unit goes to stake, none is lost to the liquid path.
func TestStakedPayout_FullyStakedLeavesNothingLiquid(t *testing.T) {
	ct := spSetup(t, "10000")
	bk := spRunEpoch(t, ct, "hive:earner")

	r := call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"), "hive:earner", 3, true)
	total := c17I64(t, r.Ret, "claimed")
	assert.EqualValues(t, 0, c17I64(t, r.Ret, "liquid"))
	assert.EqualValues(t, total, c17I64(t, r.Ret, "staked"))

	st := call(t, ct, spC1, "stakeOf", `{"account":"hive:earner"}`, "hive:probe", 3, true)
	assert.Contains(t, st.Ret, fmt.Sprint(total), "all of it must be staked: "+st.Ret)
}

// The staked half is REAL stake, subject to the same cooldown as anything the
// earner staked themselves. If it were credited without the usual lock it would be
// liquid tokens wearing a different name, and the setting would buy nothing.
func TestStakedPayout_StakedHalfIsSubjectToCooldown(t *testing.T) {
	ct := spSetup(t, "10000")
	bk := spRunEpoch(t, ct, "hive:earner")
	call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"), "hive:earner", 3, true)

	// unstaking starts a cooldown rather than paying out immediately
	call(t, ct, spC1, "unstake", `{"amount":"100"}`, "hive:earner", 4, true)
	// Cooldown is 5 blocks. Claiming early SUCCEEDS but releases nothing — the call
	// is a no-op rather than an abort, so the assertion is on the amount, not on
	// the call failing. Expecting an abort here passes for the wrong reason on any
	// contract that simply refuses to run.
	early := call(t, ct, spC1, "claimUnstaked", `{}`, "hive:earner", 5, true)
	assert.EqualValues(t, 0, c17I64(t, early.Ret, "claimed"),
		"nothing may be released before the cooldown elapses: "+early.Ret)
	// after it elapses the principal is releasable
	late := call(t, ct, spC1, "claimUnstaked", `{}`, "hive:earner", 12, true)
	assert.EqualValues(t, 100, c17I64(t, late.Ret, "claimed"),
		"the reward-staked principal unstakes like any other: "+late.Ret)
}

// An outsider must not be able to credit stake through the staking contract just
// because SOME contract is allowed to. The allowlist is per-caller and immutable.
func TestStakedPayout_OnlyTheAllowlistedContractMayCreditStake(t *testing.T) {
	ct := spSetup(t, "5000")
	// Mallory holds tokens and approves C1, so every prerequisite for a successful
	// stakeFor is met EXCEPT membership of the allowlist. That is what makes this
	// test about authorisation rather than about an empty wallet.
	call(t, ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"5000"}`, spC1), "hive:mallory", 2, true)
	call(t, ct, spC1, "stakeFor", `{"acct":"hive:mallory","amount":"1000"}`, "hive:mallory", 2, false)

	// and nothing was credited
	st := call(t, ct, spC1, "stakeOf", `{"account":"hive:mallory"}`, "hive:probe", 3, true)
	assert.NotContains(t, st.Ret, `"1000"`, "a refused stakeFor must credit nothing: "+st.Ret)
}

// What the staked split COSTS. A liquid claim is one token transfer; a staked claim
// adds an approve and a cross-contract stakeFor, and the claimant pays for both.
//
// Measured rather than asserted against a fixed number: the point is the DELTA and
// that it stays within a claimant's means, not a figure that would need editing
// every time the contracts change.
func TestRC_StakedClaimCost(t *testing.T) {
	liquid := func() int64 {
		ct := spSetup(t, "")
		bk := spRunEpoch(t, ct, "hive:earner")
		return call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"),
			"hive:earner", 3, true).RcUsed
	}()
	staked := func() int64 {
		ct := spSetup(t, "5000")
		bk := spRunEpoch(t, ct, "hive:earner")
		return call(t, ct, spDist, "claim", bk.claimFor(t, "content", "0", "hive:earner"),
			"hive:earner", 3, true).RcUsed
	}()
	t.Logf("claim RC: liquid=%d staked=%d (+%d)", liquid, staked, staked-liquid)

	if staked <= liquid {
		t.Fatalf("a staked claim does strictly more work, so it cannot cost less: "+
			"liquid=%d staked=%d", liquid, staked)
	}
	// A claim is the one call an ORDINARY holder makes, so it has to fit comfortably
	// inside the free tier — someone who has earned a reward may hold no HBD at all.
	if staked > 10000 {
		t.Fatalf("a staked claim at %d RC no longer fits the 10k free tier, so an "+
			"earner with no HBD could not collect their reward", staked)
	}
}
