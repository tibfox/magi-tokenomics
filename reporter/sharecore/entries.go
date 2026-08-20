package sharecore

import (
	"math/big"
	"strings"
)

// This file is the ONE implementation of the distributor's share-book entry rules.
//
// The rules were previously written out three times — in the contract, in
// `reporter root`, and in the itest helper — and the copies had already drifted:
// the contract accepts a `system:` payout destination and `reporter root` did not,
// so an operator's system: entry would be counted on chain and silently missing
// from the root committed for it. Nothing would have reported that; the account
// would simply never be able to prove a claim.
//
// Every consumer outside the contract must call these. The contract cannot (it is
// tinygo wasm with no imports beyond the SDK), so a drift guard compares the two.

// ledgerDomains are the address prefixes the distributor accepts as payout
// destinations, in the order applyEntries checks them.
var ledgerDomains = []string{"hive:", "contract:", "did:", "system:"}

// IsLedgerAddr mirrors the contract's isLedgerAddr. A share entry whose account
// carries no ledger domain has no payout destination, so the contract skips it
// rather than counting a slice of the epoch that nobody could ever claim.
func IsLedgerAddr(a string) bool {
	for _, d := range ledgerDomains {
		if strings.HasPrefix(a, d) {
			return true
		}
	}
	return false
}

// AddrIsWellFormed mirrors the contract's validateAddr. Note the asymmetry, which
// is deliberate on the contract's side: a bad DOMAIN is skipped, but an address
// carrying a state-key separator or a quote ABORTS the whole page, because such an
// address could forge a state key. Off-chain we cannot abort, so callers get the
// distinction back as a reason string.
func AddrIsWellFormed(a string) bool {
	if len(a) == 0 || len(a) > 256 {
		return false
	}
	return !strings.ContainsAny(a, `|"\`)
}

// SkippedEntry is an entry the contract would drop, and why. These are worth
// surfacing rather than discarding: a skipped entry is a no-op inside a
// transaction that SUCCEEDS, so the page looks applied and the earner is simply
// never paid.
type SkippedEntry struct {
	Raw    string
	Reason string
}

// ParseEntries applies the contract's applyEntries rules to a submitted entries
// string and returns the share book it produces, plus everything dropped.
//
// Duplicate accounts are SUMMED, not overwritten. The contract accumulates
// pageTotal per entry, so two entries for one account contribute both amounts; a
// map assignment would keep only the last and commit a root the chain's own
// arithmetic disagrees with.
func ParseEntries(entries string) (map[string]*big.Int, []SkippedEntry) {
	shares := map[string]*big.Int{}
	skipped := []SkippedEntry{}
	for _, e := range strings.Split(entries, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue // splitComma drops empty fields too
		}
		cut := strings.LastIndex(e, ":")
		acct, amt := e, ""
		if cut >= 0 {
			acct, amt = e[:cut], e[cut+1:]
		}
		switch {
		case acct == "":
			skipped = append(skipped, SkippedEntry{e, "no account"})
		case !AddrIsWellFormed(acct):
			// the contract ABORTS the page here — report it as such
			skipped = append(skipped, SkippedEntry{e, "illegal address: the contract aborts the whole page on this"})
		case !IsLedgerAddr(acct):
			skipped = append(skipped, SkippedEntry{e, "not a ledger address"})
		default:
			v, ok := new(big.Int).SetString(amt, 10)
			if !ok || v.Sign() <= 0 {
				skipped = append(skipped, SkippedEntry{e, "shares not positive"})
				continue
			}
			if cur, dup := shares[acct]; dup {
				shares[acct] = new(big.Int).Add(cur, v)
			} else {
				shares[acct] = v
			}
		}
	}
	return shares, skipped
}

// TotalOf sums a share book. The denominator every payout divides by.
func TotalOf(shares map[string]*big.Int) *big.Int {
	t := new(big.Int)
	for _, v := range shares {
		t.Add(t, v)
	}
	return t
}
