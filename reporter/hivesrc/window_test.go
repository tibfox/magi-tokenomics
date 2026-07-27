package hivesrc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeTransport serves canned JSON-RPC responses and records what was asked.
type fakeTransport struct {
	head   uint64
	blocks map[uint64]string    // height -> timestamp
	feeds  [][]RawPost          // successive get_ranked_posts pages
	votes  map[string][]RawVote // "author/permlink" -> votes
	calls  []string
	feedIx int
	reqs   []map[string]any
}

func (f *fakeTransport) Call(method string, params any, out any) error {
	f.calls = append(f.calls, method)
	switch method {
	case "condenser_api.get_dynamic_global_properties":
		return remarshal(map[string]any{"head_block_number": f.head}, out)
	case "block_api.get_block_header":
		m, _ := params.(map[string]any)
		h, _ := m["block_num"].(uint64)
		ts, ok := f.blocks[h]
		if !ok {
			return fmt.Errorf("no such block %d", h)
		}
		return remarshal(map[string]any{"header": map[string]any{"timestamp": ts}}, out)
	case "bridge.get_ranked_posts":
		m, ok := params.(map[string]any)
		if ok {
			f.reqs = append(f.reqs, m)
		}
		// Enforce the node's REAL constraint. api.hive.blog rejects limit>20 with
		// "Assert Exception:limit = N outside valid range [1:20]" — a live-only
		// failure once, so the fake now reproduces it.
		if lim, has := m["limit"].(int); has && (lim < 1 || lim > 20) {
			return fmt.Errorf("Assert Exception:limit = %d outside valid range [1:20]", lim)
		}
		var page []RawPost
		if f.feedIx < len(f.feeds) {
			page = f.feeds[f.feedIx]
			f.feedIx++
		}
		return remarshal(page, out)
	case "condenser_api.get_active_votes":
		p, _ := params.([]any)
		key := fmt.Sprintf("%v/%v", p[0], p[1])
		return remarshal(f.votes[key], out)
	}
	return fmt.Errorf("unexpected method %s", method)
}

