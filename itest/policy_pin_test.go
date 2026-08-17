package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// The reporter policy digest, on the contract side.
//
// sharecore proves the same input yields the same bytes, but nothing forced two
// reporters to feed the same input: tags, the dust cutoff, the reward curves, the
// vote-mana budget and pagination were local config compared against nothing. Two
// HONEST reporters differing in one of them score the epoch differently forever,
// and in Attest mode that is a DEADLOCK — the tally is per payload hash and each
// authority has one vote per action, so both burn their vote in a different bucket
// and the page can never reach threshold.
//
// The reporter's own check is what prevents that (it refuses before spending RC and
// names the field). These tests cover the chain-side half: the digest is required
// where it matters, pinned per epoch so governance can move it without rewriting a
// live epoch, and enforced at the root — the one call that authorises money.

const ppPolicyA = "aaaa111111111111111111111111111111111111111111111111111111111111"
const ppPolicyB = "bbbb222222222222222222222222222222222222222222222222222222222222"

// ppSetup is caSetupC3 with a SECOND funder bucket, so these tests can add a
// channel of their own. addChannel verifies the bucket against the funder before it
// looks at anything else, so a test channel needs a real bucket or it aborts for
// the wrong reason and proves nothing.
func ppSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(caTokenID, caOwner, read(tokenWasmPath))
	ct.RegisterContract(caC2ID, caOwner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(caC3ID, caOwner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, caTokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, caOwner, 0, true)
	fundC2Pool(t, &ct, caTokenID, caC2ID, "500000000", 0)
	call(t, &ct, caC2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0",`+
			`"vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:6000,extra:contract:%s:4000"}`,
		caTokenID, caC3ID, caC3ID), caOwner, 0, true)
	call(t, &ct, caC3ID, "init", caC3InitPayload("0", "hive:reporter", "1", "0", "hive:guardian", "1"), caOwner, 0, true)
	caAddChannel(t, &ct, "0", "hive:reporter", "1", true)
	call(t, &ct, caTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, caC2ID), caOwner, 0, true)
	call(t, &ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, caC3ID, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:anyone", 1, true)
	return &ct
}

func ppAddChannel(t *testing.T, ct *test_utils.ContractTest, ch, mode, auth, thr, policy string, ok bool) test_utils.ContractTestCallResult {
	t.Helper()
	pol := ""
	if policy != "" {
		pol = `,"policy":"` + policy + `"`
	}
	return call(t, ct, caC3ID, "addChannel", fmt.Sprintf(
		`{"channel":"%s","bucket":"%s","window":"1","reporterMode":"%s","reporterAuth":"%s",`+
			`"reporterThreshold":"%s"%s}`, ch, ch, mode, auth, thr, pol), caOwner, 0, ok)
}

// Attest mode is the mode that can deadlock, so it is the mode that must declare
// what it is scoring. Single and cosigned may omit it: a lone reporter has nobody
// to disagree with, and cosigned reporters sign ONE transaction, so a disagreement
// never reaches the chain at all.
func TestPolicy_AttestChannelCannotBeAddedWithoutOne(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	r := ppAddChannel(t, ct, "extra", "2", "hive:rep1,hive:rep2", "2", "", false)
	caFailedFor(t, r, "policy required for attest mode")

	// the same channel WITH a policy is accepted
	ppAddChannel(t, ct, "extra", "2", "hive:rep1,hive:rep2", "2", ppPolicyA, true)
}

func TestPolicy_MalformedDigestIsRefused(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	for _, bad := range []string{"nothex", strings.Repeat("a", 63), strings.Repeat("a", 65), "zz" + strings.Repeat("a", 62)} {
		r := ppAddChannel(t, ct, "extra", "0", "hive:r", "1", bad, false)
		caFailedFor(t, r, "policy must be 64 hex characters")
	}
}

