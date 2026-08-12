package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Two guards that close the same shape of hole: a rule that held only because of
// something located somewhere other than the operation it protects.

const (
	segTok = "vsc1Bd6ZgTRHZQyMXCFYnCbcZaipvHNPd9YHSC"
	segC1  = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	segC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
)

// segBoot brings up token + C1 + C2 with a yield bucket paying C1, one staker, and
// epoch 0 emitted. epochLen 4 so heights and epochs are not the same number.
func segBoot(t *testing.T, ct *test_utils.ContractTest) {
	t.Helper()
	ct.RegisterContract(segTok, owner, read(tokenWasmPath))
	ct.RegisterContract(segC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(segC2, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, ct, segTok, "init",
		`{"name":"S","symbol":"S","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, ct, segC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"4","allow":"","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, segTok), owner, 0, true)
	fundC2Pool(t, ct, segTok, segC2, "500000000", 0)
	call(t, ct, segC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"4","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
			`"vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, segTok, segC1), owner, 0, true)
	call(t, ct, segC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, segC2), owner, 0, true)

	// alice stakes across the whole of epoch 0 (blocks 0..3).
	call(t, ct, segTok, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, ct, segTok, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, ct, segTok, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, segC1), "hive:alice", 0, true)
	call(t, ct, segC1, "stake", `{"amount":"600"}`, "hive:alice", 0, true)

	// Emit epoch 0 and pull its yield into C1.
	call(t, ct, segC2, "distributeEpoch", ``, "hive:keeper", 8, true)
	call(t, ct, segC1, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 8, true)
}

// SEC-1. y_funded accumulates, but claimYield divides by whatever it holds at the
// moment of the claim — so a second tranche after the first claim would underpay
// early claimants against late ones out of one pool. Nothing on chain would report
// it: both claims "succeed".
//
// It was unreachable, but only via C2: claimBucket deletes the owed record before
// transferring and can never re-add it. The guarantee lived in a neighbouring
// contract's delete semantics rather than next to the division that depends on it.
// This asserts the local rule directly.
func TestSecEpoch_YieldFundingIsFinalOnceClaimed(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	segBoot(t, &ct)

	// alice claims epoch 0 — this is what makes the funding final.
	call(t, &ct, segC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 8, true)

	// Any further funding of that epoch must now be refused BY C1, before it calls
	// out to the funder at all.
	res := call(t, &ct, segC1, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 9, false)
	assert.Contains(t, res.ErrMsg+res.Ret, "funding is final",
		"a paid-out epoch must refuse further funding locally, not rely on C2 having "+
			"deleted its owed record")

	// An epoch nobody has claimed is NOT frozen — the guard keys on payment, not on
	// the epoch merely existing. Epoch 1 has funding available and no claims.
	call(t, &ct, segC2, "distributeEpoch", ``, "hive:keeper", 12, true)
	call(t, &ct, segC1, "pullFunding", `{"epoch":"1"}`, "hive:keeper", 12, true)
}

// SEC-1, the swept case: sweepEmptyEpoch sets y_paid to the full funded amount, so
// an epoch recovered as unclaimable is frozen by the same rule.
func TestSecEpoch_SweptEpochRefusesFurtherFunding(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })

	ct.RegisterContract(segTok, owner, read(tokenWasmPath))
	ct.RegisterContract(segC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(segC2, owner, read("../c2-emission/artifacts/main.wasm"))
	call(t, &ct, segTok, "init",
		`{"name":"S","symbol":"S","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, segC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"4","allow":"","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, segTok), owner, 0, true)
	fundC2Pool(t, &ct, segTok, segC2, "500000000", 0)
	call(t, &ct, segC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"4","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
			`"vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, segTok, segC1), owner, 0, true)
	call(t, &ct, segC1, "adoptSchedule",
		fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, segC2), owner, 0, true)

	// NOBODY stakes, so epoch 0's denominator is zero and its yield is unclaimable.
	call(t, &ct, segC2, "distributeEpoch", ``, "hive:keeper", 8, true)
	call(t, &ct, segC1, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 8, true)
	call(t, &ct, segC1, "sweepEmptyEpoch", `{"epoch":"0"}`, "hive:guardian", 8, true)

	res := call(t, &ct, segC1, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 9, false)
	assert.Contains(t, res.ErrMsg+res.Ret, "funding is final",
		"an epoch swept as unclaimable must not accept more funding it could never pay out")
}

// SEC-2. The wrap guard used to sit only on minStakeSum — the read-only query — while
// claimYield, sweepEmptyEpoch and minStakeSumAt did the same g + ep*el arithmetic on
// the same caller-supplied index with no check. A wrapped end height is small, so the
// "epoch fully elapsed" test passes and the binary search lands on a real checkpoint
// at a height belonging to a different epoch.
//
// Every one of those paths is gated behind funded > 0, so it was never reachable —
// the point is that the arithmetic now refuses on its own rather than depending on
// that. These assert the refusal is present on the paths that move tokens, not just
// on the one that reads.
func TestSecEpoch_WrappingEpochIndexIsRefusedOnEveryPath(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	segBoot(t, &ct)

	// An index whose (ep+1)*epochLen overflows uint64. Canonical, 19 digits or fewer,
	// so it passes every input validation the contracts apply.
	const huge = "4611686018427387904" // 2^62; *4 wraps

	// These two reach the arithmetic, so the guard itself is what refuses them.
	for _, tc := range []struct{ action, caller string }{
		{"sweepEmptyEpoch", "hive:guardian"},
		{"minStakeSum", "hive:any"},
	} {
		res := call(t, &ct, segC1, tc.action, `{"epoch":"`+huge+`"}`, tc.caller, 20, false)
		assert.Containsf(t, res.ErrMsg+res.Ret, "out of range",
			"%s must refuse an epoch index whose height arithmetic wraps, rather than "+
				"computing against a wrapped height", tc.action)
	}

	// claimYield refuses too, but for a DIFFERENT reason, and the distinction is the
	// whole point of the finding. Its `funded > 0` check runs before the height
	// arithmetic, so a bogus epoch never reaches epochBounds at all — the wrap was
	// unreachable there because of a precondition, not because of a bounds check.
	// That precondition is exactly what this guard stops relying on: it now holds
	// locally as well, so reordering or relaxing the funded gate later cannot
	// resurrect the hole. Assert the refusal, and record which layer produced it.
	res := call(t, &ct, segC1, "claimYield", `{"epoch":"`+huge+`"}`, "hive:alice", 20, false)
	assert.Contains(t, res.ErrMsg+res.Ret, "epoch not funded yet",
		"claimYield's funding precondition is expected to fire first; if this message "+
			"changes to 'out of range' the ordering moved and the bounds check is now "+
			"the outer guard, which is fine — update this expectation deliberately")

	// The guard must not have moved the legitimate boundary: epoch 0 is closed at
	// height 20 and still answers.
	call(t, &ct, segC1, "minStakeSum", `{"epoch":"0"}`, "hive:any", 20, true)
	call(t, &ct, segC1, "claimYield", `{"epoch":"0"}`, "hive:alice", 20, true)
}
