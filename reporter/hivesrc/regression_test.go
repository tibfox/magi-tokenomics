package hivesrc

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regressions for three bugs that only a live run against api.hive.blog exposed.
// Each is pinned offline here so it cannot come back.

// BUG 1 — the community feed is NOT monotonically newest-first: pinned posts are
// hoisted to the top of page 1 regardless of age. Stopping at the first post older
// than Since (the obvious implementation) returned ZERO posts for any community
// that pins anything, silently reporting an empty epoch.
//
// Fixture mirrors the real #hive-167922 page 1: four pinned posts from 2024/2025
// followed by the actual newest post.
func TestRegression_PinnedPostsMustNotTerminateTheWalk(t *testing.T) {
	pin := func(author, permlink, created string) RawPost {
		p := post(author, permlink, created)
		p.Stats = &struct {
			IsPinned bool `json:"is_pinned"`
		}{IsPinned: true}
		return p
	}
	page1 := []RawPost{
		pin("leofinance", "pinned-2025-03", "2025-03-18T15:07:24"),
		pin("leofinance", "pinned-2025-02", "2025-02-10T17:55:33"),
		pin("leofinance", "pinned-2024-12", "2024-12-23T17:13:12"),
		post("bomspring", "in-window-1", "2026-07-25T12:00:00"),
	}
	page2 := []RawPost{
		post("someone", "in-window-2", "2026-07-25T06:00:00"),
		post("older", "out-of-window", "2026-07-20T00:00:00"),
	}
	tr := &fakeTransport{feeds: [][]RawPost{page1, page2, {}}}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := FetchPosts(tr, Options{Tags: []string{"hive-167922"}, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pinned posts truncated the walk: want 2 in-window posts, got %d (%+v)",
			len(got), permlinks(got))
	}
	if got[0].Permlink != "in-window-2" || got[1].Permlink != "in-window-1" {
		t.Fatalf("wrong posts collected: %v", permlinks(got))
	}
	if len(tr.reqs) < 2 {
		t.Fatalf("walk stopped on page 1; expected it to page past the pinned block")
	}
}

