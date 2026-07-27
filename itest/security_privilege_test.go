package itest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// Privilege-boundary regressions from the backdoor hunt. Each pins a fix for a
// finding where a privileged path was reachable in a way its design did not intend.

const (
	pvC2  = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	pvC3  = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
	pvC6  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	pvTok = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
)

// pvCallPosting submits a tx signed with ONLY a posting authority. The VSC runtime
// still derives msg.caller from RequiredPostingAuths[0], so privileged entrypoints
// must reject it explicitly.
func pvCallPosting(t *testing.T, ct *test_utils.ContractTest, id, action, payload, caller string, height uint64, expectOK bool) test_utils.ContractTestCallResult {
	ct.BlockHeight = height
	res := ct.Call(stateEngine.TxVscCallContract{
		Caller: caller,
		Self: stateEngine.TxSelf{
			TxId: action + "-posting-tx", BlockId: "block1", BlockHeight: height,
			Timestamp:            "2025-09-03T00:00:00",
			RequiredAuths:        []string{},       // <-- no ACTIVE auth
			RequiredPostingAuths: []string{caller}, // posting only
		},
		ContractId: id, Action: action, Payload: json.RawMessage(payload),
		RcLimit: 500000, Intents: []contracts.Intent{},
	})
	fmt.Printf("[posting %s h=%d] ok=%v err=%s\n", action, height, res.Success, res.ErrMsg)
	assert.Equal(t, expectOK, res.Success, "posting-auth "+action+": "+res.Ret+" "+res.ErrMsg)
	return res
}

func pvInitC2(t *testing.T, ct *test_utils.ContractTest, bucketTarget string) {
	call(t, ct, pvTok, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, ct, pvC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, pvTok, bucketTarget), owner, 0, true)
}

// CRIT: a Hive POSTING key must never satisfy a privileged role. The runtime falls
// back to RequiredPostingAuths[0] for msg.caller, so every privileged entrypoint
// must demand an ACTIVE authority.
func TestPriv_PostingKeyCannotUsePrivilegedPaths(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pvC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(pvC6, owner, read("../c6-migration/artifacts/main.wasm"))
	pvInitC2(t, &ct, pvC3)

	// guardian's posting key must NOT be able to queue a token takeover
	pvCallPosting(t, &ct, pvC2, "queueTokenOp",
		`{"op":"changeOwner","nonce":"1","newOwner":"hive:evil"}`, "hive:guardian", 10, false)
	// ...while the same guardian WITH an active auth can
	call(t, &ct, pvC2, "queueTokenOp",
		`{"op":"changeOwner","nonce":"1","newOwner":"hive:evil"}`, "hive:guardian", 10, true)

	// C6 owner's posting key must NOT be able to move the bootstrap balance
	call(t, &ct, pvC6, "init", fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"1000"}`, pvTok), owner, 0, true)
	pvCallPosting(t, &ct, pvC6, "airdropBatch",
		`{"batchId":"b1","entries":"hive:evil:500"}`, owner, 0, false)
}

// HIGH: a matured token op must be executed by the guardian or veto — not by any
// random account — so honest parties can decline to execute; and it must EXPIRE.
func TestPriv_ExecuteTokenOpAuthorizedAndExpires(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pvC2, owner, read("../c2-emission/artifacts/main.wasm"))
	pvInitC2(t, &ct, pvC3)
	// C2 must own the token for pause/unpause to be executable
	call(t, &ct, pvTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, pvC2), owner, 0, true)

	op := `{"op":"pause","nonce":"1"}`
	call(t, &ct, pvC2, "queueTokenOp", op, "hive:guardian", 10, true) // ready at 15
	call(t, &ct, pvC2, "executeTokenOp", op, "hive:randombot", 20, false)
	call(t, &ct, pvC2, "executeTokenOp", op, "hive:guardian", 12, false) // too early
	call(t, &ct, pvC2, "executeTokenOp", op, "hive:guardian", 16, true)  // in window

	// a queued op left un-executed past its expiry window must die
	op2 := `{"op":"unpause","nonce":"2"}`
	call(t, &ct, pvC2, "queueTokenOp", op2, "hive:guardian", 100, true) // ready 105, expires 110
	call(t, &ct, pvC2, "executeTokenOp", op2, "hive:guardian", 200, false)
}

// HIGH: the veto must not be able to keep the token paused forever — a paused token
// freezes every claim AND stakers' own withdrawals.
func TestPriv_VetoCannotBlockUnpause(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pvC2, owner, read("../c2-emission/artifacts/main.wasm"))
	pvInitC2(t, &ct, pvC3)

	// veto may cancel a pause...
	call(t, &ct, pvC2, "queueTokenOp", `{"op":"pause","nonce":"1"}`, "hive:guardian", 10, true)
	call(t, &ct, pvC2, "cancelTokenOp", `{"op":"pause","nonce":"1"}`, "hive:veto", 11, true)
	// ...but must NOT be able to veto a liveness-restoring unpause
	call(t, &ct, pvC2, "queueTokenOp", `{"op":"unpause","nonce":"2"}`, "hive:guardian", 20, true)
	call(t, &ct, pvC2, "cancelTokenOp", `{"op":"unpause","nonce":"2"}`, "hive:veto", 21, false)
}

// HIGH: the pinned treasury must not be a guardian authority (cancel+sweep would be
// a drain) nor the contract itself (token forbids transfer-to-self → bricked sweep).
func TestPriv_TreasuryMustBeDisjointFromGuardian(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pvTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pvC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(pvC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	pvInitC2(t, &ct, pvC3)

	bad := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:guardian","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, pvTok, pvC2)
	call(t, &ct, pvC3, "init", bad, owner, 0, false) // treasury == guardian

	self := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"contract:%s","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, pvTok, pvC2, pvC3)
	call(t, &ct, pvC3, "init", self, owner, 0, false) // treasury == self

	good := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, pvTok, pvC2)
	call(t, &ct, pvC3, "init", good, owner, 0, true)
}
