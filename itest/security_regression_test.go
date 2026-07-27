package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Security regression tests — each asserts the FIXED behaviour for an exploit
// found during the 3-round adversarial review. A failure here means a fix regressed.

const (
	rgC1 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	rgC7 = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	rgC3 = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
)

// R3/HIGH-1: a 1-block boundary joiner must NOT dilute full-epoch stakers.
// (Regression from the R1 flash-stake fix: numerator used min(start,end) while the
// denominator used totalAt(hEnd), letting anyone burn ~90% of an epoch's yield.)
func TestSec_BoundaryJoinerCannotDilute(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(rgC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rgC7, owner, read("../c7-yield/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, rgC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":""}`, tokenID), owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, tokenID, rgC7), owner, 0, true)
	call(t, &ct, rgC7, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","genesis":"0","epochLen":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID, rgC1), owner, 0, true)

	call(t, &ct, tokenID, "mint", `{"amount":"10000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:mallory","amount":"5400"}`, owner, 0, true)

	// alice stakes for the WHOLE epoch 0 (h=0)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, rgC1), "hive:alice", 0, true)
	call(t, &ct, rgC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	// mallory joins only at the epoch-end boundary (h=1)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"5400"}`, rgC1), "hive:mallory", 1, true)
	call(t, &ct, rgC1, "stake", `{"amount":"5400"}`, "hive:mallory", 1, true)

	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 2, true)
	call(t, &ct, rgC7, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 2, true)

	// mallory must be rejected; alice must receive the FULL bucket (no dilution).
	call(t, &ct, rgC7, "claim", `{"epoch":"0"}`, "hive:mallory", 2, false)
	a := call(t, &ct, rgC7, "claim", `{"epoch":"0"}`, "hive:alice", 2, true)
	assert.Contains(t, a.Ret, `"100000"`, "full-epoch staker must not be diluted by a boundary joiner")
}

// R3/MED-1: a >uint64 epoch string must be rejected (it used to alias onto epoch 0
// in C2.claimBucket and strand the real epoch).
func TestSec_EpochAliasRejected(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, rgC3), owner, 0, true)
	call(t, &ct, rgC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)

	// overflow / non-canonical epochs are rejected outright
	call(t, &ct, rgC3, "pullFunding", `{"epoch":"18446744073709551616"}`, "hive:attacker", 1, false)
	call(t, &ct, rgC3, "pullFunding", `{"epoch":"00"}`, "hive:attacker", 1, false)
	// the honest canonical path still works and gets the full amount
	r := call(t, &ct, rgC3, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 1, true)
	assert.Contains(t, r.Ret, `"100000"`)
}

// R3/MED-2: a reporter that never finalizes must not strand funds forever —
// the guardian can rescue a stale funded epoch and sweep it to the pinned treasury.
func TestSec_StaleEpochRescuable(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, rgC3), owner, 0, true)
	call(t, &ct, rgC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, rgC3, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 1, true)

	// reporter goes silent: too early to rescue...
	call(t, &ct, rgC3, "cancelEpoch", `{"epoch":"0"}`, "hive:guardian", 2, false)
	// ...but after the staleness window the guardian can rescue + sweep to treasury
	call(t, &ct, rgC3, "cancelEpoch", `{"epoch":"0"}`, "hive:guardian", 9999, true)
	s := call(t, &ct, rgC3, "sweepUnallocated", `{"nonce":"1"}`, "hive:guardian", 9999, true)
	assert.Contains(t, s.Ret, `"100000"`)
	b := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:treasury"}`, "hive:x", 9999, true)
	assert.Contains(t, b.Ret, `"100000"`, "rescued funds must land in the PINNED treasury")
}

// R2/MED-3: window must be > 0, else the guardian veto is silently disabled.
func TestSec_ZeroWindowRejected(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, rgC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"0","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, false)
}

// R2/HIGH-3: reporter and guardian authority sets must be disjoint.
func TestSec_ReporterGuardianOverlapRejected(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, rgC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:same","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:same","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, false)
}

// R3/HIGH-1: the stale-rescue must be measured from the EPOCH END with a grace of
// >= 2 epochs, so a griefer calling (permissionless) pullFunding cannot start a
// clock that lets the guardian divert an epoch the reporter is still working on.
func TestSec_StaleRescueCannotDivertLiveEpoch(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// realistic cadence: 100-block epochs => stale grace must be >= 200 blocks after epoch end
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"100","baseAnnual":"1000000","blocksPerYear":"10000","dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, rgC3), owner, 0, true)
	call(t, &ct, rgC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 200, true)
	// griefer front-runs the keeper to start any naive clock
	call(t, &ct, rgC3, "pullFunding", `{"epoch":"0"}`, "hive:griefer", 200, true)
	// epoch 0 ended at h=99; grace is >= 1000 → rescue must be refused well past
	// the old (buggy) 1000-blocks-after-pullFunding threshold
	call(t, &ct, rgC3, "cancelEpoch", `{"epoch":"0"}`, "hive:guardian", 1050, false)
	// the reporter can still do its job on the live epoch
	call(t, &ct, rgC3, "submitShares", `{"epoch":"0","page":"0","entries":"hive:alice:1"}`, "hive:reporter", 1050, true)
	call(t, &ct, rgC3, "finalizeEpoch", `{"epoch":"0"}`, "hive:reporter", 1050, true)
}