// The pin is what makes an epoch reproducible: it is taken when the epoch is
// FUNDED and does not move afterwards, so governance may change the channel policy
// without retroactively changing what an already-funded epoch is scored against.
func TestPolicy_PinIsTakenAtFundingAndDoesNotMoveAfterwards(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	// epoch 0 of the author channel was funded in caSetupC3, before any policy was
	// set, so it carries no pin and enforcement stays off for it.
	assert.Empty(t, alState(t, ct, "policy|author|0"),
		"an epoch funded before any policy existed must not acquire one retroactively")

	// governance sets a policy now
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`, caOwner, 2, true)
	assert.Equal(t, ppPolicyA, alState(t, ct, "ch_policy|author"))

	// epoch 0 is STILL unpinned: setting a policy does not reach back
	assert.Empty(t, alState(t, ct, "policy|author|0"),
		"setPolicy rewrote an epoch that was already funded — an epoch must stay "+
			"scored against the rules it was funded under")

	// the next epoch to be funded picks the current policy up
	call(t, ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 3, true)
	call(t, ct, caC3ID, "pullFunding", `{"channel":"author","epoch":"1"}`, "hive:anyone", 3, true)
	assert.Equal(t, ppPolicyA, alState(t, ct, "policy|author|1"),
		"an epoch funded after the policy was set must carry it")

	// governance moves policy again; epoch 1 keeps the digest it was funded under
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyB+`"}`, caOwner, 4, true)
	assert.Equal(t, ppPolicyA, alState(t, ct, "policy|author|1"),
		"changing the channel policy moved an already-funded epoch's pin — old epochs "+
			"would stop being reproducible")
	assert.Equal(t, ppPolicyB, alState(t, ct, "ch_policy|author"))
}

// Why there is no "a second pullFunding must not re-pin the epoch" test here.
//
// The snapshot sits under the same !present("fundedAt|...") guard as the staleness
// clock, so it is written once per epoch however often funding is pulled. That
// guard is DEFENSIVE rather than reachable: C2 allocates an epoch all-or-nothing
// (distributeEpoch breaks with `starved` when the pool cannot cover the whole
// epoch, changing no state), addOwed therefore writes owed|<bucket>|<ep> exactly
// once, and claimBucket deletes it before transferring. A second pullFunding for
// the same epoch finds nothing owed and aborts, so the transaction reverts and
// could not have re-pinned anything anyway.
//
// A test was written for it and REMOVED: it asserted the pin was unchanged after a
// pullFunding that had aborted, which is true whether the guard exists or not.
// Deleting the guard left it green. That is the vacuous-assertion failure this
// codebase warns about, and a green test proving nothing is worse than no test —
// it reports coverage that is not there. If C2 ever gains partial allocation, the
// guard becomes reachable and a real test belongs here.

// The root is the one call that authorises money — pages are only logged now — so
// it is where a reporter running different rules must be stopped on chain.
func TestPolicy_RootIsRefusedWhenTheDigestDisagrees(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`, caOwner, 2, true)
	call(t, ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 3, true)
	call(t, ct, caC3ID, "pullFunding", `{"channel":"author","epoch":"1"}`, "hive:anyone", 3, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"1","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 3, true)

	root := strings.Repeat("cd", 32)
	// wrong digest
	r := call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","policy":"%s","root":"%s","totalShares":"100"}`,
		ppPolicyB, root), "hive:reporter", 3, false)
	caFailedFor(t, r, "policy digest does not match")

	// absent digest is refused too — omitting the field must not be a way past it
	r = call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","root":"%s","totalShares":"100"}`, root),
		"hive:reporter", 3, false)
	caFailedFor(t, r, "policy digest does not match")

	// the right one goes through
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","policy":"%s","root":"%s","totalShares":"100"}`,
		ppPolicyA, root), "hive:reporter", 3, true)
	assert.Equal(t, root, alState(t, ct, "root|author|1"))
}

// An epoch with NO pin must keep working exactly as before, or every existing
// single-reporter deployment breaks on upgrade.
func TestPolicy_UnpinnedEpochIsUnaffected(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	assert.Empty(t, alState(t, ct, "policy|author|0"))
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 1, true)
	root := strings.Repeat("ef", 32)
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, root),
		"hive:reporter", 1, true)
	assert.Equal(t, root, alState(t, ct, "root|author|0"),
		"a channel that declared no policy must submit a root exactly as it did before")
}

func TestPolicy_SetPolicyIsOwnerOnlyAndValidates(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	r := call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`,
		"hive:mallory", 2, false)
	caFailedFor(t, r, "only owner can set policy")

	r = call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"nothex"}`, caOwner, 2, false)
	caFailedFor(t, r, "policy must be 64 hex characters")

	r = call(t, ct, caC3ID, "setPolicy", `{"channel":"nosuch","policy":"`+ppPolicyA+`"}`, caOwner, 2, false)
	caFailedFor(t, r, "no such channel")
}
