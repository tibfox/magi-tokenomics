package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The policy digest must be enforced whatever order the calls arrive in.
//
// The check added with the digest reads the per-epoch pin:
//
//	if want := getStr("policy|" + ch + "|" + ep); want != "" { ... }
//
// and only pullFunding wrote that pin. The shipped reporter emits submitRoot BEFORE
// pullFunding (reporter/submit/plan_full.go), so on the reporter's own call order the
// pin was always empty at submitRoot and the branch was dead — the contract comment
// claimed it "catches the reporter that skipped the check, and it does so at the
// root, which is the single point that authorises money", and it caught nothing ever.
//
// The itest fixture hid it: ppSetup pulls funding during setup, so every earlier test
// exercised the opposite order from production. That is the whole defect — two sites
// that must agree about when the pin exists, and a fixture that ordered them one way
// while the reporter ordered them the other.
//
// Fixed by making the pin established by whichever call touches the epoch first, so
// there is no order in which the check is skipped.

func TestPolicyOrder_RootBeforeFundingIsStillChecked(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`, caOwner, 2, true)

	// epoch 1, in the order reporter/submit/plan_full.go actually emits
	call(t, ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 3, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"1","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 3, true)
	assert.Empty(t, alState(t, ct, "policy|author|1"),
		"nothing has funded epoch 1 yet — this is the state the reporter's submitRoot runs in")

	root := strings.Repeat("cd", 32)
	r := call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","policy":"%s","root":"%s","totalShares":"100"}`,
		ppPolicyB, root), "hive:reporter", 3, false)
	caFailedFor(t, r, "policy digest does not match")
	assert.Empty(t, alState(t, ct, "root|author|1"),
		"a root scored under the wrong policy must not be committed")
}

// Whichever call arrives first must PIN the policy, so the second is held to it.
func TestPolicyOrder_SubmitRootPinsWhenItIsFirst(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`, caOwner, 2, true)
	call(t, ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 3, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"1","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 3, true)

	root := strings.Repeat("cd", 32)
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","policy":"%s","root":"%s","totalShares":"100"}`,
		ppPolicyA, root), "hive:reporter", 3, true)
	assert.Equal(t, ppPolicyA, alState(t, ct, "policy|author|1"),
		"submitRoot arriving first must pin the epoch's policy, or the pin depends on call order")

	// governance moves policy, then funding lands — the epoch keeps what it was
	// scored under, which is the guarantee the per-epoch pin exists to provide
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyB+`"}`, caOwner, 4, true)
	call(t, ct, caC3ID, "pullFunding", `{"channel":"author","epoch":"1"}`, "hive:anyone", 4, true)
	assert.Equal(t, ppPolicyA, alState(t, ct, "policy|author|1"),
		"a later setPolicy must not re-pin an epoch already being reported")
}

// The honest path in the reporter's own order must still work end to end.
func TestPolicyOrder_MatchingDigestStillFinalizesAndPays(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)
	call(t, ct, caC3ID, "setPolicy", `{"channel":"author","policy":"`+ppPolicyA+`"}`, caOwner, 2, true)
	call(t, ct, caC2ID, "distributeEpoch", ``, "hive:keeper", 3, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"1","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 3, true)

	root := strings.Repeat("cd", 32)
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"1","policy":"%s","root":"%s","totalShares":"100"}`,
		ppPolicyA, root), "hive:reporter", 3, true)
	call(t, ct, caC3ID, "pullFunding", `{"channel":"author","epoch":"1"}`, "hive:anyone", 3, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"1"}`, "hive:reporter", 3, true)
	assert.Equal(t, "finalized", alState(t, ct, "status|author|1"))
}

// A channel that declared no policy is unaffected — single-reporter deployments
// must keep working exactly as before.
func TestPolicyOrder_UnpinnedChannelStillNeedsNoDigest(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`, "hive:reporter", 1, true)
	root := strings.Repeat("ef", 32)
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, root),
		"hive:reporter", 1, true)
	assert.Equal(t, root, alState(t, ct, "root|author|0"))
}
