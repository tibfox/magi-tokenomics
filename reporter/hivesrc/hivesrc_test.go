package hivesrc

import (
	"math/big"
	"testing"

	"magi_token/reporter/sharecore"
)

type fakeStake struct{ m map[string]int64 }

func (f fakeStake) StakeAtHeight(acct string, h uint64) (*big.Int, error) {
	return big.NewInt(f.m[acct]), nil
}

// Vote ordering must come from vote TIME, not the node's response order, or two
// reporters querying different nodes would compute different curation slices.
func TestMapPost_OrdersVotesByTimeNotResponseOrder(t *testing.T) {
	p := RawPost{Author: "bob", Permlink: "hello", IsPaidout: true, PayoutAt: "2026-01-08T00:00:00"}
	votes := []RawVote{
		{Voter: "zed", Rshares: "300", Percent: 10000, Time: "2026-01-01T00:00:30"},
		{Voter: "amy", Rshares: "100", Percent: 10000, Time: "2026-01-01T00:00:10"},
		{Voter: "mia", Rshares: "200", Percent: 10000, Time: "2026-01-01T00:00:20"},
	}
	got, err := MapPost(p, votes, Options{Mode: WeightHiveRshares}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Author != "hive:bob" {
		t.Fatalf("author should be hive-prefixed, got %s", got.Author)
	}
	want := []string{"hive:amy", "hive:mia", "hive:zed"}
	if len(got.Votes) != 3 {
		t.Fatalf("want 3 votes, got %d", len(got.Votes))
	}
	for i, w := range want {
		if got.Votes[i].Voter != w || got.Votes[i].Order != i {
			t.Fatalf("vote %d: want %s order %d, got %s order %d", i, w, i, got.Votes[i].Voter, got.Votes[i].Order)
		}
	}
}

// Downvotes (negative rshares) must clamp to zero — the contract cannot subtract
// shares, so downvote policy has to arrive as NET non-negative weights.
func TestMapPost_DownvotesClampToZeroAndAreDropped(t *testing.T) {
	p := RawPost{Author: "bob", Permlink: "x", IsPaidout: true, PayoutAt: "2026-01-08T00:00:00"}
	votes := []RawVote{
		{Voter: "good", Rshares: "500", Percent: 10000, Time: "2026-01-01T00:00:01"},
		{Voter: "flag", Rshares: "-900", Percent: -10000, Time: "2026-01-01T00:00:02"},
	}
	got, _ := MapPost(p, votes, Options{Mode: WeightHiveRshares}, map[string]bool{})
	if len(got.Votes) != 1 || got.Votes[0].Voter != "hive:good" {
		t.Fatalf("downvote must not become a share-earning vote: %+v", got.Votes)
	}
}

func TestParseRshares_AcceptsStringAndNumber(t *testing.T) {
	if parseRshares("12345").String() != "12345" {
		t.Fatal("string form")
	}
	if parseRshares(float64(678)).String() != "678" {
		t.Fatal("number form")
	}
	if parseRshares("not-a-number").Sign() != 0 {
		t.Fatal("garbage must be 0")
	}
	if parseRshares(nil).Sign() != 0 {
		t.Fatal("nil must be 0")
	}
}

// token_stake mode: weight = staked balance * vote% / 10000, so a tribe's own
// token governs curation power rather than HIVE stake.
func TestMapPost_TokenStakeMode(t *testing.T) {
	st := fakeStake{m: map[string]int64{"hive:whale": 10000, "hive:minnow": 100}}
	p := RawPost{Author: "bob", Permlink: "x", IsPaidout: true, PayoutAt: "2026-01-08T00:00:00"}
	votes := []RawVote{
		{Voter: "whale", Rshares: "1", Percent: 10000, Time: "2026-01-01T00:00:01"},      // 100%
		{Voter: "minnow", Rshares: "999999", Percent: 5000, Time: "2026-01-01T00:00:02"}, // 50%
	}
	got, err := MapPost(p, votes, Options{Mode: WeightTokenStake, Stake: st, SnapshotHeight: 10}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Votes[0].Weight.String() != "10000" {
		t.Fatalf("whale weight: want 10000 got %s", got.Votes[0].Weight)
	}
	if got.Votes[1].Weight.String() != "50" { // 100 * 5000/10000
		t.Fatalf("minnow weight: want 50 got %s", got.Votes[1].Weight)
	}
	// crucially: the huge hive rshares did NOT leak into token-stake weighting
}

func TestMapPost_ExcludedAccountsDropped(t *testing.T) {
	excl := map[string]bool{"hive:muted": true}
	p := RawPost{Author: "bob", Permlink: "x", IsPaidout: true, PayoutAt: "2026-01-08T00:00:00"}
	votes := []RawVote{
		{Voter: "muted", Rshares: "500", Percent: 10000, Time: "2026-01-01T00:00:01"},
		{Voter: "ok", Rshares: "100", Percent: 10000, Time: "2026-01-01T00:00:02"},
	}
	got, _ := MapPost(p, votes, Options{Mode: WeightHiveRshares}, excl)
	if len(got.Votes) != 1 || got.Votes[0].Voter != "hive:ok" {
		t.Fatalf("excluded voter must be dropped: %+v", got.Votes)
	}
}

// End-to-end determinism through the mapping layer into sharecore.
func TestMapping_FeedsDeterministicCore(t *testing.T) {
	p := RawPost{Author: "bob", Permlink: "x", IsPaidout: true, PayoutAt: "2026-01-08T00:00:00"}
	votes := []RawVote{
		{Voter: "c", Rshares: "300", Percent: 10000, Time: "2026-01-01T00:00:03"},
		{Voter: "a", Rshares: "100", Percent: 10000, Time: "2026-01-01T00:00:01"},
		{Voter: "b", Rshares: "200", Percent: 10000, Time: "2026-01-01T00:00:02"},
	}
	cfg := sharecore.Config{AuthorRewardBps: 5000, AuthorCurveNum: 1, AuthorCurveDen: 1,
		CurationCurveNum: 1, CurationCurveDen: 2}
	first := ""
	for i := 0; i < 20; i++ {
		post, _ := MapPost(p, votes, Options{Mode: WeightHiveRshares}, map[string]bool{})
		out := sharecore.Canonicalize(sharecore.ComputeShares([]sharecore.Post{post}, cfg))
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("mapping+core not deterministic: %s vs %s", out, first)
		}
	}
	t.Logf("stable: %s", first)
}
