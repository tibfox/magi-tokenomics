package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// The merkle share book, end to end through the REAL engine: the reporter's tree
// (reporter/sharecore/merkle.go) against the contract's verifier (c3-distributor).
//
// Two independent implementations of the same construction have to agree exactly —
// one in Go on a server, one compiled to wasm — and nothing but a test that runs both
// can establish that. A disagreement would not be subtle: every claim would fail.

const mkDist = "vsc1BjW8pTNvVfBqYmZRk4LhCcXt3sJd7GqPzE"

// mkSetup wires token + C2 + a distributor with one content channel, funded.
func mkSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(mkDist, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:10000"}`, tokenID, mkDist), owner, 0, true)
	call(t, &ct, mkDist, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		tokenID, c2ID), owner, 0, true)
	call(t, &ct, mkDist, "addChannel", `{"channel":"content","bucket":"content","window":"1",`+
		`"reporterMode":"0","reporterAuth":"hive:creporter","reporterThreshold":"1","role":"content"}`,
		owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, mkDist, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:anyone", 1, true)
	return &ct
}

// mkShares builds a share set with the two claimable accounts plus a tail.
func mkShares(n int) map[string]*big.Int {
	m := map[string]*big.Int{
		"hive:alice": big.NewInt(600),
		"hive:bob":   big.NewInt(400),
	}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("hive:u%03d", i)] = big.NewInt(int64(100 + i))
	}
	return m
}

// mkPublish logs the leaves in pages and submits the root, the way the reporter does.
func mkPublish(t *testing.T, ct *test_utils.ContractTest, tree *sharecore.Tree, total *big.Int) {
	t.Helper()
	const perPage = 40
	page := 0
	for i := 0; i < len(tree.Leaves); i += perPage {
		end := i + perPage
		if end > len(tree.Leaves) {
			end = len(tree.Leaves)
		}
		var b strings.Builder
		for j := i; j < end; j++ {
			if j > i {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s:%s", tree.Leaves[j].Account, tree.Leaves[j].Share)
		}
		call(t, ct, mkDist, "submitShares", fmt.Sprintf(
			`{"channel":"content","epoch":"0","page":"%d","entries":"%s"}`, page, b.String()),
			"hive:creporter", 1, true)
		page++
	}
	call(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s","accounts":"%d"}`,
		tree.Root(), total.String(), len(tree.Leaves)), "hive:creporter", 1, true)
	call(t, ct, mkDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, true)
}

// THE HEADLINE: a proof built by the reporter verifies in the contract, and pays.
func TestMerkleClaim_ReporterProofVerifiesOnChain(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(50)
	tree := sharecore.BuildTree(shares)
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	mkPublish(t, ct, tree, total)

	proof, ok := tree.Proof("hive:alice")
	assert.True(t, ok, "alice must have a proof")
	r := call(t, ct, mkDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"600","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 3, true)

	// funded * share / totalShares
	want := new(big.Int).Mul(big.NewInt(100000), big.NewInt(600))
	want.Div(want, total)
	assert.EqualValues(t, want.Int64(), c17I64(t, r.Ret, "claimed"),
		"payout must be funded*share/totalShares: "+r.Ret)

	// What the proof costs the CLAIMANT — the cost merkle moves onto them. A claim is
	// the one call an ordinary holder makes and they may hold no HBD at all, so this
	// has to stay inside the 10,000 free tier with room to spare.
	t.Logf("claim with a %d-element proof: %d RC (liquid claim without one was ~830)",
		len(proof), r.RcUsed)
	if r.RcUsed > 10000 {
		t.Fatalf("a proof claim at %d RC no longer fits the free tier — an earner with "+
			"no HBD could not collect", r.RcUsed)
	}
}

// A proof for one account must not pay another. The leaf is built from msg.caller,
// which the claimant does not choose, so a stolen proof is worthless.
func TestMerkleClaim_ProofCannotBeUsedByAnotherAccount(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(50)
	tree := sharecore.BuildTree(shares)
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	mkPublish(t, ct, tree, total)

	proof, _ := tree.Proof("hive:alice")
	// bob presents alice's proof and alice's amount
	call(t, ct, mkDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"600","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:bob", 3, false)
}

// Nor may a claimant inflate the amount: the leaf binds account AND share.
//
// ★ The inflation here is MODEST — 700 against a real 600 — and that is the whole
// point. The first version claimed 999999, which at funded*share/totalShares asked
// for more tokens than the contract holds, so the TRANSFER failed and the test passed
// with proof verification deliberately disabled. It proved the balance stops a absurd
// claim, not that the proof stops a plausible one. A 17% overclaim is affordable, and
// only the proof refuses it.
func TestMerkleClaim_ShareCannotBeInflated(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(50)
	tree := sharecore.BuildTree(shares)
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	mkPublish(t, ct, tree, total)

	proof, _ := tree.Proof("hive:alice")
	call(t, ct, mkDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"700","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:alice", 3, false)
}

// An account that was never in the tree cannot invent a path to the root.
func TestMerkleClaim_AbsentAccountCannotClaim(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(50)
	tree := sharecore.BuildTree(shares)
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	mkPublish(t, ct, tree, total)

	proof, _ := tree.Proof("hive:alice")
	call(t, ct, mkDist, "claim", fmt.Sprintf(
		`{"channel":"content","epoch":"0","share":"600","proof":"%s"}`, strings.Join(proof, ",")),
		"hive:mallory", 3, false)
	// and with no proof at all
	call(t, ct, mkDist, "claim",
		`{"channel":"content","epoch":"0","share":"600","proof":""}`, "hive:mallory", 3, false)
}

// The root is immutable once set. A re-pointable root would let a finalized epoch be
// aimed at a different share book after the fact.
func TestMerkleClaim_RootIsWriteOnce(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(10)
	tree := sharecore.BuildTree(shares)
	total := new(big.Int)
	for _, v := range shares {
		total.Add(total, v)
	}
	call(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s"}`, tree.Root(), total),
		"hive:creporter", 1, true)
	// a second root, from a different share book
	other := sharecore.BuildTree(mkShares(11))
	call(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s"}`, other.Root(), total),
		"hive:creporter", 2, false)
}

// Finalizing without a root would lock the funding away: no claim could ever verify.
func TestMerkleClaim_FinalizeRequiresARoot(t *testing.T) {
	ct := mkSetup(t)
	call(t, ct, mkDist, "submitShares",
		`{"channel":"content","epoch":"0","page":"0","entries":"hive:alice:600"}`,
		"hive:creporter", 1, true)
	call(t, ct, mkDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, false)
}
