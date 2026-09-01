package lpsrc

import (
	"math/big"
	"os"
	"testing"
)

// TestLive_SchemaMatchesRealIndexer runs the REAL queries against a REAL Hasura.
//
// Every other test in this package uses a fake server — and a fake written
// alongside the queries will always agree with them. It cannot catch a renamed
// column, a different table prefix, or a type Hasura will not coerce. This closes
// that gap, and is the only test here that proves lpsrc talks to the actual indexer.
//
// Opt-in, because it needs network and a live endpoint:
//
//	LPSRC_LIVE_INDEXER=https://api-testnet.okinoko.io/hasura/v1/graphql \
//	LPSRC_LIVE_POOL=vsc1Brm1QpGF8WXvRCvwgbpB6fiHtTBJzyZUC9 \
//	go test ./reporter/lpsrc/ -run TestLive -v
//
// It is read-only: two GraphQL SELECTs, no chain writes.
func TestLive_SchemaMatchesRealIndexer(t *testing.T) {
	endpoint := os.Getenv("LPSRC_LIVE_INDEXER")
	pool := os.Getenv("LPSRC_LIVE_POOL")
	if endpoint == "" || pool == "" {
		t.Skip("set LPSRC_LIVE_INDEXER and LPSRC_LIVE_POOL to run the live schema check")
	}

	tr := &HTTPTransport{Endpoint: endpoint, Secret: os.Getenv("LPSRC_LIVE_SECRET")}

	// Both freshness signals are part of the schema contract now. Read them together
	// because their DIFFERENCE is the whole point: the global figure is not evidence
	// about any particular pool.
	global, err := IndexerHeight(tr)
	if err != nil {
		t.Fatalf("indexer_health is unreadable: %v", err)
	}
	h, any, err := PoolIndexedHeight(tr, pool)
	if err != nil {
		t.Fatalf("contract_logs is unreadable — the freshness gate depends on it: %v", err)
	}
	if !any {
		t.Fatalf("the indexer holds no logs for pool %s — wrong pool id?", pool)
	}
	t.Logf("indexer global height = %d; THIS POOL indexed to = %d (gap %d)", global, h, int64(global)-int64(h))
	if h > global {
		t.Fatalf("a pool cannot be indexed past the global max: pool %d > global %d", h, global)
	}

	// Score AT the indexer's own height, which does two things: the freshness gate
	// passes on proof rather than being bypassed, and Start == End asks for positions
	// at ONE instant. Spanning [0, h] would instead ask who held LP at height 0 AND
	// now, and min() correctly answers nobody — which reads as a schema failure but is
	// the anti-flash rule working. Epoch scoring wants a span; a schema check wants
	// the instant.
	res, err := LPShares(tr, Options{Pool: pool, Start: h, End: h})
	// A schema mismatch surfaces here: Hasura returns its complaint in the errors
	// array and fetchTable turns it into this failure rather than an empty result.
	if err != nil {
		t.Fatalf("live query failed — the schema lpsrc assumes does not match reality: %v", err)
	}
	if len(res.Shares) == 0 {
		t.Fatal("live indexer returned no LP positions at all; either the pool id is wrong " +
			"or the field names silently matched nothing")
	}
	for who, amt := range res.Shares {
		if !isLedgerAddr(who) {
			t.Fatalf("live provider %q is not a ledger address — the distributor would skip it", who)
		}
		if amt.Sign() <= 0 {
			t.Fatalf("live provider %q has non-positive share %s", who, amt)
		}
	}
	t.Logf("live schema OK: %d providers, total %s", len(res.Shares), res.Total)
	for who, amt := range res.Shares {
		t.Logf("  %s = %s", who, amt)
	}
}

