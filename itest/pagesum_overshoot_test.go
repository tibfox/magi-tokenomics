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

// A page that would push pagesum ABOVE the committed total must be refused, not
// accepted into a state nothing can leave.
//
// submitShares gates only on status being unset, and status is set only by finalize
// or cancel — so pages stay acceptable after submitRoot. submitRoot is write-once and
// pagesum only ever accumulates, so once pagesum exceeds totalShares there is no move
// that restores equality: the total cannot be raised (the root is immutable) and
// pagesum cannot be lowered (nothing decrements it). finalizeEpoch can then never
// pass, and the epoch's whole funding reaches the treasury via a guardian stale-cancel
// instead of the earners.
//
// The comment on finalizeEpoch's guard claimed the refusal "is self-healing: once the
// pages are complete the same authority can vote again and it goes through". That is
// true only BELOW the total — submit the missing page. Above it, nothing heals.
//
// Refusing the overshooting page instead is recoverable by construction: the page
// simply does not apply, everything already published stays valid, and an epoch whose
// pages match its committed total still finalizes.
//
// Note the fix is NOT to close submitShares once a root exists. In attest mode a page
// can still be gathering votes when the root commits, and locking it out would strand
// the epoch below its total instead of above it — the same trap, mirrored.


// THE FINDING.
func TestPageSumOvershoot_LatePageBeyondTheTotalIsRefused(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{"hive:alice": big.NewInt(100)})
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)
	assert.Equal(t, "100", alState(t, ct, "pagesum|author|0"))

	// A page nobody committed to: its earner is not in the tree and could never
	// claim, but accepting it would make the epoch unfinalizable forever.
	r := call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"1","entries":"hive:bob:50"}`,
		"hive:reporter", 2, false)
	caFailedFor(t, r, "beyond the committed total")

	assert.Equal(t, "100", alState(t, ct, "pagesum|author|0"),
		"a refused page must not have moved the accumulator")

	// and the epoch still finalizes and pays, which is the whole point
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 2, true)
	proof, _ := tree.Proof("hive:alice")
	call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"100","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, true)
}

// A page that still FITS under the committed total must apply — this is the attest
// case, where a page can be gathering votes when the root commits. Locking those out
// would strand the epoch below its total: the same trap, mirrored.
func TestPageSumOvershoot_LatePageWithinTheTotalStillApplies(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:60"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:alice": big.NewInt(60), "hive:bob": big.NewInt(40)})
	// the root commits BOTH earners; bob's page has not landed yet
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"1","entries":"hive:bob:40"}`,
		"hive:reporter", 2, true)
	assert.Equal(t, "100", alState(t, ct, "pagesum|author|0"),
		"a page that completes the book must still apply after the root")

	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 2, true)
	assert.Equal(t, "finalized", alState(t, ct, "status|author|0"))
}

// Before a root exists there is no total to overshoot, so pages accumulate freely.
func TestPageSumOvershoot_NoRootYetMeansNoCeiling(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`,
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"1","entries":"hive:bob:900"}`,
		"hive:reporter", 1, true)
	assert.Equal(t, "1000", alState(t, ct, "pagesum|author|0"))
}
