package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
)

// A posting key must not configure a deployment either.
//
// TestPriv_PostingKeyCannotUsePrivilegedPaths already covers the FUND-MOVING owner
// entrypoints — airdropBatch, queueTokenOp — and they are guarded: both call
// auth.RequireActive after the owner check. The CONFIG entrypoints are not, and they
// were never tested: init, adoptSchedule, addChannel and setPolicy each compare
// msg.caller to contract.owner and stop there.
//
// That is enough on VSC. The runtime derives msg.caller from RequiredPostingAuths[0]
// when a transaction carries no active auth (the reason auth.RequireActive exists at
// all, see auth/auth.go:90), so a posting-only transaction from the owner's account
// satisfies `mustCaller() == owner`.
//
// Posting authority is the one people delegate. Hive users hand it to front-ends
// routinely and think of it as the safe half of their keys, which is exactly why the
// fund paths guard it — and configuring a distributor is not a lesser power than
// spending from it. addChannel fixes a channel's reporter authority permanently
// (channels are append-only), so it decides who may publish share books.

const pcC1 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"

func pcSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pvC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(pvC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	ct.RegisterContract(pcC1, owner, read("../c1-staking/artifacts/main.wasm"))
	pvInitC2(t, &ct, pvC3)
	call(t, &ct, pvC3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:pctreasury",`+
			`"guardianMode":"0","guardianAuth":"hive:pcguard","guardianThreshold":"1"}`,
		pvTok, pvC2), owner, 0, true)
	return &ct
}

// addChannel is the sharpest of these: a channel's reporter authority is fixed here
// and channels are append-only, so this call decides who may publish share books for
// that channel for the life of the deployment.
func TestPostingConfig_CannotAddChannel(t *testing.T) {
	ct := pcSetup(t)
	pvCallPosting(t, ct, pvC3, "addChannel",
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:attacker","reporterThreshold":"1"}`, owner, 1, false)

	// the same call WITH an active authority must still work — otherwise this test
	// would pass against a contract that simply refused everything
	call(t, ct, pvC3, "addChannel",
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:creporter","reporterThreshold":"1"}`, owner, 1, true)
}

// setPolicy changes what the reporters of a channel are scored against.
func TestPostingConfig_CannotSetPolicy(t *testing.T) {
	ct := pcSetup(t)
	call(t, ct, pvC3, "addChannel",
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:creporter","reporterThreshold":"1"}`, owner, 1, true)

	pol := `{"channel":"author","policy":"` + ppPolicyA + `"}`
	pvCallPosting(t, ct, pvC3, "setPolicy", pol, owner, 2, false)
	call(t, ct, pvC3, "setPolicy", pol, owner, 2, true)
}

// adoptSchedule is one-shot and immutable, so a posting key reaching it does not
// merely misconfigure the deployment — it spends the only chance to configure it.
func TestPostingConfig_CannotAdoptSchedule(t *testing.T) {
	ct := pcSetup(t)
	call(t, ct, pcC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":""}`, pvTok),
		owner, 0, true)

	// The bucket here pays C3, not C1, so this call cannot succeed either way. What
	// matters is WHICH abort it gets: "the funder's bucket of that name pays a
	// different contract" is raised well after the owner gate, and reaching it proves
	// a posting-only transaction passed that gate. The authority abort must come
	// first.
	adopt := fmt.Sprintf(`{"funder":"%s","bucket":"author"}`, pvC2)
	r := pvCallPosting(t, ct, pcC1, "adoptSchedule", adopt, owner, 1, false)
	caFailedFor(t, r, "active authority required")
}

// init is the same shape: one-shot, and it pins the token, the treasury, the
// guardian set and the stakeFor allowlist.
func TestPostingConfig_CannotInit(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pcC1, owner, read("../c1-staking/artifacts/main.wasm"))
	call(t, &ct, pvTok, "init",
		`{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)

	initPayload := fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"hive:attacker"}`, pvTok)
	pvCallPosting(t, &ct, pcC1, "init", initPayload, owner, 0, false)
	call(t, &ct, pcC1, "init", initPayload, owner, 0, true)
}
