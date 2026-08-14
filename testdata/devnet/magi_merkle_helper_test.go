package devnet

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// Shared merkle helpers for the devnet suites.
//
// Under the commitment a share book is no longer just a submitShares call: the
// leaves are published, a root is committed, and every claimant carries a proof.
// Suites that push a book by hand — most of them — need all three, and doing it
// per-suite is how six of them ended up publishing shares with no root and
// claiming with no proof while still being described as migrated.
//
// The arithmetic is the REPORTER's, shelled out to rather than reimplemented. A
// second merkle implementation living in the test tree could drift from the one
// that computes real roots, and the tests would keep passing while production
// claims failed — which is the whole failure mode these helpers exist to prevent.

// merkleBook is a share book plus the commitment for it.
type merkleBook struct {
	entries     string
	Root        string   `json:"root"`
	TotalShares string   `json:"total_shares"`
	Accounts    int      `json:"accounts"`
	Skipped     []string `json:"skipped"`
}

// buildBook computes the root for "acct:share" pairs, in a canonical order so two
// callers listing the same shares get the same root.
func buildBook(t *testing.T, shares map[string]string) *merkleBook {
	t.Helper()
	accts := make([]string, 0, len(shares))
	for a := range shares {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	parts := make([]string, 0, len(accts))
	for _, a := range accts {
		parts = append(parts, a+":"+shares[a])
	}
	return bookFromEntries(t, strings.Join(parts, ","))
}

// bookFromEntries computes the root for an entries string exactly as submitted.
func bookFromEntries(t *testing.T, entries string) *merkleBook {
	t.Helper()
	out, err := exec.Command(reporterBin, "root", "-entries", entries, "-json").CombinedOutput()
	if err != nil {
		t.Fatalf("reporter root for %q failed: %v\n%s", entries, err, out)
	}
	b := &merkleBook{entries: entries}
	if err := json.Unmarshal(out, b); err != nil {
		t.Fatalf("reporter root output is not json: %v\n%s", err, out)
	}
	// A skipped entry is a no-op inside a SUCCESSFUL transaction: the page applies,
	// the earner is never paid, and nothing in the result says so. In a test that
	// silence would show up much later as an unexplained zero payout, so fail here.
	if len(b.Skipped) > 0 {
		t.Fatalf("reporter root skipped entries in %q: %v — the contract would skip them "+
			"too, and the test's expected payouts would be wrong", entries, b.Skipped)
	}
	return b
}

// SubmitPayload is the submitShares payload publishing the whole book as one page.
func (b *merkleBook) SubmitPayload(channel, epoch string) string {
	return fmt.Sprintf(`{"channel":"%s","epoch":"%s","page":"0","entries":"%s"}`,
		channel, epoch, b.entries)
}

// RootPayload is the submitRoot payload committing the book. finalizeEpoch refuses
// an epoch without one, because finalizing it would lock the funding away with
// nothing able to claim it.
func (b *merkleBook) RootPayload(channel, epoch string) string {
	return fmt.Sprintf(`{"channel":"%s","epoch":"%s","root":"%s","totalShares":"%s","accounts":"%d"}`,
		channel, epoch, b.Root, b.TotalShares, b.Accounts)
}

// ClaimPayload is what an account sends to claim: its share and the path proving
// that share against this epoch's committed root.
func (b *merkleBook) ClaimPayload(t *testing.T, channel, epoch, account string) string {
	t.Helper()
	out, err := exec.Command(reporterBin, "root", "-entries", b.entries,
		"-account", account, "-json").CombinedOutput()
	if err != nil {
		t.Fatalf("reporter proof for %s failed: %v\n%s", account, err, out)
	}
	var pf struct {
		Share string   `json:"share"`
		Proof []string `json:"proof"`
	}
	if err := json.Unmarshal(out, &pf); err != nil {
		t.Fatalf("reporter proof output is not json: %v\n%s", err, out)
	}
	if pf.Share == "" {
		t.Fatalf("%s is not in the share book %q", account, b.entries)
	}
	return fmt.Sprintf(`{"channel":"%s","epoch":"%s","share":"%s","proof":"%s"}`,
		channel, epoch, pf.Share, strings.Join(pf.Proof, ","))
}
