package sharecore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"
)

func shareSet(n int) map[string]*big.Int {
	m := map[string]*big.Int{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("hive:u%03d", i)] = big.NewInt(int64(1000 + i))
	}
	return m
}

// Every account in the tree can prove itself. The base case, at sizes that exercise
// odd levels, a single leaf, and the 502-earner shape a real epoch produces.
func TestMerkle_EveryLeafProves(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7, 8, 502} {
		shares := shareSet(n)
		tree := BuildTree(shares)
		root := tree.Root()
		if root == "" {
			t.Fatalf("n=%d produced no root", n)
		}
		for acct, amt := range shares {
			proof, ok := tree.Proof(acct)
			if !ok {
				t.Fatalf("n=%d: no proof for %s", n, acct)
			}
			if !VerifyProof(acct, amt.String(), proof, root) {
				t.Fatalf("n=%d: %s cannot prove its own share", n, acct)
			}
		}
	}
}

// THE CENTRAL SECURITY PROPERTY. A proof binds the account AND the amount, so it
// cannot be replayed under another name or inflated. Either change breaks the leaf
// hash and the recomputed root stops matching.
func TestMerkle_ProofDoesNotTransferOrInflate(t *testing.T) {
	shares := shareSet(50)
	tree := BuildTree(shares)
	root := tree.Root()

	proof, _ := tree.Proof("hive:u010")
	real := shares["hive:u010"].String()

	if !VerifyProof("hive:u010", real, proof, root) {
		t.Fatal("the honest case must verify")
	}
	// same proof, someone else's name
	if VerifyProof("hive:u011", real, proof, root) {
		t.Fatal("a proof must not verify for a DIFFERENT account")
	}
	// same proof and name, a larger number
	if VerifyProof("hive:u010", "999999999", proof, root) {
		t.Fatal("a proof must not verify for an INFLATED share")
	}
	// an account that is not in the tree at all
	if _, ok := tree.Proof("hive:nobody"); ok {
		t.Fatal("an absent account must not receive a proof")
	}
}

// A truncated or padded proof must not verify. Silently accepting a short path would
// let a claimant prove membership of an internal node rather than a leaf.
func TestMerkle_MalformedProofsAreRejected(t *testing.T) {
	shares := shareSet(50)
	tree := BuildTree(shares)
	root := tree.Root()
	proof, _ := tree.Proof("hive:u010")
	amt := shares["hive:u010"].String()

	if len(proof) < 2 {
		t.Fatal("need a multi-level proof for this test")
	}
	if VerifyProof("hive:u010", amt, proof[:len(proof)-1], root) {
		t.Fatal("a truncated proof must not verify")
	}
	if VerifyProof("hive:u010", amt, append(append([]string{}, proof...), proof[0]), root) {
		t.Fatal("a padded proof must not verify")
	}
	if VerifyProof("hive:u010", amt, []string{"nothex"}, root) {
		t.Fatal("a non-hex proof element must not verify")
	}
	if VerifyProof("hive:u010", amt, []string{"aabb"}, root) {
		t.Fatal("a wrong-length proof element must not verify")
	}
}

// DETERMINISM. The root is a function of the share set alone — not of map iteration
// order, not of insertion order. Attest depends on two machines agreeing byte for
// byte, and the root is now the thing they must agree on.
func TestMerkle_RootIsDeterministic(t *testing.T) {
	a := BuildTree(shareSet(101)).Root()
	for i := 0; i < 20; i++ {
		if got := BuildTree(shareSet(101)).Root(); got != a {
			t.Fatalf("root changed between builds: %s vs %s", a, got)
		}
	}
	// a different share set must produce a different root
	other := shareSet(101)
	other["hive:u050"] = big.NewInt(999)
	if BuildTree(other).Root() == a {
		t.Fatal("changing one share must change the root")
	}
}

// An odd node is PROMOTED, never duplicated.
//
// Duplicating it is CVE-2012-2459: a tree over [A,B,C] becomes H(H(A,B), H(C,C)),
// which is exactly the root a tree over [A,B,C,C] would produce — so an attacker can
// claim a leaf set the reporter never published.
//
// ★ The first version of this test compared a 3-leaf set against a 4-leaf set with a
// DIFFERENT fourth account, whose leaf hash differs either way. It passed with
// duplication deliberately reintroduced. This asserts the shape directly instead:
// with promotion the root is H(H(A,B), C); with duplication it is H(H(A,B), H(C,C)).
func TestMerkle_OddNodeIsPromotedNotDuplicated(t *testing.T) {
	shares := map[string]*big.Int{
		"hive:a": big.NewInt(1), "hive:b": big.NewInt(2), "hive:c": big.NewInt(3),
	}
	got := BuildTree(shares).Root()

	la := LeafHash("hive:a", "1")
	lb := LeafHash("hive:b", "2")
	lc := LeafHash("hive:c", "3")

	promoted := hexOf(nodeHash(nodeHash(la, lb), lc))
	duplicated := hexOf(nodeHash(nodeHash(la, lb), nodeHash(lc, lc)))

	if got != promoted {
		t.Fatalf("odd leaf must be PROMOTED: root %s, expected %s", got, promoted)
	}
	if got == duplicated {
		t.Fatal("root matches the DUPLICATED construction — CVE-2012-2459")
	}
}

// Domain separation. The attack is that an internal node's 64-byte preimage could be
// offered as a leaf's preimage, proving membership of something never in the tree.
// The 0x00/0x01 tags make the two preimage spaces disjoint.
//
// ★ The first version compared a leaf hash to a node hash and passed with both tags
// REMOVED, because a 9-byte preimage and a 64-byte one hash differently anyway. This
// asserts the tags are actually applied, by comparing against the untagged digests.
func TestMerkle_LeavesAndNodesAreDomainSeparated(t *testing.T) {
	if LeafHash("hive:a", "1") == sha256.Sum256([]byte("hive:a|1")) {
		t.Fatal("leaf hash is UNTAGGED — an internal node preimage could pose as a leaf")
	}
	x, y := LeafHash("hive:a", "1"), LeafHash("hive:b", "2")
	lo, hi := x, y
	if !lessHash(x, y) {
		lo, hi = y, x
	}
	if nodeHash(x, y) == sha256.Sum256(append(lo[:], hi[:]...)) {
		t.Fatal("node hash is UNTAGGED — a leaf preimage could pose as an internal node")
	}
	// and the two domains must not meet
	if LeafHash("hive:a", "1") == nodeHash(x, y) {
		t.Fatal("leaf and node hashes collided")
	}
}

func hexOf(h [32]byte) string { return hex.EncodeToString(h[:]) }