// TestLive_ReconstructsRealPositions replays REAL liquidity events and checks the
// position arithmetic, not just the schema.
//
// The sibling test above proves the queries still match the indexer's columns. It
// does not prove the replay is right: add_liq and rem_liq have to be folded into a
// per-provider balance, and both epoch boundaries evaluated so a provider earns on
// min(LP(start), LP(end)) — the anti-flash-liquidity rule. A fake fixture cannot
// falsify that, because the fixture author decides both the events and the expected
// answer.
//
// The window is derived from the pool's own newest log rather than from a live epoch,
// because a testnet pool may have no recent activity at all: the freshness gate then
// refuses (correctly — it cannot tell "nothing happened" from "not indexed yet"), and
// refusing is the behaviour the OTHER test covers. Here we want the replay itself, so
// we score a window the indexer demonstrably has, with AllowStale set to say that is
// deliberate.
//
//	LPSRC_LIVE_INDEXER=... LPSRC_LIVE_POOL=... go test ./reporter/lpsrc/ -run TestLive -v
func TestLive_ReconstructsRealPositions(t *testing.T) {
	endpoint := os.Getenv("LPSRC_LIVE_INDEXER")
	pool := os.Getenv("LPSRC_LIVE_POOL")
	if endpoint == "" || pool == "" {
		t.Skip("set LPSRC_LIVE_INDEXER and LPSRC_LIVE_POOL to replay real positions")
	}
	tr := &HTTPTransport{Endpoint: endpoint, Secret: os.Getenv("LPSRC_LIVE_SECRET")}

	end, any, err := PoolIndexedHeight(tr, pool)
	if err != nil {
		t.Fatalf("PoolIndexedHeight: %v", err)
	}
	if !any {
		t.Skipf("pool %s has no indexed logs at all — nothing to replay", pool)
	}

	// Score a window whose START is already past the pool's activity, not from
	// genesis. Starting at 0 credits nobody by construction and says nothing about
	// the replay: the rule is min(LP(start), LP(end)), and at block 0 every position
	// is zero, so the minimum is zero however well the fold works. Asserting on that
	// would have looked like a broken replay for a pool that is in fact fully funded
	// — 430,239 LP minted and not one rem_liq.
	//
	// Both boundaries inside the settled range means both see the same positions, so
	// a zero here really does mean the events were not folded in.
	res, err := LPShares(tr, Options{Pool: pool, Start: end, End: end, AllowStale: true})
	if err != nil {
		t.Fatalf("LPShares over blocks 0..%d: %v", end, err)
	}
	t.Logf("replayed to block %d: %d providers, total %s", end, len(res.Shares), res.Total)
	if len(res.Shares) == 0 {
		t.Fatalf("pool %s has indexed logs up to %d but the replay produced no "+
			"providers — either every position was withdrawn, or add_liq is not "+
			"being folded in at all", pool, end)
	}

	// The total must be the sum of the parts. A mismatch means the aggregate and the
	// per-provider book disagree, and the contract divides by that total: every payout
	// would be scaled wrongly while each individual share still looked plausible.
	sum := new(big.Int)
	for who, v := range res.Shares {
		if v.Sign() < 0 {
			t.Fatalf("%s has a NEGATIVE position (%s) — a rem_liq was applied that no "+
				"add_liq paid for", who, v)
		}
		sum.Add(sum, v)
	}
	if sum.Cmp(res.Total) != 0 {
		t.Fatalf("total %s does not equal the sum of %d providers (%s) — the "+
			"denominator the contract divides by disagrees with the book",
			res.Total, len(res.Shares), sum)
	}

	for who, v := range res.Shares {
		t.Logf("  %s = %s", who, v)
	}

	// The anti-flash rule itself: a window that OPENS at genesis must credit nothing,
	// because min(LP(0), LP(end)) is zero for everyone no matter how much they hold
	// now. This is what stops liquidity added just before a snapshot from earning a
	// whole epoch, and it is the property a fixture is least able to falsify.
	joined, err := LPShares(tr, Options{Pool: pool, Start: 0, End: end, AllowStale: true})
	if err != nil {
		t.Fatalf("LPShares over 0..%d: %v", end, err)
	}
	if joined.Total.Sign() != 0 {
		t.Fatalf("a window opening at block 0 credited %s — min(LP(start), LP(end)) is "+
			"not being applied, so liquidity added mid-epoch would earn the whole epoch",
			joined.Total)
	}
}