func remarshal(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func TestLatestClosedEpoch(t *testing.T) {
	// genesis 1000, len 100 -> epoch 0 = [1000,1099], epoch 1 = [1100,1199]
	for _, tc := range []struct {
		head    uint64
		want    uint64
		wantErr bool
	}{
		{head: 1000, wantErr: true}, // epoch 0 still running
		{head: 1098, wantErr: true},
		{head: 1099, wantErr: true}, // closed but cur==0 -> nothing before it
		{head: 1100, want: 0},       // epoch 0 closed, epoch 1 started
		{head: 1199, want: 0},
		{head: 1200, want: 1},
		{head: 500, wantErr: true}, // before genesis
	} {
		got, err := LatestClosedEpoch(1000, 100, tc.head)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("head %d: expected an error, got epoch %d", tc.head, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("head %d: %v", tc.head, err)
		}
		if got != tc.want {
			t.Fatalf("head %d: got epoch %d want %d", tc.head, got, tc.want)
		}
	}
}

// The window must be the same CLOSED interval the contracts use: C7 computes
// hEnd as genesis+(ep+1)*epochLen-1, so an off-by-one here would score the wrong
// blocks.
func TestEpochWindow_MatchesContractInterval(t *testing.T) {
	tr := &fakeTransport{blocks: map[uint64]string{
		1100: "2026-01-02T00:00:00",
		1199: "2026-01-02T04:57:00",
	}}
	w, err := EpochWindow(tr, 1000, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if w.StartBlock != 1100 || w.EndBlock != 1199 {
		t.Fatalf("want blocks 1100..1199, got %d..%d", w.StartBlock, w.EndBlock)
	}
	if w.StartTime.Format(HiveTimeLayout) != "2026-01-02T00:00:00" {
		t.Fatalf("start time: %s", w.StartTime)
	}
	if w.EndTime.Format(HiveTimeLayout) != "2026-01-02T04:57:00" {
		t.Fatalf("end time: %s", w.EndTime)
	}
}

func TestEpochWindow_RejectsZeroLen(t *testing.T) {
	if _, err := EpochWindow(&fakeTransport{}, 0, 0, 0); err == nil {
		t.Fatal("epochLen 0 must error, not divide by zero")
	}
}

// Block times come from the chain, never from a 3s estimate: two reporters that
// estimated differently would compute different windows and, in Attest mode,
// never agree on a payload.
func TestBlockTime_AsksTheChain(t *testing.T) {
	tr := &fakeTransport{blocks: map[uint64]string{42: "2026-03-01T12:00:00"}}
	got, err := BlockTime(tr, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %s", got)
	}
	if len(tr.calls) != 1 || tr.calls[0] != "block_api.get_block_header" {
		t.Fatalf("expected exactly one header call, got %v", tr.calls)
	}
}

func TestBlockTime_UnproducedBlockIsAnError(t *testing.T) {
	tr := &fakeTransport{blocks: map[uint64]string{}}
	if _, err := BlockTime(tr, 999); err == nil {
		t.Fatal("a block with no header must error rather than yield the zero time")
	}
}

func TestParseHiveTime(t *testing.T) {
	if _, err := ParseHiveTime("2026-01-02T03:04:05"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseHiveTime("2026-01-02T03:04:05Z"); err != nil {
		t.Fatalf("RFC3339 form should be tolerated: %v", err)
	}
	if _, err := ParseHiveTime(""); err == nil {
		t.Fatal("empty timestamp must error")
	}
	if _, err := ParseHiveTime("yesterday"); err == nil {
		t.Fatal("garbage must error")
	}
}

// ---- FetchPosts ----------------------------------------------------------

func post(author, permlink, created string) RawPost {
	return RawPost{Author: author, Permlink: permlink, Created: created, Depth: 0}
}

func TestFetchPosts_FiltersToTheEpochWindow(t *testing.T) {
	tr := &fakeTransport{feeds: [][]RawPost{{
		post("a", "too-new", "2026-01-03T00:00:00"), // after Until
		post("b", "inside", "2026-01-02T12:00:00"),
		post("c", "also-inside", "2026-01-02T01:00:00"),
		post("d", "too-old", "2026-01-01T00:00:00"), // before Since -> stop
		post("e", "never-seen", "2025-12-31T00:00:00"),
	}}}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")

	got, err := FetchPosts(tr, Options{Tag: "x", Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 in-window posts, got %d: %+v", len(got), got)
	}
	// oldest-first
	if got[0].Permlink != "also-inside" || got[1].Permlink != "inside" {
		t.Fatalf("want oldest-first, got %s then %s", got[0].Permlink, got[1].Permlink)
	}
}

func TestFetchPosts_SkipsComments(t *testing.T) {
	c := post("a", "a-comment", "2026-01-02T12:00:00")
	c.Depth = 1
	tr := &fakeTransport{feeds: [][]RawPost{{c, post("b", "a-post", "2026-01-02T12:00:00")}}}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")
	got, err := FetchPosts(tr, Options{Tag: "x", Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Permlink != "a-post" {
		t.Fatalf("comments must not be scored as posts: %+v", got)
	}
}

// An epoch longer than one API page must page with start_author/start_permlink.
func TestFetchPosts_PagesThroughTheFeed(t *testing.T) {
	var pageA, pageB []RawPost
	for i := 0; i < 20; i++ {
		pageA = append(pageA, post("a", fmt.Sprintf("p%03d", 119-i), "2026-01-02T12:00:00"))
	}
	for i := 0; i < 15; i++ {
		pageB = append(pageB, post("a", fmt.Sprintf("q%03d", 99-i), "2026-01-02T11:00:00"))
	}
	tr := &fakeTransport{feeds: [][]RawPost{pageA, pageB, {}}}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")

	got, err := FetchPosts(tr, Options{Tag: "x", Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 35 {
		t.Fatalf("want 35 posts across pages, got %d", len(got))
	}
	if len(tr.reqs) < 2 {
		t.Fatalf("expected at least 2 feed requests, got %d", len(tr.reqs))
	}
	// the second request must carry the cursor, or it would refetch page 1 forever
	if tr.reqs[1]["start_author"] != "a" || tr.reqs[1]["start_permlink"] != "p100" {
		t.Fatalf("second page cursor wrong: %+v", tr.reqs[1])
	}
}

func TestFetchPosts_RespectsLimit(t *testing.T) {
	var page []RawPost
	for i := 0; i < 20; i++ {
		page = append(page, post("a", fmt.Sprintf("p%03d", i), "2026-01-02T12:00:00"))
	}
	tr := &fakeTransport{feeds: [][]RawPost{page, page, {}}}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")
	got, err := FetchPosts(tr, Options{Tag: "x", Limit: 7, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("limit not respected: got %d", len(got))
	}
}

// A node that keeps returning the same cursor post must not spin forever.
func TestFetchPosts_TerminatesOnStalledCursor(t *testing.T) {
	same := []RawPost{post("a", "only", "2026-01-02T12:00:00")}
	feeds := make([][]RawPost, 50)
	for i := range feeds {
		feeds[i] = same
	}
	tr := &fakeTransport{feeds: feeds}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")

	done := make(chan struct{})
	var got []RawPost
	var err error
	go func() { got, err = FetchPosts(tr, Options{Tag: "x", Since: since, Until: until}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchPosts did not terminate on a stalled cursor")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the single post once, got %d", len(got))
	}
}

func TestFetchPosts_UnparseableCreatedIsAnError(t *testing.T) {
	tr := &fakeTransport{feeds: [][]RawPost{{post("a", "p", "not-a-time")}}}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	_, err := FetchPosts(tr, Options{Tag: "x", Since: since})
	if err == nil || !strings.Contains(err.Error(), "@a/p") {
		t.Fatalf("a post that cannot be placed in time must fail loudly, got %v", err)
	}
}

// End-to-end through Collect with a fake transport: no network, no keys.
func TestCollect_EndToEndWithFakeTransport(t *testing.T) {
	tr := &fakeTransport{
		feeds: [][]RawPost{{post("bob", "hello", "2026-01-02T12:00:00")}, {}},
		votes: map[string][]RawVote{
			"bob/hello": {
				{Voter: "amy", Rshares: "100", Percent: 10000, Time: "2026-01-02T12:01:00"},
				{Voter: "zed", Rshares: "300", Percent: 10000, Time: "2026-01-02T12:02:00"},
			},
		},
	}
	since, _ := ParseHiveTime("2026-01-02T00:00:00")
	until, _ := ParseHiveTime("2026-01-02T23:59:59")
	posts, err := Collect(tr, Options{Tag: "x", Mode: WeightHiveRshares, Since: since, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("want 1 post, got %d", len(posts))
	}
	if posts[0].Author != "hive:bob" || len(posts[0].Votes) != 2 {
		t.Fatalf("unexpected mapping: %+v", posts[0])
	}
	if posts[0].Votes[0].Voter != "hive:amy" {
		t.Fatalf("votes should be time-ordered, got %s first", posts[0].Votes[0].Voter)
	}
}
