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

// Coverage tests for the shared `auth` package's multi-party modes.
//
// Every pre-existing itest drives auth in Single mode (mode "0"), so the
// Cosigned (mode "1") and Attest (mode "2") M-of-N paths — the ones that carry
// the real custody risk — had zero end-to-end coverage. These tests exercise
// them through the real contracts (C3 reporter/guardian, C2 guardian/veto) on
// the cross-contract engine.

const (
	caTokenID = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
	caC2ID    = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	caC3ID    = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
	caOwner   = "hive:tibfox"
)

var caTxSeq int

// caCall issues a contract call whose tx carries MULTIPLE RequiredAuths — the
// shape Cosigned mode inspects. The engine derives msg.caller from
// RequiredAuths[0], so putting a non-authority first models the confused-deputy
// case (authority signatures present, but the invoking identity is not one).
func caCall(t *testing.T, ct *test_utils.ContractTest, id, action, payload string, auths []string, height uint64, expectOK bool) test_utils.ContractTestCallResult {
	t.Helper()
	caTxSeq++
	ct.BlockHeight = height
	res := ct.Call(stateEngine.TxVscCallContract{
		Caller: auths[0],
		Self: stateEngine.TxSelf{
			TxId:                 fmt.Sprintf("%s-cov%d-tx", action, caTxSeq),
			BlockId:              "block1",
			BlockHeight:          height,
			Timestamp:            "2025-09-03T00:00:00",
			RequiredAuths:        auths,
			RequiredPostingAuths: []string{},
		},
		ContractId: id,
		Action:     action,
		Payload:    json.RawMessage(payload),
		RcLimit:    500000,
		Intents:    []contracts.Intent{},
	})
	fmt.Printf("[cov %s h=%d auths=%v] ok=%v ret=%s err=%s\n", action, height, auths, res.Success, res.Ret, res.ErrMsg)
	assert.Equal(t, expectOK, res.Success, action+": "+res.Ret+" "+res.ErrMsg)
	return res
}

// caFailedFor asserts a call failed for the expected reason (abort text lands in
// Ret or ErrMsg depending on the runtime path).
func caFailedFor(t *testing.T, res test_utils.ContractTestCallResult, want string) {
	t.Helper()
	both := res.Ret + " | " + res.ErrMsg
	assert.Contains(t, both, want, "expected abort reason %q, got: %s", want, both)
}

// caShare reads C3's per-account share view.
func caShare(t *testing.T, ct *test_utils.ContractTest, ep, acct string, height uint64) string {
	t.Helper()
	r := caCall(t, ct, caC3ID, "shareOf",
		fmt.Sprintf(`{"epoch":"%s","account":"%s"}`, ep, acct), []string{"hive:observer"}, height, true)
	return r.Ret
}

// caTokenInit + caC2Init are the shared preamble: a fungible token owned by C2,
// one epoch of emission (100000) routed 100% to C3.
func caC2InitPayload(guardMode, guardAuth, guardThr, vetoMode, vetoAuth, vetoThr, timelock, bucketTarget string) string {
	return fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"%s","guardianMode":"%s","guardianAuth":"%s","guardianThreshold":"%s","vetoMode":"%s","vetoAuth":"%s","vetoThreshold":"%s","buckets":"author:%s:10000"}`,
		caTokenID, timelock, guardMode, guardAuth, guardThr, vetoMode, vetoAuth, vetoThr, bucketTarget)
}

func caC3InitPayload(rMode, rAuth, rThr, gMode, gAuth, gThr string) string {
	return fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"%s","reporterAuth":"%s","reporterThreshold":"%s","treasury":"hive:treasury","guardianMode":"%s","guardianAuth":"%s","guardianThreshold":"%s"}`,
		caTokenID, caC2ID, rMode, rAuth, rThr, gMode, gAuth, gThr)
}

