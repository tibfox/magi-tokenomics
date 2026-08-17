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

// PROBE — not a regression test. Asks whether a C3 epoch can end up holding money
// that no call can ever move, and quantifies how much.
//
// Under the merkle share book `totalShares` is DECLARED by the reporter rather than
// summed on chain. claim already refuses the under-declared direction: paid|<ch>|<ep>
// accumulates and a payout that would push it past `funded` aborts, so nobody can be
// overpaid. The OVER-declared direction has no such check, because there is nothing
// on chain to check against — the leaves live in the log.
//
// Over-declaring does not overpay anyone. It shrinks every payout proportionally and
// leaves the difference sitting in funded|<ch>|<ep>. This asks what can retrieve it.
func TestProbe_C3EpochResidueHasNoRecoveryPath(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	// One earner, the whole book. Declare DOUBLE the true total: alice should be paid
	// half of what the epoch holds, and the other half becomes residue.
	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`,
		"hive:reporter", 1, true)

	tree := sharecore.BuildTree(map[string]*big.Int{"hive:alice": big.NewInt(100)})
	proof, _ := tree.Proof("hive:alice")
	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"200"}`, tree.Root()),
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1, true)

	funded := parseBigStr(alState(t, ct, "funded|author|0"))
	if funded.Sign() <= 0 {
		t.Fatal("epoch is not funded — the probe would prove nothing")
	}

	call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"100","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, true)

	paid := parseBigStr(alState(t, ct, "paid|author|0"))
	residue := new(big.Int).Sub(funded, paid)
	t.Logf("funded %s, paid %s, residue %s (%.0f%% of the epoch)",
		funded, paid, residue,
		100*float64(residue.Int64())/float64(funded.Int64()))

	if residue.Sign() <= 0 {
		t.Fatal("no residue was produced — the probe did not reproduce the condition")
	}

	// Everything that could plausibly move it.
	//
	// cancelEpoch is the guardian's veto and the only thing that returns funding to
	// unalloc| — but it refuses a finalized epoch once the challenge window has
	// elapsed, which is exactly when a residue becomes visible (you only know what
	// went unclaimed after claims have run).
	r := call(t, ct, caC3ID, "cancelEpoch", `{"channel":"author","epoch":"0"}`,
		"hive:guardian", 5, false)
	caFailedFor(t, r, "challenge window elapsed")

	// sweepUnallocated only ever moves unalloc|<ch>, which a finalized epoch never
	// contributes to.
	assert.Empty(t, alState(t, ct, "unalloc|author"),
		"a finalized epoch's residue does not reach the unallocated pool")
	r = call(t, ct, caC3ID, "sweepUnallocated", `{"channel":"author","nonce":"1"}`,
		"hive:guardian", 5, false)
	caFailedFor(t, r, "nothing to sweep")

	// And alice cannot claim twice to collect the rest.
	r = call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"100","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 6, false)
	caFailedFor(t, r, "already claimed")

	t.Logf("RESIDUE IS UNREACHABLE: %s units held by the contract with no entrypoint "+
		"able to move them. C1 has sweepEmptyEpoch for exactly this; C3 has no equivalent.",
		residue)
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