// A page consisting ONLY of pinned old posts must not end the walk either.
func TestRegression_AllPinnedPageDoesNotEndTheWalk(t *testing.T) {
	pin := func(permlink, created string) RawPost {
		p := post("comm", permlink, created)
		p.Stats = &struct {
			IsPinned bool `json:"is_pinned"`
		}{IsPinned: true}
		return p
	}
	tr := &fakeTransport{feeds: [][]RawPost{
		{pin("old-a", "2024-01-01T00:00:00"), pin("old-b", "2024-01-02T00:00:00")},
		{post("real", "in-window", "2026-07-25T12:00:00")},
		{},
	}}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := FetchPosts(tr, Options{Tags: []string{"x"}, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Permlink != "in-window" {
		t.Fatalf("want the one organic in-window post, got %v", permlinks(got))
	}
}

// Once past the window, an organic older page DOES stop the walk — otherwise the
// reporter would page through the community's entire history every epoch.
func TestRegression_OrganicOlderPageStopsTheWalk(t *testing.T) {
	tr := &fakeTransport{feeds: [][]RawPost{
		{post("a", "in-window", "2026-07-25T12:00:00")},
		{post("b", "older-1", "2026-07-20T00:00:00"), post("c", "older-2", "2026-07-19T00:00:00")},
		{post("d", "much-older", "2026-01-01T00:00:00")},
		{},
	}}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := FetchPosts(tr, Options{Tags: []string{"x"}, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 post, got %v", permlinks(got))
	}
	// page 3 must never be requested
	if len(tr.reqs) > 2 {
		t.Fatalf("walk did not stop after the first fully-older page: %d requests", len(tr.reqs))
	}
}

// CORRECTION — there is NO pruning deadline. An earlier version of this package
// claimed Hive discarded vote detail after the 7-day payout and refused to score
// paid-out posts. That was wrong: the "Post ... does not exist" error came from a
// TRUNCATED permlink in a debug print, not from pruning. Verified against live
// mainnet: @tibfox/blrdbeha, created 2023-03-09 and paid out 2023-03-16, still
// returns all 287 votes with time/percent/rshares 1234 days later.
//
// So a paid-out post is the NORMAL input, not an error.
func TestRegression_PaidOutPostsAreScorable(t *testing.T) {
	p := post("tibfox", "blrdbeha", "2023-03-09T21:19:06")
	p.IsPaidout = true
	p.PayoutAt = "2023-03-16T21:19:06"
	tr := &fakeTransport{
		feeds: [][]RawPost{{p}, {}},
		votes: map[string][]RawVote{"tibfox/blrdbeha": {
			{Voter: "kevinwong", Rshares: "95029627410", Percent: 1200, Time: "2023-03-10T08:08:06"},
			{Voter: "roelandp", Rshares: "34898459754", Percent: 420, Time: "2023-03-10T08:07:33"},
		}},
	}
	since, _ := ParseHiveTime("2023-03-16T00:00:00")
	until, _ := ParseHiveTime("2023-03-16T23:59:59")

	got, err := Collect(tr, Options{
		Tags: []string{"x"}, Mode: WeightHiveRshares,
		Since: mustTime("2023-03-09T00:00:00"), Until: mustTime("2023-03-09T23:59:59"),
		PayoutSince: since, PayoutUntil: until,
	})
	if err != nil {
		t.Fatalf("a paid-out post must be scorable, not rejected: %v", err)
	}
	if len(got) != 1 || len(got[0].Votes) != 2 {
		t.Fatalf("expected the post with both votes, got %+v", got)
	}
}

// BUG 4 — votes keep arriving AFTER a post pays out. @tibfox/blrdbeha paid out on
// 2023-03-16 and still took a vote on 2023-03-20. Counting those would make the
// report depend on WHEN it was generated: a verifier recomputing during the
// challenge window, or a second Attest machine running a day later, would see a
// bigger vote set and produce different numbers. The cutoff makes the input
// immutable.
func TestRegression_VotesAfterPayoutAreExcluded(t *testing.T) {
	p := post("tibfox", "blrdbeha", "2023-03-09T21:19:06")
	p.IsPaidout = true
	p.PayoutAt = "2023-03-16T21:19:06"
	votes := []RawVote{
		{Voter: "intime", Rshares: "100", Percent: 10000, Time: "2023-03-10T08:08:06"},
		{Voter: "late", Rshares: "999999", Percent: 10000, Time: "2023-03-20T21:20:30"}, // after payout
	}
	opt := Options{Mode: WeightHiveRshares}

	got, err := MapPost(p, votes, opt, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Votes) != 1 || got.Votes[0].Voter != "hive:intime" {
		t.Fatalf("post-payout vote must not be counted: %+v", got.Votes)
	}
}

// A post made near the end of an epoch must NOT be penalised for it. Scoring on
// payout means the vote set is whatever existed when Hive closed voting, so votes
// arriving on later days still count — which is the whole reason a post is never
// scored before its voting period ends.
func TestRegression_LatePosterIsNotPenalised(t *testing.T) {
	// posted 10 minutes before the epoch closed; most votes land on later days
	p := post("latecomer", "post", "2026-01-02T23:50:00")
	p.IsPaidout = true
	p.PayoutAt = "2026-01-09T23:50:00"
	votes := []RawVote{
		{Voter: "v1", Rshares: "100", Percent: 10000, Time: "2026-01-02T23:55:00"},
		{Voter: "v2", Rshares: "900", Percent: 10000, Time: "2026-01-03T09:00:00"},
		{Voter: "v3", Rshares: "900", Percent: 10000, Time: "2026-01-04T09:00:00"},
	}
	got, err := MapPost(p, votes, Options{Mode: WeightHiveRshares}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Votes) != 3 {
		t.Fatalf("all three votes must count, got %d — scoring a post before its "+
			"voting period ends is what would drop the later two", len(got.Votes))
	}
	// ...and each is weighted, not merely counted: v2/v3 carry 9x v1's weight.
	total := new(big.Int)
	for _, v := range got.Votes {
		total.Add(total, v.Weight)
	}
	if total.String() != "1900" {
		t.Fatalf("votes must contribute their WEIGHT (100+900+900=1900), got %s", total)
	}
}

// Refusing to score an epoch whose posts are still taking votes is the whole
// safety property of cashout mode — freezing a partial vote set into a finalized
// epoch is unrecoverable.
func TestRegression_UnpaidPostUnderCashoutIsRefused(t *testing.T) {
	p := post("a", "still-open", "2026-07-25T12:00:00")
	p.IsPaidout = false
	p.PayoutAt = "2026-08-01T12:00:00"
	tr := &fakeTransport{feeds: [][]RawPost{{p}, {}}}

	_, err := Collect(tr, Options{
		Tags: []string{"x"}, Mode: WeightHiveRshares,
		Since: mustTime("2026-07-25T00:00:00"), Until: mustTime("2026-07-25T23:59:59"),
	})
	if err == nil {
		t.Fatal("an epoch with posts still taking votes must not be scored")
	}
	if !strings.Contains(err.Error(), "has not paid out yet") {
		t.Fatalf("error should say voting is still open, got: %v", err)
	}
}

func mustTime(s string) time.Time {
	t, err := ParseHiveTime(s)
	if err != nil {
		panic(err)
	}
	return t
}

// BUG 3 — rshares run to ~1e15 and the default JSON decoder turns numbers into
// float64, which loses integer precision above 2^53 (~9.007e15). One rounded
// value would give two reporters different shares for the same post, defeating the
// whole point of sharecore's integer math (and of Attest's byte-identical pages).
func TestRegression_LargeRsharesSurviveDecodingExactly(t *testing.T) {
	// 2^53 + 1 is the smallest integer float64 cannot represent
	const exact = "9007199254740993"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":[{"voter":"whale","rshares":%s,"percent":10000,"time":"2026-01-01T00:00:00"}]}`, exact)
	}))
	defer srv.Close()

	var votes []RawVote
	if err := NewHTTPTransport(srv.URL).Call("condenser_api.get_active_votes", []any{"a", "b"}, &votes); err != nil {
		t.Fatal(err)
	}
	if len(votes) != 1 {
		t.Fatalf("want 1 vote, got %d", len(votes))
	}
	if _, isNum := votes[0].Rshares.(json.Number); !isNum {
		t.Fatalf("numbers must decode as json.Number, not %T — float64 rounds", votes[0].Rshares)
	}
	got := parseRshares(votes[0].Rshares)
	want, _ := new(big.Int).SetString(exact, 10)
	if got.Cmp(want) != 0 {
		t.Fatalf("rshares lost precision: got %s want %s", got, want)
	}
}

// The embedded active_votes in get_ranked_posts carry rshares but NO time and NO
// percent, so they cannot drive the curation curve (which needs vote order) or
// token_stake weighting (which needs percent). Documented here so nobody
// "optimises" the per-post get_active_votes call away.
func TestRegression_EmbeddedActiveVotesLackTimeAndPercent(t *testing.T) {
	// shape as returned by bridge.get_ranked_posts
	const embedded = `[{"voter":"xeldal","rshares":10424165235820},{"voter":"adol","rshares":2503262023962}]`
	var votes []RawVote
	if err := json.Unmarshal([]byte(embedded), &votes); err != nil {
		t.Fatal(err)
	}
	for _, v := range votes {
		if v.Time != "" {
			t.Fatal("fixture is stale: embedded votes now carry a time — the extra " +
				"get_active_votes round trip could be dropped")
		}
		if v.Percent != 0 {
			t.Fatal("fixture is stale: embedded votes now carry a percent")
		}
	}
}

func permlinks(ps []RawPost) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Permlink)
	}
	return out
}
