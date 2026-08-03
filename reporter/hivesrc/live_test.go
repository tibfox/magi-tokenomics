package hivesrc

import (
	"os"
	"testing"

	"magi_token/reporter/sharecore"
)

// Live smoke test against real Hive infrastructure. Skipped unless
// REPORTER_LIVE=1, so the normal suite stays offline and deterministic.
//
//	REPORTER_LIVE=1 go test ./reporter/hivesrc/ -run Live -v
//
// It exists because the offline tests use canned responses: they prove the
// mapping is correct but not that the RPC methods, parameter shapes and field
// names still match what public nodes actually serve.
func TestLive_FetchAndCompute(t *testing.T) {
	if os.Getenv("REPORTER_LIVE") != "1" {
		t.Skip("set REPORTER_LIVE=1 to run against real Hive nodes")
	}
	endpoint := os.Getenv("REPORTER_LIVE_API")
	if endpoint == "" {
		endpoint = "https://api.hive.blog"
	}
	tag := os.Getenv("REPORTER_LIVE_TAG")
	if tag == "" {
		tag = "hive-167922" // a busy community, so the window is never empty
	}
	tr := NewHTTPTransport(endpoint)

	head, err := HeadBlock(tr)
	if err != nil {
		t.Fatalf("HeadBlock: %v", err)
	}
	t.Logf("head block %d", head)

	// One day of blocks whose PAYOUT window has fully closed: posts created 8-9
	// days ago paid out 1-2 days ago, so cashout attribution has real input.
	const day = 28800
	genesis := head - 9*day
	win, err := EpochWindow(tr, genesis, day, 0)
	if err != nil {
		t.Fatalf("EpochWindow: %v", err)
	}
	t.Logf("epoch window blocks %d..%d  (%s .. %s)",
		win.StartBlock, win.EndBlock,
		win.StartTime.Format(HiveTimeLayout), win.EndTime.Format(HiveTimeLayout))
	if !win.EndTime.After(win.StartTime) {
		t.Fatalf("window end %s is not after start %s", win.EndTime, win.StartTime)
	}

	// cashout attribution: the epoch is the PAYOUT window; walk the creation
	// window one payout period earlier (exactly what the CLI does).
	posts, err := Collect(tr, Options{
		Tag: tag, Limit: 15, Mode: WeightHiveRshares,
		Since: win.StartTime, Until: win.EndTime,
		PayoutSince: win.StartTime.Add(PayoutPeriod),
		PayoutUntil: win.EndTime.Add(PayoutPeriod),
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	t.Logf("collected %d paid-out posts with votes for #%s", len(posts), tag)
	if len(posts) == 0 {
		t.Skipf("no posts with votes for #%s in that window — nothing to assert", tag)
	}

	// every mapped post must be usable by the deterministic core
	for _, p := range posts {
		if p.Author == "" || p.Permlink == "" {
			t.Fatalf("post mapped with an empty identity: %+v", p)
		}
		if len(p.Votes) == 0 {
			t.Fatalf("@%s/%s kept with no votes", p.Author, p.Permlink)
		}
		for _, v := range p.Votes {
			if v.Weight == nil || v.Weight.Sign() <= 0 {
				t.Fatalf("@%s/%s: vote by %s has non-positive weight %v",
					p.Author, p.Permlink, v.Voter, v.Weight)
			}
		}
	}

	cfg := sharecore.Config{AuthorRewardBps: 5000, AuthorCurveNum: 1, AuthorCurveDen: 1,
		CurationCurveNum: 1, CurationCurveDen: 2}
	res := sharecore.ComputeShares(posts, cfg)
	canon := sharecore.Canonicalize(res)
	if res.Total.Sign() <= 0 {
		t.Fatalf("real data produced zero total shares: %s", canon)
	}
	t.Logf("accounts %d  total shares %s", len(res.Shares), res.Total)

	pages := sharecore.Paginate(canon, 60, 3800)
	for _, pg := range pages {
		if len(pg.Entries) > 3800 {
			t.Fatalf("page %d is %d bytes, over the configured cap", pg.Index, len(pg.Entries))
		}
	}
	t.Logf("%d page(s); first: %.200s", len(pages), pages[0].Entries)

	// determinism on REAL data, not just synthetic fixtures
	res2 := sharecore.ComputeShares(posts, cfg)
	if sharecore.Canonicalize(res2) != canon {
		t.Fatal("recompute on identical real input produced a different canonical result")
	}
}
