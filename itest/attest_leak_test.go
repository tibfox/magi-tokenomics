package itest_test

import (
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Attest mode's working state, and the two ways it used to be stranded.
//
// Each attested payload is held in full — up to 4,096 bytes, a whole share page —
// until the round resolves. Two paths left it there permanently:
//
//  1. A round that COMMITS while authorities disagreed. Only the winning payload's
//     blob was deleted, so every losing payload stayed. That is not a corner case:
//     it is exactly what happens when a rogue reporter attests a fraudulent page
//     and the honest majority commits a different one.
//  2. A round that NEVER commits. Two of three attest, the third never arrives,
//     the epoch is re-run under a different page number, and nothing can remove
//     what the abandoned round left behind.
//
// The tests read `rep|ahashes|<action>`, which lists the hashes an action has seen.
// That is what makes the leak observable without reimplementing the hash here — a
// second copy of it in the test tree could drift from the contract's.

func alState(t *testing.T, ct *test_utils.ContractTest, key string) string {
	t.Helper()
	return strings.Trim(ct.StateGet(caC3ID, key), `"`)
}

const (
	alPage1A = `{"channel":"author","epoch":"0","page":"1","entries":"hive:carol:10"}`
	alPage1B = `{"channel":"author","epoch":"0","page":"1","entries":"hive:carol:20"}`
	alAction = "ss:author:0:1"
)

func TestAttest_CommitReleasesEveryPayload_NotOnlyTheWinner(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	caCall(t, ct, caC3ID, "submitShares", alPage1A, []string{"hive:rep1"}, 1, true)
	caCall(t, ct, caC3ID, "submitShares", alPage1B, []string{"hive:rep2"}, 1, true)

	// both rival payloads are now held; capture their hashes before the commit
	hashes := alState(t, ct, "rep|ahashes|"+alAction)
	parts := strings.Split(hashes, ",")
	if len(parts) != 2 {
		t.Fatalf("expected 2 rival payloads tracked, got %q", hashes)
	}
	for _, h := range parts {
		if got := alState(t, ct, "rep|acanon|"+alAction+"|"+h); got == "" {
			t.Fatalf("payload %s is not held before the commit — the test is not observing what it thinks", h)
		}
	}

	// a third authority backs A, reaching the threshold for A
	r := caCall(t, ct, caC3ID, "submitShares", alPage1A, []string{"hive:rep3"}, 1, true)
	assert.Contains(t, r.Ret, `"applied":true`)

	// EVERY payload must be released, including the one that lost
	for _, h := range parts {
		if got := alState(t, ct, "rep|acanon|"+alAction+"|"+h); got != "" {
			t.Errorf("payload %s survived the commit (%d bytes held): a losing page stays in "+
				"state for the life of the deployment", h, len(got))
		}
		if got := alState(t, ct, "rep|atally|"+alAction+"|"+h); got != "" {
			t.Errorf("tally for %s survived the commit: %q", h, got)
		}
	}
	assert.Empty(t, alState(t, ct, "rep|ahashes|"+alAction), "hash list must not survive")
	assert.Empty(t, alState(t, ct, "rep|astart|"+alAction), "start stamp must not survive")
	for _, a := range []string{"hive:rep1", "hive:rep2", "hive:rep3"} {
		assert.Empty(t, alState(t, ct, "rep|aseen|"+alAction+"|"+a), "vote record for "+a+" must not survive")
	}
	// the commit flag IS the lasting record and must remain
	assert.NotEmpty(t, alState(t, ct, "rep|adone|"+alAction), "the done flag is the lasting record")
}

func TestAttest_StalledRoundIsReleasableButOnlyOnceStale(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	// one attestation, then the coalition stalls
	caCall(t, ct, caC3ID, "submitShares", alPage1A, []string{"hive:rep1"}, 100, true)
	hashes := alState(t, ct, "rep|ahashes|"+alAction)
	if hashes == "" {
		t.Fatal("nothing was recorded for the stalled round")
	}
	assert.Equal(t, "100", alState(t, ct, "rep|astart|"+alAction), "the round's start height is stamped")

	rel := `{"role":"rep","channel":"author","action":"` + alAction + `"}`

	// NOT releasable while it could still legitimately complete. This is the
	// property that keeps it permissionless: an authority losing a vote must not
	// be able to clear the tally and re-run it.
	r := caCall(t, ct, caC3ID, "releaseStaleAttest", rel, []string{"hive:anyone"}, 200, false)
	caFailedFor(t, r, "not stale yet")
	assert.NotEmpty(t, alState(t, ct, "rep|acanon|"+alAction+"|"+hashes), "payload must survive an early release attempt")

	// once genuinely stale, anyone may release it
	r = caCall(t, ct, caC3ID, "releaseStaleAttest", rel, []string{"hive:anyone"}, 100+28800, true)
	assert.Contains(t, r.Ret, `"released":"1"`)
	assert.Empty(t, alState(t, ct, "rep|acanon|"+alAction+"|"+hashes), "the stalled payload must be gone")
	assert.Empty(t, alState(t, ct, "rep|ahashes|"+alAction))
	assert.Empty(t, alState(t, ct, "rep|astart|"+alAction))
	assert.Empty(t, alState(t, ct, "rep|aseen|"+alAction+"|hive:rep1"))

	// and the action is genuinely re-runnable afterwards
	caCall(t, ct, caC3ID, "submitShares", alPage1B, []string{"hive:rep1"}, 100+28801, true)
	r = caCall(t, ct, caC3ID, "submitShares", alPage1B, []string{"hive:rep2"}, 100+28802, true)
	assert.Contains(t, r.Ret, `"applied":true`, "a released round must not block a fresh one")
}

func TestAttest_ReleaseRefusesCommittedAndUnknownRounds(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	caCall(t, ct, caC3ID, "submitShares", alPage1A, []string{"hive:rep1"}, 10, true)
	caCall(t, ct, caC3ID, "submitShares", alPage1A, []string{"hive:rep2"}, 10, true) // commits

	r := caCall(t, ct, caC3ID, "releaseStaleAttest",
		`{"role":"rep","channel":"author","action":"`+alAction+`"}`, []string{"hive:anyone"}, 10+28800, false)
	caFailedFor(t, r, "already committed")

	r = caCall(t, ct, caC3ID, "releaseStaleAttest",
		`{"role":"rep","channel":"author","action":"ss:author:0:99"}`, []string{"hive:anyone"}, 99999, false)
	caFailedFor(t, r, "no attestation round in progress")
}
