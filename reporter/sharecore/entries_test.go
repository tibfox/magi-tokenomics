package sharecore

import (
	"math/big"
	"testing"
)

func bookOf(t *testing.T, entries string) (map[string]string, map[string]string) {
	t.Helper()
	shares, skipped := ParseEntries(entries)
	got := map[string]string{}
	for k, v := range shares {
		got[k] = v.String()
	}
	why := map[string]string{}
	for _, s := range skipped {
		why[s.Raw] = s.Reason
	}
	return got, why
}

// Every domain the contract's isLedgerAddr accepts must be counted here. The
// regression this pins is real: system: was accepted on chain and dropped by
// `reporter root`, which committed a root missing a leaf the chain had counted.
func TestParseEntriesCountsEveryLedgerDomain(t *testing.T) {
	got, why := bookOf(t, "hive:a:1,contract:vsc1b:2,did:key:z6Mk:3,system:treasury:4")
	for acct, want := range map[string]string{
		"hive:a": "1", "contract:vsc1b": "2", "did:key:z6Mk": "3", "system:treasury": "4",
	} {
		if got[acct] != want {
			t.Errorf("%s = %q, want %q — the contract counts this domain; a root that omits it "+
				"cannot prove a claim the chain considers funded", acct, got[acct], want)
		}
	}
	if len(why) != 0 {
		t.Errorf("nothing should have been skipped, got %v", why)
	}
}

// The contract accumulates pageTotal per ENTRY, so two entries for one account
// contribute both amounts. A map assignment keeps only the last, and the root
// would then disagree with the chain's own arithmetic.
func TestParseEntriesSumsDuplicateAccounts(t *testing.T) {
	got, _ := bookOf(t, "hive:carol:7,hive:bob:1,hive:carol:3")
	if got["hive:carol"] != "10" {
		t.Fatalf("hive:carol = %q, want 10 — duplicates must SUM, not overwrite", got["hive:carol"])
	}
	shares, _ := ParseEntries("hive:carol:7,hive:bob:1,hive:carol:3")
	if TotalOf(shares).Cmp(big.NewInt(11)) != 0 {
		t.Fatalf("total = %s, want 11", TotalOf(shares))
	}
}

func TestParseEntriesNamesWhatItDrops(t *testing.T) {
	for _, tc := range []struct{ entry, reason string }{
		{"alice:5", "not a ledger address"},
		{"hive:a:0", "shares not positive"},
		{"hive:a:-3", "shares not positive"},
		{"hive:a:x", "shares not positive"},
		// split2 cuts at the LAST colon, so "hive:a" parses as account "hive" with
		// amount "a" — the contract drops it for the domain, not the amount.
		{"hive:a", "not a ledger address"},
		{"hive:a:", "shares not positive"}, // trailing colon does leave an empty amount
		{":5", "no account"},
		{`hive:a|b:5`, "illegal address: the contract aborts the whole page on this"},
	} {
		got, why := bookOf(t, tc.entry)
		if len(got) != 0 {
			t.Errorf("%q was counted as %v, want dropped", tc.entry, got)
		}
		if why[tc.entry] != tc.reason {
			t.Errorf("%q dropped as %q, want %q", tc.entry, why[tc.entry], tc.reason)
		}
	}
}

// A separator or quote in an address could forge a state key, which is why the
// contract aborts rather than skips. Off-chain that distinction has to survive as
// a reason, or an operator reads "skipped" and assumes the page still applied.
func TestIllegalAddressIsReportedAsAborting(t *testing.T) {
	for _, bad := range []string{`hive:a|b:5`, `hive:a"b:5`, `hive:a\b:5`} {
		_, why := bookOf(t, bad)
		if why[bad] == "" || why[bad] == "not a ledger address" {
			t.Errorf("%q reported as %q — the contract ABORTS the page on this, and an "+
				"operator told it was merely skipped will believe the rest applied", bad, why[bad])
		}
	}
	long := "hive:" + string(make([]byte, 300)) + ":5"
	if AddrIsWellFormed(long[:len(long)-2]) {
		t.Error("a 300-byte address is well-formed here but bad address on chain")
	}
}