// caSetupC3 registers token/C2/C3, inits them with the given C3 reporter policy,
// hands token ownership to C2, mints epoch 0 and pulls it into C3.
func caSetupC3(t *testing.T, rMode, rAuth, rThr string) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(caTokenID, caOwner, read(tokenWasmPath))
	ct.RegisterContract(caC2ID, caOwner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(caC3ID, caOwner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, caTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, caOwner, 0, true)
	fundC2Pool(t, &ct, caTokenID, caC2ID, "500000000", 0)
	call(t, &ct, caC2ID, "init", caC2InitPayload("0", "hive:guardian", "1", "0", "hive:veto", "1", "1", "contract:"+caC3ID), caOwner, 0, true)
	call(t, &ct, caC3ID, "init", caC3InitPayload(rMode, rAuth, rThr, "0", "hive:guardian", "1"), caOwner, 0, true)
	call(t, &ct, caTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, caC2ID), caOwner, 0, true)
	call(t, &ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, caC3ID, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 1, true)
	return &ct
}

// ---------------------------------------------------------------------------
// Attest (mode "2") — C3 reporter, 2-of-3
// ---------------------------------------------------------------------------

// TestCovAuth_C3ReporterAttest2of3 proves the async M-of-N identical-payload
// scheme end to end: no effect below threshold, exactly-once application at
// threshold, no self-threshold, no outsiders, no equivocation, no re-commit.
func TestCovAuth_C3ReporterAttest2of3(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	const pageP = `{"epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`

	// --- 1 of 2: recorded, but NOT applied -------------------------------
	r := caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep1"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":false`, "one attestation must not apply the page")
	s := caShare(t, ct, "0", "hive:alice", 1)
	assert.Contains(t, s, `"share":"0"`, "shares must be untouched below threshold")
	assert.Contains(t, s, `"totalShares":"0"`)

	// --- the SAME authority cannot reach the threshold by itself ---------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep1"}, 1, false)
	caFailedFor(t, r, "already attested")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 1), `"totalShares":"0"`)

	// --- a non-authority cannot attest -----------------------------------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:mallory"}, 1, false)
	caFailedFor(t, r, "not an authority")

	// --- 2nd DISTINCT authority, byte-identical payload → commits --------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep2"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":true`, "threshold reached must apply the page")
	s = caShare(t, ct, "0", "hive:alice", 1)
	assert.Contains(t, s, `"share":"60"`)
	assert.Contains(t, s, `"totalShares":"100"`, "page applied exactly once (60+40)")

	// --- a 3rd attestation after commit is rejected, page not re-applied --
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep3"}, 1, false)
	caFailedFor(t, r, "already committed")
	s = caShare(t, ct, "0", "hive:alice", 1)
	assert.Contains(t, s, `"share":"60"`, "shares must not double-apply")
	assert.Contains(t, s, `"totalShares":"100"`)

	// --- divergent payloads for the same (epoch,page) do NOT commit ------
	const page1A = `{"epoch":"0","page":"1","entries":"hive:carol:10"}`
	const page1B = `{"epoch":"0","page":"1","entries":"hive:carol:20"}`
	r = caCall(t, ct, caC3ID, "submitShares", page1A, []string{"hive:rep1"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":false`)
	r = caCall(t, ct, caC3ID, "submitShares", page1B, []string{"hive:rep2"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":false`, "two authorities on DIFFERENT payloads must not commit")
	s = caShare(t, ct, "0", "hive:carol", 1)
	assert.Contains(t, s, `"share":"0"`, "no payload reached the threshold")
	assert.Contains(t, s, `"totalShares":"100"`)

	// equivocation guard: one authority may not also back the rival payload
	r = caCall(t, ct, caC3ID, "submitShares", page1B, []string{"hive:rep1"}, 1, false)
	caFailedFor(t, r, "already attested")
	assert.Contains(t, caShare(t, ct, "0", "hive:carol", 1), `"share":"0"`)

	// an honest 2nd backer of payload A still commits A (tally is per payload)
	r = caCall(t, ct, caC3ID, "submitShares", page1A, []string{"hive:rep3"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":true`)
	s = caShare(t, ct, "0", "hive:carol", 1)
	assert.Contains(t, s, `"share":"10"`)
	assert.Contains(t, s, `"totalShares":"110"`)

	// --- finalizeEpoch needs the threshold too ---------------------------
	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1"}, 2, true)
	assert.Contains(t, r.Ret, `"finalized":false`, "a single reporter must not finalize")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 2), `"status":""`, "epoch must still be open")
	// and the payout stays shut while unfinalized
	caFailedFor(t, caCall(t, ct, caC3ID, "claim", `{"epoch":"0"}`, []string{"hive:alice"}, 4, false), "not finalized")

	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep2"}, 2, true)
	assert.Contains(t, r.Ret, `"success":true`)
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 2), `"status":"finalized"`)

	// sanity: the attested report actually pays out after the challenge window
	caCall(t, ct, caC3ID, "claim", `{"epoch":"0"}`, []string{"hive:alice"}, 4, true)
}

// ---------------------------------------------------------------------------
// Cosigned (mode "1") — C3 reporter, 2-of-3 in ONE tx
// ---------------------------------------------------------------------------

// TestCovAuth_C3ReporterCosigned2of3 proves the atomic M-of-N path: the tx's
// required_auths must contain >= M distinct authorities AND the invoking
// identity must itself be an authority (confused-deputy guard, CRIT-2).
func TestCovAuth_C3ReporterCosigned2of3(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "1", "hive:rep1,hive:rep2,hive:rep3", "2")

	const pageP = `{"epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`

	// --- one signer is below the threshold -------------------------------
	r := caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep1"}, 1, false)
	caFailedFor(t, r, "threshold not met")

	// --- the same authority listed twice must not inflate the count ------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep1", "hive:rep1"}, 1, false)
	caFailedFor(t, r, "threshold not met")

	// --- an outsider alone is rejected -----------------------------------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:mallory"}, 1, false)
	caFailedFor(t, r, "caller not an authority")

	// --- CONFUSED DEPUTY: 2 authority signatures present, but the invoking
	//     identity is not an authority — must FAIL even though the threshold
	//     would otherwise be satisfied. -----------------------------------
	r = caCall(t, ct, caC3ID, "submitShares", pageP,
		[]string{"hive:mallory", "hive:rep1", "hive:rep2"}, 1, false)
	caFailedFor(t, r, "caller not an authority")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 1), `"totalShares":"0"`,
		"nothing may be applied by a relayed authority set")

	// --- 2 authorities in one tx → applies -------------------------------
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep1", "hive:rep2"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":true`)
	s := caShare(t, ct, "0", "hive:alice", 1)
	assert.Contains(t, s, `"share":"60"`)
	assert.Contains(t, s, `"totalShares":"100"`)

	// --- exactly-once: a fresh cosigning quorum cannot re-apply the page --
	r = caCall(t, ct, caC3ID, "submitShares", pageP, []string{"hive:rep2", "hive:rep3"}, 1, false)
	caFailedFor(t, r, "page already applied")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 1), `"totalShares":"100"`)

	// --- finalizeEpoch is gated by the same threshold --------------------
	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1"}, 2, false)
	caFailedFor(t, r, "threshold not met")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 2), `"status":""`)

	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1", "hive:rep3"}, 2, true)
	assert.Contains(t, r.Ret, `"success":true`)
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 2), `"status":"finalized"`)
}

// ---------------------------------------------------------------------------
// Config validation — auth.Validate + the per-contract mode/threshold parsing
// ---------------------------------------------------------------------------

// TestCovAuth_ConfigValidationRejectsBadPolicies proves an incoherent M-of-N
// policy cannot be installed at init: a failed init aborts (and rolls back), so
// the contract stays uninitialized until a coherent policy is supplied.
func TestCovAuth_ConfigValidationRejectsBadPolicies(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(caTokenID, caOwner, read(tokenWasmPath))
	ct.RegisterContract(caC2ID, caOwner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(caC3ID, caOwner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, caTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, caOwner, 0, true)
	fundC2Pool(t, &ct, caTokenID, caC2ID, "500000000", 0)
	call(t, &ct, caC2ID, "init", caC2InitPayload("0", "hive:guardian", "1", "0", "hive:veto", "1", "1", "contract:"+caC3ID), caOwner, 0, true)

	bad := func(name, payload, want string) {
		t.Helper()
		fmt.Printf("--- bad-policy case: %s\n", name)
		r := call(t, &ct, caC3ID, "init", payload, caOwner, 0, false)
		caFailedFor(t, r, want)
		// the failed init must not have latched the contract
		caFailedFor(t, call(t, &ct, caC3ID, "shareOf", `{"epoch":"0","account":"hive:alice"}`, "hive:x", 0, false),
			"not initialized")
	}

	// duplicate authority — one signer would otherwise satisfy an M-of-N count
	bad("dup", caC3InitPayload("2", "hive:rep1,hive:rep1", "2", "0", "hive:guardian", "1"),
		"duplicate authority")
	// threshold greater than N is unsatisfiable
	bad("thr>n", caC3InitPayload("2", "hive:rep1,hive:rep2", "3", "0", "hive:guardian", "1"),
		"threshold must be 1..N")
	// threshold 0 must not silently mean "anyone"
	bad("thr0", caC3InitPayload("2", "hive:rep1,hive:rep2", "0", "0", "hive:guardian", "1"),
		"threshold must be a positive integer")
	// non-numeric threshold must not silently mean 1-of-N
	bad("thrNaN", caC3InitPayload("2", "hive:rep1,hive:rep2", "two", "0", "hive:guardian", "1"),
		"threshold must be a positive integer")
	// unknown mode must not silently downgrade to Single
	bad("mode?", caC3InitPayload("7", "hive:rep1,hive:rep2", "2", "0", "hive:guardian", "1"),
		"unknown mode")
	bad("modeEmpty", caC3InitPayload("", "hive:rep1", "1", "0", "hive:guardian", "1"),
		"unknown mode")
	// Single mode with more than one authority = operator meant M-of-N
	bad("single>1", caC3InitPayload("0", "hive:rep1,hive:rep2", "1", "0", "hive:guardian", "1"),
		"single mode takes exactly one authority")
	// no authorities at all
	bad("none", caC3InitPayload("2", "", "1", "0", "hive:guardian", "1"),
		"no authorities")
	// the state-key delimiter must never appear inside an authority id
	bad("pipe", caC3InitPayload("2", "hive:rep1,hive:re|p2", "2", "0", "hive:guardian", "1"),
		"not allowed in authority")
	// the GUARDIAN policy is validated the same way, not just the reporter one
	bad("guardianSingle>1", caC3InitPayload("2", "hive:rep1,hive:rep2", "2", "0", "hive:g1,hive:g2", "1"),
		"single mode takes exactly one authority")
	bad("guardianDup", caC3InitPayload("2", "hive:rep1,hive:rep2", "2", "2", "hive:g1,hive:g1", "2"),
		"duplicate authority")

	// a coherent 2-of-3 attest policy is accepted
	call(t, &ct, caC3ID, "init",
		caC3InitPayload("2", "hive:rep1,hive:rep2,hive:rep3", "2", "1", "hive:g1,hive:g2,hive:g3", "2"),
		caOwner, 0, true)
	call(t, &ct, caC3ID, "shareOf", `{"epoch":"0","account":"hive:alice"}`, "hive:x", 0, true)
}

// ---------------------------------------------------------------------------
// Attest (mode "2") on C2 — guardian 2-of-3 queue/execute, veto 2-of-2 cancel
// ---------------------------------------------------------------------------

// TestCovAuth_C2GuardianVetoAttest proves the timelocked token passthrough is
// gated by an M-of-N guardian coalition, and that the SEPARATE veto authority
// (which the guardian must not be able to satisfy) needs its own threshold to
// kill a queued op.
func TestCovAuth_C2GuardianVetoAttest(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(caTokenID, caOwner, read(tokenWasmPath))
	ct.RegisterContract(caC2ID, caOwner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, caTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, caOwner, 0, true)
	call(t, &ct, caC2ID, "init",
		caC2InitPayload("2", "hive:g1,hive:g2,hive:g3", "2", "2", "hive:v1,hive:v2", "2", "5", "hive:sink"),
		caOwner, 0, true)
	call(t, &ct, caTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, caC2ID), caOwner, 0, true)

	const pause1 = `{"op":"pause","nonce":"1"}`

	// --- guardian 2-of-3 queue -------------------------------------------
	r := caCall(t, &ct, caC2ID, "queueTokenOp", pause1, []string{"hive:g1"}, 10, true)
	assert.Contains(t, r.Ret, `"queued":false`, "one guardian must not queue an op")
	// nothing queued yet → execute must say so
	caFailedFor(t, caCall(t, &ct, caC2ID, "executeTokenOp", pause1, []string{"hive:g1"}, 30, false), "op not queued")

	// no self-threshold, and no outsiders
	caFailedFor(t, caCall(t, &ct, caC2ID, "queueTokenOp", pause1, []string{"hive:g1"}, 10, false), "already attested")
	caFailedFor(t, caCall(t, &ct, caC2ID, "queueTokenOp", pause1, []string{"hive:mallory"}, 10, false), "not an authority")

	r = caCall(t, &ct, caC2ID, "queueTokenOp", pause1, []string{"hive:g2"}, 10, true)
	assert.Contains(t, r.Ret, `"queued":true`, "threshold guardians must queue the op")
	// timelock (5) has not elapsed
	caFailedFor(t, caCall(t, &ct, caC2ID, "executeTokenOp", pause1, []string{"hive:g1"}, 12, false), "timelock not elapsed")

	// --- veto is a DIFFERENT authority set --------------------------------
	caFailedFor(t, caCall(t, &ct, caC2ID, "cancelTokenOp", pause1, []string{"hive:g1"}, 12, false),
		"not an authority") // a guardian cannot veto its own queued op
	caFailedFor(t, caCall(t, &ct, caC2ID, "cancelTokenOp", pause1, []string{"hive:mallory"}, 12, false),
		"not an authority")

	// one veto signer is below the 2-of-2 threshold → op survives
	r = caCall(t, &ct, caC2ID, "cancelTokenOp", pause1, []string{"hive:v1"}, 12, true)
	assert.Contains(t, r.Ret, `"cancelled":false`, "one veto signer must not cancel")
	caFailedFor(t, caCall(t, &ct, caC2ID, "executeTokenOp", pause1, []string{"hive:g1"}, 12, false),
		"timelock not elapsed") // still queued (would be "op not queued" if cancelled)
	caFailedFor(t, caCall(t, &ct, caC2ID, "cancelTokenOp", pause1, []string{"hive:v1"}, 12, false),
		"already attested")

	// the threshold does cancel it
	r = caCall(t, &ct, caC2ID, "cancelTokenOp", pause1, []string{"hive:v2"}, 12, true)
	assert.Contains(t, r.Ret, `"success":true`)
	caFailedFor(t, caCall(t, &ct, caC2ID, "executeTokenOp", pause1, []string{"hive:g1"}, 30, false), "op not queued")

	// --- a fresh 2-of-3 (different pair) queues + executes for real -------
	const pause2 = `{"op":"pause","nonce":"2"}`
	r = caCall(t, &ct, caC2ID, "queueTokenOp", pause2, []string{"hive:g1"}, 40, true)
	assert.Contains(t, r.Ret, `"queued":false`)
	r = caCall(t, &ct, caC2ID, "queueTokenOp", pause2, []string{"hive:g3"}, 40, true)
	assert.Contains(t, r.Ret, `"queued":true`, "any 2 of the 3 guardians must suffice")

	p := caCall(t, &ct, caTokenID, "isPaused", `{}`, []string{"hive:x"}, 40, true)
	assert.Contains(t, p.Ret, `false`, "token must still be live before execution")

	caCall(t, &ct, caC2ID, "executeTokenOp", pause2, []string{"hive:g1"}, 46, true)
	p = caCall(t, &ct, caTokenID, "isPaused", `{}`, []string{"hive:x"}, 46, true)
	assert.Contains(t, p.Ret, `true`, "the attested guardian op must have reached the token")

	// and it is consumed exactly once
	caFailedFor(t, caCall(t, &ct, caC2ID, "executeTokenOp", pause2, []string{"hive:g1"}, 46, false), "op not queued")
}

// ---------------------------------------------------------------------------
// Attest (mode "2") on the C3 GUARDIAN — cancelEpoch + sweepUnallocated
// ---------------------------------------------------------------------------

// caSetupC3Policy is caSetupC3 with an explicit guardian policy as well.
func caSetupC3Policy(t *testing.T, rMode, rAuth, rThr, gMode, gAuth, gThr string) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(caTokenID, caOwner, read(tokenWasmPath))
	ct.RegisterContract(caC2ID, caOwner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(caC3ID, caOwner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, caTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, caOwner, 0, true)
	fundC2Pool(t, &ct, caTokenID, caC2ID, "500000000", 0)
	call(t, &ct, caC2ID, "init", caC2InitPayload("0", "hive:guardian", "1", "0", "hive:veto", "1", "1", "contract:"+caC3ID), caOwner, 0, true)
	call(t, &ct, caC3ID, "init", caC3InitPayload(rMode, rAuth, rThr, gMode, gAuth, gThr), caOwner, 0, true)
	call(t, &ct, caTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, caC2ID), caOwner, 0, true)
	call(t, &ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, caC3ID, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 1, true)
	return &ct
}

// TestCovAuth_C3GuardianAttest2of3 proves the guardian's fund-moving powers
// (epoch veto + treasury sweep) also require an M-of-N coalition, and that a
// sub-threshold attempt changes nothing.
func TestCovAuth_C3GuardianAttest2of3(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3Policy(t, "0", "hive:reporter", "1", "2", "hive:g1,hive:g2,hive:g3", "2")

	call(t, ct, caC3ID, "submitShares", `{"epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`, "hive:reporter", 1, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, "hive:reporter", 2, true)

	// --- cancelEpoch (the veto) needs 2 of 3 ------------------------------
	caFailedFor(t, caCall(t, ct, caC3ID, "cancelEpoch", `{"epoch":"0"}`, []string{"hive:mallory"}, 2, false),
		"not an authority")
	r := caCall(t, ct, caC3ID, "cancelEpoch", `{"epoch":"0"}`, []string{"hive:g1"}, 2, true)
	assert.Contains(t, r.Ret, `"cancelled":false`, "one guardian must not cancel an epoch")
	s := caShare(t, ct, "0", "hive:alice", 2)
	assert.Contains(t, s, `"status":"finalized"`, "sub-threshold veto must leave the epoch intact")
	assert.Contains(t, s, `"funded":"100000"`)
	caFailedFor(t, caCall(t, ct, caC3ID, "cancelEpoch", `{"epoch":"0"}`, []string{"hive:g1"}, 2, false),
		"already attested")

	r = caCall(t, ct, caC3ID, "cancelEpoch", `{"epoch":"0"}`, []string{"hive:g2"}, 2, true)
	assert.Contains(t, r.Ret, `"success":true`)
	s = caShare(t, ct, "0", "hive:alice", 2)
	assert.Contains(t, s, `"status":"cancelled"`)
	assert.Contains(t, s, `"funded":"0"`, "cancelled funding rolls into the unallocated pool")
	caFailedFor(t, caCall(t, ct, caC3ID, "claim", `{"epoch":"0"}`, []string{"hive:alice"}, 4, false), "not finalized")

	// --- sweepUnallocated needs 2 of 3, and pays only the pinned treasury -
	r = caCall(t, ct, caC3ID, "sweepUnallocated", `{"nonce":"1"}`, []string{"hive:g1"}, 4, true)
	assert.Contains(t, r.Ret, `"swept":false`, "one guardian must not sweep")
	b := caCall(t, ct, caTokenID, "balanceOf", `{"account":"hive:treasury"}`, []string{"hive:x"}, 4, true)
	assert.Contains(t, b.Ret, `"0"`, "nothing may move below the threshold")

	r = caCall(t, ct, caC3ID, "sweepUnallocated", `{"nonce":"1"}`, []string{"hive:g3"}, 4, true)
	assert.Contains(t, r.Ret, `"swept":"100000"`)
	b = caCall(t, ct, caTokenID, "balanceOf", `{"account":"hive:treasury"}`, []string{"hive:x"}, 4, true)
	assert.Contains(t, b.Ret, `"100000"`)

	// the committed sweep action cannot be replayed, and a fresh nonce finds
	// an empty pool (no double-spend of the unallocated balance)
	caFailedFor(t, caCall(t, ct, caC3ID, "sweepUnallocated", `{"nonce":"1"}`, []string{"hive:g2"}, 4, false),
		"already committed")
	caCall(t, ct, caC3ID, "sweepUnallocated", `{"nonce":"2"}`, []string{"hive:g1"}, 4, true)
	caFailedFor(t, caCall(t, ct, caC3ID, "sweepUnallocated", `{"nonce":"2"}`, []string{"hive:g2"}, 4, false),
		"nothing to sweep")
	b = caCall(t, ct, caTokenID, "balanceOf", `{"account":"hive:treasury"}`, []string{"hive:x"}, 4, true)
	assert.Contains(t, b.Ret, `"100000"`)
}