// FINAL/HIGH-1: C7 sweepResidual must not steal still-claimable yield. Claims close
// at the same deadline the sweep opens, so swept funds are genuinely unclaimable
// and Σclaims ≤ funded holds.
func TestSec_C7SweepCannotStealClaimableYield(t *testing.T) {
	const fC1, fC7 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9", "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(fC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(fC7, owner, read("../c7-yield/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, fC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":""}`, tokenID), owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, tokenID, fC7), owner, 0, true)
	call(t, &ct, fC7, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","genesis":"0","epochLen":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID, fC1), owner, 0, true)

	call(t, &ct, tokenID, "mint", `{"amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, fC1), "hive:alice", 0, true)
	call(t, &ct, fC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 2, true)
	call(t, &ct, fC7, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 2, true)

	// too early to sweep — the yield is still claimable
	call(t, &ct, fC7, "sweepResidual", `{"epoch":"0"}`, "hive:guardian", 2, false)
	// after the deadline the guardian may sweep the (now unclaimable) residual...
	call(t, &ct, fC7, "sweepResidual", `{"epoch":"0"}`, "hive:guardian", 5000, true)
	// ...and a late claimer must NOT also be paid (would break Σclaims ≤ funded)
	call(t, &ct, fC7, "claim", `{"epoch":"0"}`, "hive:alice", 5001, false)
}

// FINAL/HIGH-2: a guardian must not escape a spent veto by re-queueing the same op.
func TestSec_CannotRequeueToEscapeVeto(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(rgC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, rgC3), owner, 0, true)

	op := `{"op":"changeOwner","nonce":"1","newOwner":"hive:evil"}`
	call(t, &ct, c2ID, "queueTokenOp", op, "hive:guardian", 10, true)
	// a queued op cannot be re-queued (which previously reset the timelock and
	// inherited a spent cancellation)
	call(t, &ct, c2ID, "queueTokenOp", op, "hive:guardian", 11, false)
	// the VETO (not the guardian) cancels it
	call(t, &ct, c2ID, "cancelTokenOp", op, "hive:guardian", 11, false)
	call(t, &ct, c2ID, "cancelTokenOp", op, "hive:veto", 11, true)
	// after cancellation the op is gone and cannot be executed
	call(t, &ct, c2ID, "executeTokenOp", op, "hive:anyone", 100, false)
	// re-queueing is allowed again, but the veto can cancel the NEW instance too
	call(t, &ct, c2ID, "queueTokenOp", op, "hive:guardian", 101, true)
	call(t, &ct, c2ID, "cancelTokenOp", op, "hive:veto", 102, true)
	call(t, &ct, c2ID, "executeTokenOp", op, "hive:anyone", 200, false)
}

// FINAL/MED: keeper lag must not strand an epoch. The claim window is anchored to
// max(epoch end, funding arrival) + grace, so a late (permissionless) keeper poke
// cannot deliver funds into an already-closed window and hand them to the sweep.
func TestSec_KeeperLagCannotStrandEpoch(t *testing.T) {
	const kC1, kC7 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9", "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(kC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(kC7, owner, read("../c7-yield/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, kC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":""}`, tokenID), owner, 0, true)
	call(t, &ct, c2ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, tokenID, kC7), owner, 0, true)
	call(t, &ct, kC7, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","genesis":"0","epochLen":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID, kC1), owner, 0, true)

	call(t, &ct, tokenID, "mint", `{"amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, kC1), "hive:alice", 0, true)
	call(t, &ct, kC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	// keeper is VERY late — epoch 0 ended at h=0, funding only arrives at h=2000,
	// far beyond hEnd+grace(1000)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 2000, true)
	call(t, &ct, kC7, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 2000, true)
	// the sweep must NOT be open yet, and alice must still be able to claim
	call(t, &ct, kC7, "sweepResidual", `{"epoch":"0"}`, "hive:guardian", 2000, false)
	a := call(t, &ct, kC7, "claim", `{"epoch":"0"}`, "hive:alice", 2000, true)
	assert.Contains(t, a.Ret, `"100000"`, "late-funded epoch must still be claimable")
}
