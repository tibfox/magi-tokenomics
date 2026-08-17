package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"

	"github.com/stretchr/testify/assert"
)

// An over-declared totalShares must never reach a finalized epoch.
//
// Under the merkle share book totalShares is DECLARED by the reporter rather than
// summed on chain. claim refuses the under-declared direction — paid|<ch>|<ep>
// accumulates and a payout past `funded` aborts, so nobody can be overpaid — but
// over-declaring shrinks every payout and leaves the difference in funded|<ch>|<ep>,
// which nothing could reach: cancelEpoch refuses a finalized epoch past its window
// (which is exactly when a residue becomes visible, since you only learn what went
// unclaimed after claims have run), sweepUnallocated only moves unalloc|, and no one
// can claim twice. The probe this replaced measured 30,000 of 60,000 — half an
// epoch — lost to one wrong number.
//
// finalizeEpoch now holds the declared denominator to what the pages published.
func TestPageSum_OverDeclaredTotalCannotBeFinalized(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`,
		"hive:reporter", 1, true)

	tree := sharecore.BuildTree(map[string]*big.Int{"hive:alice": big.NewInt(100)})
	proof, _ := tree.Proof("hive:alice")
	// DOUBLE the true total: alice would be paid half, the rest stranded.
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"200"}`, tree.Root()),
		"hive:reporter", 1, true)

	r := call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`,
		"hive:reporter", 1, false)
	caFailedFor(t, r, "does not equal the shares the pages published")

	// The epoch stays OPEN rather than stuck: nothing was finalized, so the guardian's
	// stale rescue can still recover the funding. Being refused here is strictly
	// better than finalizing into a residue nothing can move.
	assert.Empty(t, alState(t, ct, "status|author|0"),
		"a refused finalize must leave the epoch open so the funding stays recoverable")

	// and no claim can run against an unfinalized epoch
	r = call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"100","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, false)
	caFailedFor(t, r, "epoch not finalized")
}

// The mirror case: a root committed with the pages MISSING. The leaves were never
// logged, so nobody could build a proof against the root, and finalizing would lock
// the entire epoch away rather than a fraction of it.
func TestPageSum_RootWithoutPagesCannotBeFinalized(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	tree := sharecore.BuildTree(map[string]*big.Int{"hive:alice": big.NewInt(100)})
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)

	r := call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`,
		"hive:reporter", 1, false)
	caFailedFor(t, r, "does not equal the shares the pages published")
}

// An entry the CHAIN skipped — malformed address, non-positive share — is one the
// reporter's tree counted and the chain did not. That divergence used to dilute
// every other earner by a slice nobody could ever claim, silently. It now stops the
// epoch instead.
func TestPageSum_ASkippedEntryBlocksFinalize(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	// "alice" carries no ledger domain, so applyEntries skips it and logs why. The
	// page still applies; only its total is short.
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:bob:100,alice:50"}`,
		"hive:reporter", 1, true)
	assert.Equal(t, "1", alState(t, ct, "ssdone|author|0|0"), "the page must have applied")
	assert.Equal(t, "100", alState(t, ct, "pagesum|author|0"),
		"the skipped entry must not count toward what the chain published")

	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:bob": big.NewInt(100), "alice": big.NewInt(50)})
	r := call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"150"}`, tree.Root()),
		"hive:reporter", 1, true)
	assert.True(t, r.Success)

	r = call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`,
		"hive:reporter", 1, false)
	caFailedFor(t, r, "does not equal the shares the pages published")
}

// The honest path must still work: declare what the pages published and the epoch
// finalizes and pays normally. Without this the three refusals above would be
// satisfied by a check that refused everything.
func TestPageSum_MatchingTotalFinalizesAndPays(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:60"}`,
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"1","entries":"hive:bob:40"}`,
		"hive:reporter", 1, true)
	assert.Equal(t, "100", alState(t, ct, "pagesum|author|0"),
		"the accumulator must span pages, not just the last one")

	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:alice": big.NewInt(60), "hive:bob": big.NewInt(40)})
	proof, _ := tree.Proof("hive:alice")
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1, true)

	funded := parseBigStr(alState(t, ct, "funded|author|0"))
	call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"60","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, true)

	// alice's 60/100 of the epoch, exactly
	want := new(big.Int).Div(new(big.Int).Mul(funded, big.NewInt(60)), big.NewInt(100))
	assert.Equal(t, want.String(), alState(t, ct, "paid|author|0"))
}

// The same shape reached by ordinary means: earners who simply never claim.
// No mistake and no malice — just an inactive or lost account.
func TestProbe_C3UnclaimedShareIsLockedForever(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:50,hive:bob:50"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:alice": big.NewInt(50), "hive:bob": big.NewInt(50)})
	proofA, _ := tree.Proof("hive:alice")
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1, true)

	// only alice claims; bob's account is gone
	call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"50","proof":"%s"}`, strings.Join(proofA, ",")),
		"hive:alice", 5, true)

	funded := parseBigStr(alState(t, ct, "funded|author|0"))
	paid := parseBigStr(alState(t, ct, "paid|author|0"))
	residue := new(big.Int).Sub(funded, paid)
	t.Logf("bob never claims: %s of %s stays in the contract, claimable by bob alone, forever",
		residue, funded)

	// This one is arguably CORRECT: bob can always come back, and any deadline would
	// mean denying a slow claimant. Recorded so the two cases are not confused — the
	// probe above is a residue nobody can ever claim, this is one only bob can.
	assert.Positive(t, residue.Sign())
}

func parseBigStr(s string) *big.Int {
	v, _ := new(big.Int).SetString(strings.TrimSpace(s), 10)
	if v == nil {
		return new(big.Int)
	}
	return v
}
