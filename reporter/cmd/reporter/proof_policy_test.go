package main

import (
	"strings"
	"testing"
)

// `reporter proof` must keep working after a policy change, and must refuse a book
// the chain did not commit.
//
// cmdProof went through compute(), which applies both SUBMIT-path gates. After a
// setPolicy from digest A to B, an epoch funded under A satisfies neither config: the
// old one fails verifyPolicy (A != current B) and the new one fails verifyEpochPolicy
// (B != pin A). So every pre-change epoch lost its proof path — while
// docs/how-it-works.md and reporter/README.md both promise it as the route that
// "needs no indexer at all… so you are never locked out by someone else's server
// being down", and proofsvc was separately returning 503 on everything.
//
// The gates are right for submitting and wrong for reading. What replaces them is
// stricter: the computed book must reproduce the epoch's COMMITTED ROOT. A digest is
// a proxy for "scored the agreed way"; the root is the thing itself. That check did
// not exist before — cmdProof verified its proof against its OWN tree, so a divergent
// book produced a proof that verified locally and failed at the point of payment.

func proofApp(t *testing.T, chainRoot string) (*app, *fakeState) {
	t.Helper()
	kv := map[string]string{}
	if chainRoot != "" {
		kv["root|content|4"] = chainRoot
	}
	return gateApp(t, kv, nil)
}

const prRoot = "cc11223344556677889900aabbccddeeff00112233445566778899aabbccddee"

// A book reproducing the committed root is accepted regardless of policy digests.
func TestProofPolicy_MatchingRootIsAccepted(t *testing.T) {
	a, _ := proofApp(t, prRoot)
	if err := a.assertRootMatchesChain(4, prRoot); err != nil {
		t.Fatalf("a book that reproduces the committed root must be usable for a proof, "+
			"whatever the policy digest says: %v", err)
	}
}

// Case must not decide it — the contract stores lowercase.
func TestProofPolicy_RootComparisonIsCaseInsensitive(t *testing.T) {
	a, _ := proofApp(t, prRoot)
	if err := a.assertRootMatchesChain(4, strings.ToUpper(prRoot)); err != nil {
		t.Fatalf("the same root differently cased is the same root: %v", err)
	}
}

// THE CHECK THAT DID NOT EXIST: a divergent book must be refused, not handed over.
func TestProofPolicy_DivergentBookIsRefused(t *testing.T) {
	a, _ := proofApp(t, prRoot)
	err := a.assertRootMatchesChain(4, "dd11223344556677889900aabbccddeeff00112233445566778899aabbccddee")
	if err == nil {
		t.Fatal("a book that does not reproduce the committed root must be refused: its " +
			"proof verifies locally and then fails on chain, which costs the claimant RC " +
			"to discover")
	}
	if !strings.Contains(err.Error(), "would not verify on chain") {
		t.Errorf("the error must say the proof would fail at the point of payment, got: %v", err)
	}
}

// An epoch with no commitment has nothing to prove against, and must say so rather
// than emit a proof for a root that does not exist.
func TestProofPolicy_NoCommittedRootIsRefused(t *testing.T) {
	a, _ := proofApp(t, "")
	err := a.assertRootMatchesChain(4, prRoot)
	if err == nil {
		t.Fatal("an epoch with no committed root must not yield a proof")
	}
	if !strings.Contains(err.Error(), "no committed root") {
		t.Errorf("the error must name the missing commitment, got: %v", err)
	}
}
