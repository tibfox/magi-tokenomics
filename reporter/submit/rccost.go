package submit

// The measured RC cost of `submitShares`, and the only place it is written down in
// code. `reporter init-config` sizes rc_limit from it and config validation refuses a
// config where a FULL page would not fit.
//
// WHY IT LIVES HERE rather than next to the check that uses it. These numbers describe
// the CONTRACT, not the reporter, and they drift every time the contract's writes or
// logs change — which has now happened twice: channel-scoping added a component to
// every state key, and event emission added a log per page. Both times the constants
// were left behind, and a stale one is not a cosmetic problem: it makes the coherence
// check UNDER-estimate a page, so a config loads cleanly and then reverts every full
// page it sends while the cheap calls (poke, pull, finalize) all succeed.
//
// Exported so `itest` can bind them to a REAL measurement — see
// TestRC_ReporterRcLimitFormulaCoversAFullPage, which fails if the metered cost of a
// page outgrows what these predict. That test is the reason this file can be trusted;
// without it these are two numbers in a comment that nobody re-derives.
//
// PER-ENTRY COST SCALES WITH ENTRY BYTES, not just entry count. The call writes a
// state key per account and now logs the submitted entry list verbatim, so a page of
// long account names costs materially more than the same count of short ones — the
// first draft of these constants was fitted against a fixture using 15-character ids
// and under-covered real ones by 5% at 30 entries.
//
// They are therefore sized against the WORST CASE for Hive accounts: `hive:` plus the
// 16-character maximum, which is what the guard test submits. A page of `did:`
// recipients has longer ids still and will cost more per entry; the 4096-byte payload
// cap binds first there, but an operator paginating did: recipients should measure
// rather than trust this.
//
// Current source: docs/rc-costs.md, ~144 RC/entry over a ~455 base, both rounded up.
const (
	RCPerEntry = 150
	RCBase     = 500
)

// RCHeadroomNum / RCHeadroomDen scale the estimate to absorb metering variance
// between pages of different byte lengths.
//
// The margin is REAL again. At 95/500 it had silently evaporated: 95 * 1.2 came to
// exactly the 114 RC/entry a page really costs after event emission, leaving a flat
// 174 RC of slack whatever the page size — 0.7% at 200 entries, where 20% was
// intended. The check still passed, so nothing failed; it had simply stopped being a
// margin. Sizing the constants to the measurement is what makes the multiplier mean
// what it says.
const (
	RCHeadroomNum = 12
	RCHeadroomDen = 10
)

// RCForPage is the rc_limit a page of n entries needs, headroom included.
func RCForPage(n int) int {
	return (RCBase + RCPerEntry*n) * RCHeadroomNum / RCHeadroomDen
}
