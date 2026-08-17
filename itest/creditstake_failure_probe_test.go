package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// PROBE — what happens to a claimant when the cross-contract stakeFor FAILS.
//
// claim marks claimed| and paid| BEFORE moving anything (CEI), pays the liquid part
// with adapter.Transfer, then hands the staked part to C1 via approve + stakeFor.
// That last call is the only one in the contract whose result is DISCARDED:
//
//	sdk.ContractCall(sc, "stakeFor", ...)   // no result checked
//
// If a failing inner call does NOT unwind the outer transaction, the claimant has
// already been marked as paid, received only the liquid half, and lost the staked
// half — with a standing allowance left behind. If it DOES unwind, the whole claim
// reverts and the claimant can retry, which is the safe outcome.
//
// The engine returns a result.Err for a failed contracts.call rather than trapping
// the caller, so which of those two happens is not obvious from reading the code.
// This settles it by construction: C3 is configured to pay stake, but is NOT in
// C1's stakeFor allowlist, so stakeFor aborts for a reason unrelated to funds.
func TestProbe_ClaimWhenStakeForIsRefused(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(spC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(spDist, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// THE DIFFERENCE from spSetup: the allowlist names somebody else, so the
	// distributor's stakeFor will be refused on authorisation alone.
	call(t, &ct, spC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"contract:%s",`+
			`"treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:10000"}`, tokenID, spDist), owner, 0, true)
	call(t, &ct, spDist, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"stakeContract":"%s","stakedBps":"5000"}`, tokenID, c2ID, spC1), owner, 0, true)
	call(t, &ct, spDist, "addChannel", `{"channel":"content","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:creporter","reporterThreshold":"1","role":"content"}`,
		owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	b := spRunEpoch(t, &ct, "hive:alice")
	proof, _ := b.tree.Proof("hive:alice")

	before := balanceOfAcct(t, &ct, "hive:alice", 5)
	r := call(t, &ct, spDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"100","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, false)
	t.Logf("claim with a refused stakeFor: success=%v ret=%q err=%q", r.Success, r.Ret, r.ErrMsg)

	after := balanceOfAcct(t, &ct, "hive:alice", 6)
	claimed := stateOfKey(t, &ct, spDist, "claimed|content|0|hive:alice")

	if r.Success {
		t.Errorf("SILENT LOSS: the claim reported success with stakeFor refused. "+
			"balance %s -> %s, claimed marker %q — alice is recorded as paid but "+
			"received only the liquid half", before, after, claimed)
		return
	}

	// The safe outcome: the whole claim unwound, so alice keeps her entitlement and
	// can claim again once the allowlist is right.
	assert.Equal(t, before, after,
		"the claim failed, so no partial payment may have landed")
	assert.Empty(t, claimed,
		"the claim failed, so the claimed marker must have unwound too — otherwise "+
			"alice is locked out of an entitlement she was never paid")
	t.Log("SAFE: a refused stakeFor unwinds the whole claim — nothing paid, " +
		"nothing marked, alice can retry")
}

func balanceOfAcct(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) string {
	t.Helper()
	r := call(t, ct, tokenID, "balanceOf", fmt.Sprintf(`{"account":"%s"}`, acct), "hive:probe", h, true)
	return r.Ret
}

func stateOfKey(t *testing.T, ct *test_utils.ContractTest, contract, key string) string {
	t.Helper()
	return strings.TrimSpace(ct.StateGet(contract, key))
}
