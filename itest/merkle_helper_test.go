package itest_test

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"

	"vsc-node/lib/test_utils"
)

// Test helpers for the merkle share book.
//
// Publishing a book used to be one submitShares call with a literal entries string,
// and claiming used to need nothing but the caller. Under the commitment both grow a
// step — the leaves are logged in pages, a root is submitted, and a claimant carries
// a proof — so these exist to keep the tests that are ABOUT something else (auth,
// veto, sweeps, rounding) readable.
//
// The tree is the REPORTER's implementation, not a second copy: a helper with its own
// merkle code would let the two drift and the tests would keep passing while real
// claims failed.

// book is a published share book plus the tree needed to prove against it.
type book struct {
	tree  *sharecore.Tree
	total *big.Int
}

// shareBook builds the tree for a set of shares without touching the chain.
func shareBook(shares map[string]int64) *book {
	m := make(map[string]*big.Int, len(shares))
	total := new(big.Int)
	for a, v := range shares {
		m[a] = big.NewInt(v)
		total.Add(total, big.NewInt(v))
	}
	return &book{tree: sharecore.BuildTree(m), total: total}
}

// publish logs the leaves in pages and submits the root, as the reporter does.
// perPage of 0 puts everything in one page.
func (b *book) publish(t *testing.T, ct *test_utils.ContractTest,
	dist, ch, ep, reporter string, height uint64, perPage int) {
	t.Helper()
	if perPage <= 0 {
		perPage = len(b.tree.Leaves)
	}
	page := 0
	for i := 0; i < len(b.tree.Leaves); i += perPage {
		end := i + perPage
		if end > len(b.tree.Leaves) {
			end = len(b.tree.Leaves)
		}
		var sb strings.Builder
		for j := i; j < end; j++ {
			if j > i {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "%s:%s", b.tree.Leaves[j].Account, b.tree.Leaves[j].Share)
		}
		call(t, ct, dist, "submitShares", fmt.Sprintf(
			`{"channel":"%s","epoch":"%s","page":"%d","entries":"%s"}`, ch, ep, page, sb.String()),
			reporter, height, true)
		page++
	}
	b.submitRoot(t, ct, dist, ch, ep, reporter, height)
}

// submitRoot posts the commitment alone, for tests that publish leaves themselves.
func (b *book) submitRoot(t *testing.T, ct *test_utils.ContractTest,
	dist, ch, ep, reporter string, height uint64) {
	t.Helper()
	call(t, ct, dist, "submitRoot", fmt.Sprintf(
		`{"channel":"%s","epoch":"%s","root":"%s","totalShares":"%s","accounts":"%d"}`,
		ch, ep, b.tree.Root(), b.total.String(), len(b.tree.Leaves)), reporter, height, true)
}

// claimFor builds the payload an account needs to claim: its share and its proof.
func (b *book) claimFor(t *testing.T, ch, ep, acct string) string {
	t.Helper()
	proof, ok := b.tree.Proof(acct)
	if !ok {
		t.Fatalf("%s is not in the share book", acct)
	}
	var share string
	for _, l := range b.tree.Leaves {
		if l.Account == acct {
			share = l.Share
			break
		}
	}
	return fmt.Sprintf(`{"channel":"%s","epoch":"%s","share":"%s","proof":"%s"}`,
		ch, ep, share, strings.Join(proof, ","))
}

// claimForged builds a claim payload with a share the account is not entitled to,
// keeping its real proof — the shape an inflation attempt takes.
func (b *book) claimForged(t *testing.T, ch, ep, acct, share string) string {
	t.Helper()
	proof, ok := b.tree.Proof(acct)
	if !ok {
		t.Fatalf("%s is not in the share book", acct)
	}
	return fmt.Sprintf(`{"channel":"%s","epoch":"%s","share":"%s","proof":"%s"}`,
		ch, ep, share, strings.Join(proof, ","))
}

// publishEntries takes the SAME "acct:share,acct:share" string the old tests passed
// to submitShares, publishes it as a merkle book, and hands back the tree.
//
// Shaped to match the existing call sites deliberately: most of these tests are about
// auth, vetoes, sweeps or rounding and merely need a share book to exist, so the
// migration should not require rewriting what each one is actually testing.
func publishEntries(t *testing.T, ct *test_utils.ContractTest,
	dist, ch, ep, entries, reporter string, height uint64) *book {
	t.Helper()
	// The contract's own rules, from sharecore. This used to be a hand-written copy
	// and it had already drifted — it rejected the system: domain applyEntries
	// accepts, so a book containing one committed a root over a different set than
	// the chain published, and every claim against it would fail.
	parsed, _ := sharecore.ParseEntries(entries)
	shares := map[string]int64{}
	for a, v := range parsed {
		shares[a] = v.Int64()
	}
	b := shareBook(shares)
	call(t, ct, dist, "submitShares", fmt.Sprintf(
		`{"channel":"%s","epoch":"%s","page":"0","entries":"%s"}`, ch, ep, entries),
		reporter, height, true)
	b.submitRoot(t, ct, dist, ch, ep, reporter, height)
	return b
}
