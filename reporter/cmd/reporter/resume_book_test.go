package main

import (
	"strings"
	"testing"

	"magi_token/reporter/submit"
)

// A resume must not splice two different books together.
//
// buildPlan recomputes the whole book on every run and repaginates it, but
// chainApplied decides a page is done purely from `ssdone|<ch>|<ep>|<pageIndex>`. It
// never reads `root|<ch>|<ep>` or `totalShares|<ch>|<ep>`, so nothing compares this
// run's book against what the epoch already holds.
//
// If the book changes between a partial run and its resume — a repagination, or a
// load-balanced api.hive.blog backend returning one more post — the indices already
// marked done are treated as satisfying the NEW pagination, and a contiguous middle
// slice of the canonical entry list is never published. On chain applyEntries
// accumulates pagesum from what actually landed while submitRoot recorded the full
// declared totalShares, so finalizeEpoch aborts on the pagesum guard forever. The
// root is immutable, so the declared total can never be corrected.
//
// And it is invisible: broadcast.Send returns an L1 txid, no L2 receipt is read
// anywhere, so the run records progress and prints "epoch N submitted" every time.
//
// The committed root is the epoch's identity. If one is already on chain and this
// run's book does not produce it, the two are different books and resuming would
// splice them.

func resumeApp(t *testing.T, chainRoot string) (*app, *fakeState) {
	t.Helper()
	kv := map[string]string{
		"funded|content|7":      "50000",
		"ssdone|content|7|0":    "1",
		"ch_rMode|content":      "0",
	}
	if chainRoot != "" {
		kv["root|content|7"] = chainRoot
	}
	return gateApp(t, kv, nil)
}

func resumePlan(root string, pages int) submit.Plan {
	pl := submit.Plan{Epoch: "7"}
	for i := 0; i < pages; i++ {
		pl.Calls = append(pl.Calls, submit.Call{
			Action:  "submitShares",
			Payload: `{"channel":"content","epoch":"7","page":"` + itoa(i) + `","entries":"hive:a:1"}`,
		})
	}
	pl.Calls = append(pl.Calls, submit.Call{
		Action:  "submitRoot",
		Payload: `{"channel":"content","epoch":"7","root":"` + root + `","totalShares":"100"}`,
	})
	pl.Calls = append(pl.Calls, submit.Call{Action: "finalizeEpoch", Payload: `{"channel":"content","epoch":"7"}`})
	return pl
}

const rootA = "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
const rootB = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee"

// THE FINDING: the epoch already committed rootA, this run computes rootB.
func TestResumeBook_DivergentBookIsRefused(t *testing.T) {
	a, _ := resumeApp(t, rootA)
	err := a.assertBookMatchesChain(resumePlan(rootB, 3))
	if err == nil {
		t.Fatal("a run whose book does not produce the committed root must be refused: " +
			"resuming would publish this book's pages against the other book's root, and the " +
			"epoch could then never satisfy the on-chain pagesum guard")
	}
	if !strings.Contains(err.Error(), "root") {
		t.Errorf("the error must name the root mismatch, got: %v", err)
	}
}

// The ordinary resume — same book, same root — must proceed.
func TestResumeBook_SameBookResumes(t *testing.T) {
	a, _ := resumeApp(t, rootA)
	if err := a.assertBookMatchesChain(resumePlan(rootA, 3)); err != nil {
		t.Fatalf("an unchanged book must resume cleanly: %v", err)
	}
}

// A first run, before any root is committed, has nothing to compare against.
func TestResumeBook_NoCommittedRootIsFine(t *testing.T) {
	a, _ := resumeApp(t, "")
	if err := a.assertBookMatchesChain(resumePlan(rootA, 3)); err != nil {
		t.Fatalf("a first run must not be blocked: %v", err)
	}
}

// Case must not decide it: the contract stores the root lowercase, and a caller
// comparing a differently-cased rendering would refuse an identical book.
func TestResumeBook_CaseInsensitiveComparison(t *testing.T) {
	a, _ := resumeApp(t, rootA)
	if err := a.assertBookMatchesChain(resumePlan(strings.ToUpper(rootA), 3)); err != nil {
		t.Fatalf("the same root differently cased is the same root: %v", err)
	}
}
