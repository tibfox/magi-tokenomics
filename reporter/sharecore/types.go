// Package sharecore is the DETERMINISTIC core of the reporter service: it turns
// observed Hive activity for one epoch into per-account share weights that C3/C5
// consume via submitShares.
//
// Determinism is the whole point. The C3/C5 challenge window and the auth
// package's Attest mode (N machines must push BYTE-IDENTICAL pages) are only
// meaningful if any operator, on any machine, reproduces exactly the same output
// from the same input. Therefore this package:
//
//   - uses INTEGER math only (math/big) — never float64, whose rounding varies
//     with expression order and platform;
//   - sorts every map iteration before emitting;
//   - emits one canonical serialization.
//
// It performs NO I/O: no network, no clock, no randomness. Feed it a snapshot.
package sharecore

import "math/big"

// Vote is a single vote on a post, already resolved to an integer weight.
//
// Weight is the voter's effective stake-weight for this vote (rshares), computed
// upstream: stake × vote% × remaining vote-mana. Keeping it integer here means
// the mana/curve policy can evolve without endangering determinism.
type Vote struct {
	Voter  string   // "hive:alice"
	Weight *big.Int // rshares contributed (>=0)
	Order  int      // position in the post's vote sequence (0-based, ascending time)
}

// Post is one rewardable item (post or comment) inside the epoch.
type Post struct {
	Author   string // "hive:bob"
	Permlink string // stable id, used for deterministic tie-breaking
	Votes    []Vote

	// Downweight is the summed MAGNITUDE of the post's downvotes, already positive.
	// It is kept apart from Votes rather than folded in as negative weights because
	// the two are used differently: it nets off the post's total, deciding what the
	// post earns, but it never enters the curation curve — a downvoter is not a
	// curator and must not draw from the curation pool.
	//
	// Zero when downvotes are disabled, which is the collector's job to enforce.
	Downweight *big.Int

	// TaxBps skims this many basis points off the post's weight before the
	// author/curator split, paying Config.AppTaxBeneficiary. Set by the collector
	// for a post published outside the designated apps.
	TaxBps int
}

// Config mirrors the SCOT reward knobs the framework cares about. All values are
// integers so the computation stays reproducible.
type Config struct {
	// AuthorRewardBps is the author's cut of a post's weight, in basis points.
	// SCOT's author_reward_percentage. e.g. 5000 = 50% author / 50% curators.
	AuthorRewardBps int

	// AuthorCurveNum/Den express the author reward curve exponent as a rational,
	// e.g. 1/1 = linear, 2/1 = quadratic, 3/2 = r^1.5. SCOT's author_curve_exponent.
	AuthorCurveNum, AuthorCurveDen int

	// CurationCurveNum/Den likewise for the curation curve. The DIRECTION is a
	// policy choice with inverted incentives, so choose deliberately:
	//   CONCAVE (num<den, e.g. 1/2 = sqrt) → EARLY voters earn more (the classic
	//     Steem/Hive curation incentive — reward discovering good content first).
	//   LINEAR  (1/1)                      → order-neutral.
	//   CONVEX  (num>den, e.g. 2/1)        → LATE voters earn more (pile-on).
	CurationCurveNum, CurationCurveDen int

	// Muted accounts earn nothing (SCOT account muting). Sorted or not; matched exactly.
	Muted []string

	// AppTaxBeneficiary receives every post's Post.TaxBps skim. Empty means the skim
	// is not collected at all: a tax with nowhere to go would silently BURN the
	// slice, shrinking the epoch's total shares and quietly paying it to everyone
	// else, so the collector refuses that configuration upstream.
	AppTaxBeneficiary string

	// MinShareBps drops any account whose share is below this many basis points of
	// the epoch's total. 0 pays everyone.
	//
	// WHY IT EXISTS: cost. Every earner costs ~311 RC of state on chain whatever they
	// are owed, so a long tail of accounts earning a rounding error is the single
	// largest line in a reporter's bill — for 500 earners it IS the bill. Dropping
	// them is the one lever that scales it down.
	//
	// It is a POLICY choice, not a technicality: a dropped account receives nothing.
	// Expressed relative to the epoch's total rather than in tokens because the
	// reporter computes shares before it knows what the epoch was funded with — with
	// emission E, a threshold of B basis points is a floor of E*B/10000 tokens.
	//
	// Nothing is stranded. Payout is funded*share/totalShares and dropped shares
	// leave totalShares, so the amount simply redistributes pro-rata to everyone who
	// remains rather than sitting unclaimable.
	MinShareBps int
}

// Result is the deterministic output for one epoch.
type Result struct {
	// Shares maps account -> integer share weight. Never contains zero entries.
	Shares map[string]*big.Int
	// Total is the sum of all shares (informational; the CONTRACT recomputes it).
	Total *big.Int
}