// Two honest reporters must be able to finalize even if they attest at DIFFERENT
// points in the report's construction.
//
// finalizeEpoch used to attest over `totalShares|<ep> : funded|<ep>`, read from chain
// state at attestation time. But submitShares stays open until status is set — which
// finalize itself sets only on commit — so totalShares provably moves under a pending
// finalize vote. Reporter A finalizing after page 0, and reporter B after pages 0 and
// 1, therefore attested DIFFERENT payloads. The tally is per payload hash while the
// seen-marker is one per (action, authority) with no payload component and no way to
// clear it: both votes were spent in different buckets, the threshold became
// unreachable, and the epoch was permanently unfinalizable. Recovery was only a
// guardian cancel, which pays the treasury rather than the earners.
//
// Attesting over a constant removes the divergence. Anti-equivocation is unaffected —
// still one vote per authority per action.
func TestCovAuth_C3FinalizeAttestSurvivesChangingShares(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	// page 0 reaches the threshold and APPLIES, so totalShares becomes 100
	const page0 = `{"epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`
	caCall(t, ct, caC3ID, "submitShares", page0, []string{"hive:rep1"}, 1, true)
	r := caCall(t, ct, caC3ID, "submitShares", page0, []string{"hive:rep2"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":true`)
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 1), `"totalShares":"100"`)

	// rep1 finalizes HERE — under the old code it bound totalShares=100
	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1"}, 2, true)
	assert.Contains(t, r.Ret, `"finalized":false`, "one attestation is below the threshold")

	// a second page now applies, MOVING totalShares to 110 while rep1's vote is pending
	const page1 = `{"epoch":"0","page":"1","entries":"hive:carol:10"}`
	caCall(t, ct, caC3ID, "submitShares", page1, []string{"hive:rep1"}, 2, true)
	caCall(t, ct, caC3ID, "submitShares", page1, []string{"hive:rep2"}, 2, true)
	assert.Contains(t, caShare(t, ct, "0", "hive:carol", 2), `"totalShares":"110"`)

	// rep2 finalizes against the NEW state. Under the old binding this landed in a
	// different payload bucket and the epoch could never be finalized by anyone.
	r = caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep2"}, 3, true)
	assert.Contains(t, r.Ret, `"success":true`,
		"votes cast at different totalShares must still merge to the threshold")
	assert.Contains(t, caShare(t, ct, "0", "hive:carol", 3), `"status":"finalized"`)

	// and the epoch pays out the FULL report, not the partial one
	caCall(t, ct, caC3ID, "claim", `{"epoch":"0"}`, []string{"hive:carol"}, 5, true)
}

// The constant payload must not weaken anti-equivocation: one authority still gets
// exactly one finalize vote, and cannot spend a second.
func TestCovAuth_C3FinalizeStillOneVotePerAuthority(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")
	caCall(t, ct, caC3ID, "submitShares",
		`{"epoch":"0","page":"0","entries":"hive:alice:60"}`, []string{"hive:rep1"}, 1, true)
	caCall(t, ct, caC3ID, "submitShares",
		`{"epoch":"0","page":"0","entries":"hive:alice:60"}`, []string{"hive:rep2"}, 1, true)

	caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1"}, 2, true)
	r := caCall(t, ct, caC3ID, "finalizeEpoch", `{"epoch":"0"}`, []string{"hive:rep1"}, 2, false)
	caFailedFor(t, r, "already attested")
	assert.Contains(t, caShare(t, ct, "0", "hive:alice", 2), `"status":""`,
		"one authority must not reach the threshold alone")
}
