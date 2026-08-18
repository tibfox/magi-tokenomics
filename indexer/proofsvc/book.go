// Package proofsvc rebuilds an epoch's share book from the indexer's copy of the
// distributor's logs, checks it against the root the contract committed, and serves
// merkle proofs to claimants.
//
// The design point is that this service is NOT trusted. It holds the share book
// because the chain no longer does — per-account state writes were ~92% of an
// epoch's cost — but every answer it gives is checkable against a 32-byte root the
// contract stores, and a claim carrying a bad proof is refused on chain. So the
// service's job is not to be believed; it is to be verifiable. Accordingly it
// refuses to serve an epoch whose rebuilt root does not match, rather than serving
// proofs that would be rejected at the point of payment with no explanation.
package proofsvc

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"magi_token/reporter/sharecore"
)

// Numeric is a Postgres NUMERIC as it arrives over the wire.
//
// The mapping declares total_shares and page_total `numeric`, magi-mongo-indexer
// creates them as NUMERIC, and Hasura serialises NUMERIC as an UNQUOTED JSON number.
// These were declared `string`, so json.Unmarshal failed on every response and both
// /proof and /root returned 503 — with an error that reads like indexer lag, so an
// operator waits for a backlog that never clears.
//
// Kept as TEXT rather than decoded to a number: a share total is a big.Int on chain
// (the 502-earner scale run commits 208,552,153,997,506,000) and routing it through
// float64 would corrupt it silently. Accepts quoted as well as unquoted, because the
// reconciliations downstream are ok-guarded on parsing this text — if a view or a
// cast ever quoted it, a silent "" would switch those checks off instead of failing.
type Numeric string

func (n Numeric) String() string { return string(n) }

func (n *Numeric) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*n = ""
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var q string
		if err := json.Unmarshal(b, &q); err != nil {
			return err
		}
		*n = Numeric(q)
		return nil
	}
	*n = Numeric(s)
	return nil
}

// SharesRow is one `shares` log the distributor emitted — a page of the book.
type SharesRow struct {
	Channel   string  `json:"channel"`
	Epoch     int64   `json:"epoch"`
	Page      int64   `json:"page"`
	Entries   int64   `json:"entries"`
	PageTotal Numeric `json:"page_total"`
	Submitted string  `json:"submitted"`
}

// RootRow is the commitment: one row per (channel, epoch).
type RootRow struct {
	Channel     string  `json:"channel"`
	Epoch       int64   `json:"epoch"`
	Root        string  `json:"root"`
	TotalShares Numeric `json:"total_shares"`
	Accounts    int64   `json:"accounts"`
}

// Book is a rebuilt, root-checked share book for one epoch.
type Book struct {
	Channel string
	Epoch   int64
	Root    string
	Total   *big.Int
	shares  map[string]*big.Int
	tree    *sharecore.Tree
	Skipped []sharecore.SkippedEntry
}

// BuildBook reassembles the book from its pages and verifies it against the
// committed root. It returns an error rather than a book on ANY disagreement.
//
// Four things can disagree, and each is reported separately because they mean
// different things operationally:
//
//   - a missing page — the indexer is behind, or a submitShares log was dropped;
//   - a page whose own logged total does not match its entries — the indexer's
//     copy of that row is corrupt;
//   - a root mismatch — the book is wrong somewhere, and no proof from it would
//     verify on chain anyway;
//   - a total or account count that differs from the committed one — the
//     denominator a payout divides by is not the one the chain will use.
//
// Serving through any of these would hand claimants proofs that fail at the point
// of payment, which is the worst possible place to discover it.
func BuildBook(root RootRow, pages []SharesRow) (*Book, error) {
	sort.Slice(pages, func(i, j int) bool { return pages[i].Page < pages[j].Page })
	for i, p := range pages {
		if p.Page != int64(i) {
			return nil, fmt.Errorf("page %d is missing from %s epoch %d: the book is "+
				"incomplete and any root rebuilt from it would be wrong",
				i, root.Channel, root.Epoch)
		}
	}

	shares := map[string]*big.Int{}
	var skipped []sharecore.SkippedEntry
	for _, p := range pages {
		pageShares, pageSkipped := sharecore.ParseEntries(p.Submitted)
		skipped = append(skipped, pageSkipped...)
		pageTotal := sharecore.TotalOf(pageShares)
		if want, ok := new(big.Int).SetString(p.PageTotal.String(), 10); ok && pageTotal.Cmp(want) != 0 {
			return nil, fmt.Errorf("page %d of %s epoch %d sums to %s but its log says %s: "+
				"the indexer's copy of that row does not match what the contract counted",
				p.Page, root.Channel, root.Epoch, pageTotal, want)
		}
		for acct, v := range pageShares {
			if cur, dup := shares[acct]; dup {
				shares[acct] = new(big.Int).Add(cur, v)
			} else {
				shares[acct] = v
			}
		}
	}

	tree := sharecore.BuildTree(shares)
	if got := tree.Root(); got != root.Root {
		return nil, fmt.Errorf("rebuilt root %s does not match the committed root %s for %s "+
			"epoch %d: this service will not serve proofs that the contract would reject",
			got, root.Root, root.Channel, root.Epoch)
	}
	total := sharecore.TotalOf(shares)
	if want, ok := new(big.Int).SetString(root.TotalShares.String(), 10); ok && total.Cmp(want) != 0 {
		return nil, fmt.Errorf("rebuilt total %s does not match the committed total %s: the "+
			"denominator every payout divides by is not the one the chain will use", total, want)
	}
	if int64(len(shares)) != root.Accounts {
		return nil, fmt.Errorf("rebuilt book holds %d accounts, the commitment declares %d",
			len(shares), root.Accounts)
	}

	return &Book{
		Channel: root.Channel, Epoch: root.Epoch, Root: root.Root,
		Total: total, shares: shares, tree: tree, Skipped: skipped,
	}, nil
}

// Proof returns an account's share and the path proving it under the committed
// root, plus the payload it should hand to `claim`.
func (b *Book) Proof(account string) (share string, path []string, err error) {
	v, ok := b.shares[account]
	if !ok {
		return "", nil, fmt.Errorf("%s earned nothing in %s epoch %d", account, b.Channel, b.Epoch)
	}
	p, ok := b.tree.Proof(account)
	if !ok {
		return "", nil, fmt.Errorf("%s is in the book but has no path: the tree and the book "+
			"disagree, which is a bug in this service, not a claim you should retry", account)
	}
	return v.String(), p, nil
}

// Accounts lists the book's earners, sorted, for operators reconciling an epoch.
func (b *Book) Accounts() []string {
	out := make([]string, 0, len(b.shares))
	for a := range b.shares {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
