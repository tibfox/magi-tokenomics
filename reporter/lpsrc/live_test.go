package lpsrc

import (
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
