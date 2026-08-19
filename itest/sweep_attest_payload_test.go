package itest_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// sweepUnallocated's attested payload must not be a value the chain can move while
// the vote is open.
//
// It attested over `amt.String()`, read live from unalloc|<ch>. cancelEpoch ADDS to
// that key. So in attest mode: guardian A votes while the pool is 100, a cancelled
// epoch rolls in, guardian B votes while it is 5100 — a different payload, a
// different hash, a different tally. Anti-equivocation gives each authority one vote
// per ACTION, so both burned theirs in different buckets and the threshold became
// unreachable. The sweep action key is then dead permanently.
//
// This is the identical shape to the finalizeEpoch/totalShares deadlock already
// fixed once (see the constant-payload note on finalizeEpoch): an attested payload
// read from chain state that another entrypoint can move mid-vote. Binding to the
// amount was deliberate and correct — a co-signer approving a sweep of 0 must not
// have that vote reused later to move a large balance — so the fix is not to drop
// the binding but to bind to the amount the guardians DECLARE, which does not move,
// and to verify the chain still matches it at the moment it commits.


// Guardians voting either side of a cancelEpoch must still converge.
func TestSweepAttest_PoolMovingMidVoteDoesNotDeadlock(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3Policy(t, "0", "hive:reporter", "1", "2", "hive:g1,hive:g2,hive:g3", "2")

	// put something in the unallocated pool: finalize nothing, cancel epoch 0
	// The stale rescue, not the veto: staleBlocks is at least 1000, anchored on
	// max(epochEnd, fundedAt), so cancel well past it.
	const h = 5000
	caCall(t, ct, caC3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`,
		[]string{"hive:g1"}, h, true)
	caCall(t, ct, caC3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`,
		[]string{"hive:g2"}, h, true)
	before := alState(t, ct, "unalloc|author")
	assert.NotEmpty(t, before, "epoch 0's funding must have rolled into the unallocated pool")

	// g1 proposes a sweep of exactly what is there
	sweep := `{"channel":"author","nonce":"1","amount":"` + before + `"}`
	r := caCall(t, ct, caC3ID, "sweepUnallocated", sweep, []string{"hive:g1"}, h+1, true)
	assert.Contains(t, r.Ret, `"swept":false`, "one guardian is below the 2-of-3 threshold")

	// ...and g2 agrees. The declared amount is what they attest over, so the vote
	// merges even though the live pool is a chain value others can move.
	r = caCall(t, ct, caC3ID, "sweepUnallocated", sweep, []string{"hive:g2"}, h+2, true)
	assert.Contains(t, r.Ret, `"swept":"`+before+`"`,
		"two guardians declaring the same amount must reach the threshold")
	assert.Equal(t, "0", alState(t, ct, "unalloc|author"))
}

// A sweep must not execute against a pool that changed after it was proposed: the
// guardians approved an amount, not a blank cheque.
func TestSweepAttest_RefusesWhenThePoolMovedSinceProposal(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3Policy(t, "0", "hive:reporter", "1", "2", "hive:g1,hive:g2,hive:g3", "2")

	const h = 5000
	caCall(t, ct, caC3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`,
		[]string{"hive:g1"}, h, true)
	caCall(t, ct, caC3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`,
		[]string{"hive:g2"}, h, true)

	// both guardians declare an amount that is NOT what the pool holds
	sweep := `{"channel":"author","nonce":"1","amount":"999999"}`
	caCall(t, ct, caC3ID, "sweepUnallocated", sweep, []string{"hive:g1"}, h+1, true)
	r := caCall(t, ct, caC3ID, "sweepUnallocated", sweep, []string{"hive:g2"}, h+2, false)
	caFailedFor(t, r, "pool no longer holds")
}

// An explicit amount is required: without it the attestation would fall back to a
// chain value again.
func TestSweepAttest_AmountIsRequired(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3Policy(t, "0", "hive:reporter", "1", "2", "hive:g1,hive:g2,hive:g3", "2")
	r := caCall(t, ct, caC3ID, "sweepUnallocated", `{"channel":"author","nonce":"1"}`,
		[]string{"hive:g1"}, 5000, false)
	caFailedFor(t, r, "amount required")
}
