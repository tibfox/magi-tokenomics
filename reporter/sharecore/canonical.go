package sharecore

import (
	"sort"
	"strings"
)

// Page is one submitShares call's worth of entries.
type Page struct {
	Index   int    // page number; becomes the contract's `page` (must be canonical decimal)
	Entries string // "acct:shares,acct:shares" — exactly what goes on-chain
	Count   int    // number of entries in this page
}

// Canonicalize renders a Result as the contract's entry format, with a total
// ordering by account name. Two reporters with the same Result MUST produce the
// same bytes — that is the precondition for Attest mode (the contract compares
// pushed payloads byte-for-byte) and for a meaningful challenge window.
//
// Zero/negative shares are omitted: the contract skips them anyway, and including
// them would let two implementations differ harmlessly-but-fatally on bytes.
func Canonicalize(r Result) string {
	keys := make([]string, 0, len(r.Shares))
	for k, v := range r.Shares {
		if v != nil && v.Sign() > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(r.Shares[k].String())
	}
	return b.String()
}

// Paginate splits a canonical entry list into pages that respect BOTH limits the
// chain imposes:
//
//	maxEntries — RC budget. Measured cost is ~80-95 RC per entry over a ~200 fixed
//	             base (docs/rc-costs.md), so 60 entries is ~4,900 RC — inside the
//	             10,000 free tier, but only just.
//	maxBytes   — MAX_CONTRACT_PAYLOAD_SIZE is 8 KiB for the whole tx payload, so
//	             the entries string must stay comfortably under it.
//
// Splitting happens only at entry boundaries, so every page is independently valid
// and deterministic: the same Result and limits always yield the same pages.
func Paginate(canonical string, maxEntries, maxBytes int) []Page {
	if canonical == "" {
		return nil
	}
	if maxEntries < 1 {
		maxEntries = 1
	}
	if maxBytes < 16 {
		maxBytes = 16
	}
	items := strings.Split(canonical, ",")
	pages := []Page{}
	cur := make([]string, 0, maxEntries)
	curLen := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		pages = append(pages, Page{Index: len(pages), Entries: strings.Join(cur, ","), Count: len(cur)})
		cur = make([]string, 0, maxEntries)
		curLen = 0
	}
	for _, it := range items {
		addLen := len(it)
		if len(cur) > 0 {
			addLen++ // comma
		}
		if len(cur) >= maxEntries || (curLen+addLen) > maxBytes {
			flush()
			addLen = len(it)
		}
		cur = append(cur, it)
		curLen += addLen
	}
	flush()
	return pages
}
