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

// A root the verifier can never match must not be accepted.
//
// submitRoot validates the root with hexToBytes, and hexNibble accepts A-F as well
// as a-f (c3-distributor/contract/main.go:1396). The root is then stored VERBATIM.
// verifyProof ends with `bytesToHex(h) == root` (:1455), and bytesToHex emits from
// the constant "0123456789abcdef" (:1403) — always lowercase.
//
// So a root carrying any uppercase hex digit passes submitRoot, passes finalizeEpoch
// (which only checks presence), and then fails the equality test in every claim no
// matter how correct the proof is. The root is immutable by design, and once the
// challenge window elapses cancelEpoch refuses — so the whole epoch's funding is
// unreachable.
//
// The reference reporter is safe: it emits hex.EncodeToString, which is lowercase,
// and PlanOpts.Validate rejects non-lowercase. The exposure is every OTHER caller —
// the hand-submission path `reporter root` exists to support, a re-implemented
// reporter, or any tooling that upper-cases a hash. submitRoot is a public entrypoint
// that accepts a form its own verifier cannot match.

func TestRootHexCase_UppercaseRootIsRefused(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:alice": big.NewInt(60), "hive:bob": big.NewInt(40)})

	upper := strings.ToUpper(tree.Root())
	r := call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, upper),
		"hive:reporter", 1, false)
	caFailedFor(t, r, "root must be lowercase hex")
	assert.Empty(t, alState(t, ct, "root|author|0"),
		"a root no proof can ever match must not be committed")
}

// Mixed case is the likelier accident than fully uppercase — a hand-edited value, a
// spreadsheet, a tool that title-cases.
func TestRootHexCase_MixedCaseRootIsRefused(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:100"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{"hive:alice": big.NewInt(100)})

	root := tree.Root()
	mixed := strings.ToUpper(root[:8]) + root[8:]
	if mixed == root {
		t.Skip("this root has no letters in its first 8 digits; nothing to upper-case")
	}
	r := call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, mixed),
		"hive:reporter", 1, false)
	caFailedFor(t, r, "root must be lowercase hex")
}

// The honest path must be untouched: a lowercase root commits, finalizes and pays.
func TestRootHexCase_LowercaseRootStillPays(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := ppSetup(t)

	call(t, ct, caC3ID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`,
		"hive:reporter", 1, true)
	tree := sharecore.BuildTree(map[string]*big.Int{
		"hive:alice": big.NewInt(60), "hive:bob": big.NewInt(40)})
	proof, _ := tree.Proof("hive:alice")

	call(t, ct, caC3ID, "submitRoot", fmt.Sprintf(
		`{"channel":"author","epoch":"0","root":"%s","totalShares":"100"}`, tree.Root()),
		"hive:reporter", 1, true)
	call(t, ct, caC3ID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1, true)
	call(t, ct, caC3ID, "claim", fmt.Sprintf(
		`{"channel":"author","epoch":"0","share":"60","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 5, true)
	assert.NotEmpty(t, alState(t, ct, "claimed|author|0|hive:alice"))
}
