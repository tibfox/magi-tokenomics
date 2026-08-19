package sharecore

import (
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

func bi(n int64) *big.Int { return big.NewInt(n) }

func linCfg() Config {
	return Config{AuthorRewardBps: 5000, AuthorCurveNum: 1, AuthorCurveDen: 1,
		CurationCurveNum: 1, CurationCurveDen: 1}
}

// ---- the property everything depends on: determinism -----------------------

// Same input in ANY map/slice order must yield byte-identical output. If this
// fails, Attest mode can never reach threshold and the challenge window is void.
func TestDeterminism_ShuffledInputSameBytes(t *testing.T) {
	mk := func() []Post {
		return []Post{
			{Author: "hive:bob", Permlink: "p1", Votes: []Vote{
				{Voter: "hive:v1", Weight: bi(500), Order: 0},
				{Voter: "hive:v2", Weight: bi(300), Order: 1},
				{Voter: "hive:v3", Weight: bi(200), Order: 2},
			}},
			{Author: "hive:carol", Permlink: "p2", Votes: []Vote{
				{Voter: "hive:v2", Weight: bi(700), Order: 0},
				{Voter: "hive:v4", Weight: bi(100), Order: 1},
			}},
			{Author: "hive:alice", Permlink: "p3", Votes: []Vote{
				{Voter: "hive:v1", Weight: bi(50), Order: 0},
			}},
		}
	}
	want := Canonicalize(ComputeShares(mk(), linCfg()))
	if want == "" {
		t.Fatal("expected non-empty canonical output")
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		posts := mk()
		rng.Shuffle(len(posts), func(a, b int) { posts[a], posts[b] = posts[b], posts[a] })
		for pi := range posts {
			v := posts[pi].Votes
			rng.Shuffle(len(v), func(a, b int) { v[a], v[b] = v[b], v[a] })
		}
		if got := Canonicalize(ComputeShares(posts, linCfg())); got != want {
			t.Fatalf("non-deterministic!\n iter %d\n want %s\n got  %s", i, want, got)
		}
	}
	t.Logf("canonical output stable across 40 shuffles: %s", want)
}

// Repeated runs of the identical input must be bit-identical (no map iteration leak).
func TestDeterminism_RepeatedRuns(t *testing.T) {
	posts := []Post{{Author: "hive:a", Permlink: "x", Votes: []Vote{
		{Voter: "hive:v1", Weight: bi(1), Order: 0}, {Voter: "hive:v2", Weight: bi(2), Order: 1}}}}
	first := Canonicalize(ComputeShares(posts, linCfg()))
	for i := 0; i < 200; i++ {
		if got := Canonicalize(ComputeShares(posts, linCfg())); got != first {
			t.Fatalf("run %d differs: %s vs %s", i, got, first)
		}
	}
}

// ---- curve math ------------------------------------------------------------

func TestPowRational_WholeAndFractional(t *testing.T) {
	if got := PowRational(bi(7), 1, 1); got.String() != "7" {
		t.Fatalf("linear: want 7 got %s", got)
	}
	if got := PowRational(bi(7), 2, 1); got.String() != "49" {
		t.Fatalf("square: want 49 got %s", got)
	}
	// r^(3/2) of 100 = 1000
	if got := PowRational(bi(100), 3, 2); got.String() != "1000" {
		t.Fatalf("r^1.5(100): want 1000 got %s", got)
	}
	// exact roots
	if got := IntNthRoot(bi(1000000), 3); got.String() != "100" {
		t.Fatalf("cbrt(1e6): want 100 got %s", got)
	}
	// floor behaviour
	if got := IntNthRoot(bi(26), 3); got.String() != "2" {
		t.Fatalf("cbrt(26): want floor 2 got %s", got)
	}
	if got := IntNthRoot(bi(27), 3); got.String() != "3" {
		t.Fatalf("cbrt(27): want 3 got %s", got)
	}
	if got := PowRational(bi(0), 3, 2); got.String() != "0" {
		t.Fatalf("zero input must give 0, got %s", got)
	}
}

// nth root must be exactly floor(x^(1/n)) for a spread of values — the property
// that makes two implementations agree.
func TestIntNthRoot_FloorProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 300; i++ {
		x := big.NewInt(rng.Int63n(1 << 40))
		for _, n := range []int{2, 3, 5} {
			r := IntNthRoot(x, n)
			lo := new(big.Int).Exp(r, big.NewInt(int64(n)), nil)
			hi := new(big.Int).Exp(new(big.Int).Add(r, bi(1)), big.NewInt(int64(n)), nil)
			if lo.Cmp(x) > 0 || hi.Cmp(x) <= 0 {
				t.Fatalf("not floor root: x=%s n=%d r=%s", x, n, r)
			}
		}
	}
}

// ---- reward split ----------------------------------------------------------

func TestAuthorCuratorSplit(t *testing.T) {
	// one post, total rshares 1000, linear curves, 50/50 split
	posts := []Post{{Author: "hive:auth", Permlink: "p", Votes: []Vote{
		{Voter: "hive:c1", Weight: bi(600), Order: 0},
		{Voter: "hive:c2", Weight: bi(400), Order: 1},
	}}}
	r := ComputeShares(posts, linCfg())
	// postWeight = 1000 ; author = 500 ; curators split 500 by 600:400
	if got := r.Shares["hive:auth"].String(); got != "500" {
		t.Fatalf("author: want 500 got %s", got)
	}
	if got := r.Shares["hive:c1"].String(); got != "300" {
		t.Fatalf("c1: want 300 got %s", got)
	}
	if got := r.Shares["hive:c2"].String(); got != "200" {
		t.Fatalf("c2: want 200 got %s", got)
	}
	// conservation: nothing invented
	if r.Total.Cmp(bi(1000)) > 0 {
		t.Fatalf("total %s exceeds post weight 1000", r.Total)
	}
}

// The curation curve's DIRECTION is an economic policy choice, and getting it
// backwards would silently invert incentives — so pin both directions.
//
// The mechanism is the marginal slice C(cum_after)-C(cum_before):
//
//	CONCAVE (num<den, e.g. 1/2 = sqrt) → early slices are the steep ones → EARLY
//	  voters earn more. This is the classic Steem/Hive curation incentive.
//	CONVEX  (num>den, e.g. 2/1)        → later slices are steeper → LATE voters
//	  earn more (a "pile-on" incentive).
func TestCurationCurve_ConcaveRewardsEarly_ConvexRewardsLate(t *testing.T) {
	mk := func() []Post {
		return []Post{{Author: "hive:a", Permlink: "p", Votes: []Vote{
			{Voter: "hive:early", Weight: bi(500), Order: 0},
			{Voter: "hive:late", Weight: bi(500), Order: 1},
		}}}
	}

	// concave sqrt curve → early voter must win
	cc := linCfg()
	cc.CurationCurveNum, cc.CurationCurveDen = 1, 2
	rc := ComputeShares(mk(), cc)
	early, late := rc.Shares["hive:early"], rc.Shares["hive:late"]
	if early == nil || late == nil {
		t.Fatal("both curators must earn something under a concave curve")
	}
	if early.Cmp(late) <= 0 {
		t.Fatalf("CONCAVE curve must reward the EARLY voter: early=%s late=%s", early, late)
	}
	t.Logf("concave (1/2): early=%s late=%s  <- classic early-voter advantage", early, late)

	// convex curve → late voter wins (documented, deliberate inverse)
	cv := linCfg()
	cv.CurationCurveNum, cv.CurationCurveDen = 2, 1
	rv := ComputeShares(mk(), cv)
	if rv.Shares["hive:late"].Cmp(rv.Shares["hive:early"]) <= 0 {
		t.Fatalf("CONVEX curve should reward the LATE voter: early=%s late=%s",
			rv.Shares["hive:early"], rv.Shares["hive:late"])
	}
	t.Logf("convex  (2/1): early=%s late=%s  <- pile-on incentive", rv.Shares["hive:early"], rv.Shares["hive:late"])

	// linear → equal weights earn equally
	rl := ComputeShares(mk(), linCfg())
	if rl.Shares["hive:early"].Cmp(rl.Shares["hive:late"]) != 0 {
		t.Fatalf("LINEAR curve must be order-neutral: early=%s late=%s",
			rl.Shares["hive:early"], rl.Shares["hive:late"])
	}
}

func TestMutedAccountsEarnNothing(t *testing.T) {
	cfg := linCfg()
	cfg.Muted = []string{"hive:spammer"}
	posts := []Post{{Author: "hive:spammer", Permlink: "p", Votes: []Vote{
		{Voter: "hive:good", Weight: bi(100), Order: 0}}}}
	r := ComputeShares(posts, cfg)
	if _, ok := r.Shares["hive:spammer"]; ok {
		t.Fatal("muted author must earn nothing")
	}
	if r.Shares["hive:good"] == nil {
		t.Fatal("curator should still earn")
	}
}

func TestUnvotedPostEarnsNothing(t *testing.T) {
	r := ComputeShares([]Post{{Author: "hive:a", Permlink: "p"}}, linCfg())
	if len(r.Shares) != 0 {
		t.Fatalf("unvoted post must produce no shares, got %v", r.Shares)
	}
	if Canonicalize(r) != "" {
		t.Fatal("canonical form of empty result must be empty")
	}
}

// ---- pagination ------------------------------------------------------------

func TestPaginate_RespectsBothLimits(t *testing.T) {
	r := Result{Shares: map[string]*big.Int{}}
	for i := 0; i < 47; i++ {
		r.Shares[string(rune('a'+i%26))+"cct"+string(rune('0'+i/26))+":x"] = bi(int64(i + 1))
	}
	canon := Canonicalize(r)
	pages := Paginate(canon, 12, 8000)
	if len(pages) != 4 { // 47 entries / 12 per page
		t.Fatalf("want 4 pages, got %d", len(pages))
	}
	total := 0
	for i, p := range pages {
		if p.Index != i {
			t.Fatalf("page index %d != %d", p.Index, i)
		}
		if p.Count > 12 {
			t.Fatalf("page %d has %d entries (>12)", i, p.Count)
		}
		if len(p.Entries) > 8000 {
			t.Fatalf("page %d is %d bytes (>8000)", i, len(p.Entries))
		}
		total += p.Count
	}
	if total != 47 {
		t.Fatalf("pages cover %d entries, want 47", total)
	}
	// byte limit must also bind
	tiny := Paginate(canon, 100, 40)
	for _, p := range tiny {
		if len(p.Entries) > 40 {
			t.Fatalf("byte-limited page too long: %d", len(p.Entries))
		}
	}
	// deterministic
	if a, b := Paginate(canon, 12, 8000), Paginate(canon, 12, 8000); len(a) != len(b) || a[0].Entries != b[0].Entries {
		t.Fatal("pagination must be deterministic")
	}
}

// Reassembling the pages must reproduce the canonical string exactly — the
// contract sums pages, so a lossy split would silently change totalShares.
func TestPaginate_LosslessRoundTrip(t *testing.T) {
	posts := []Post{}
	for i := 0; i < 30; i++ {
		posts = append(posts, Post{Author: "hive:a" + string(rune('a'+i)), Permlink: "p",
			Votes: []Vote{{Voter: "hive:v" + string(rune('a'+i)), Weight: bi(int64(100 + i)), Order: 0}}})
	}
	canon := Canonicalize(ComputeShares(posts, linCfg()))
	pages := Paginate(canon, 7, 8000)
	joined := ""
	for i, p := range pages {
		if i > 0 {
			joined += ","
		}
		joined += p.Entries
	}
	if joined != canon {
		t.Fatalf("pagination lost data:\n canon  %s\n joined %s", canon, joined)
	}
}

// Account names become an on-chain payload parsed by splitting on commas and then the
// last colon, so a name carrying either character corrupts it. lpsrc checks its own
// providers; this is the shared gate that also covers the Hive path, whose names come
// from whatever API endpoint the operator points at.
func TestValidateAccounts_RejectsNamesThatCorruptThePayload(t *testing.T) {
	bad := []struct{ name, want string }{
		{"hive:evil,hive:victim", "comma"},
		{"hive:a,hive:attacker:999999999,", "comma"},
		{"hive:alice:", "colon"},
		{"hive:al ice", "unsafe character"},
		{"hive:alice\n", "unsafe character"},
		{"hive:alice\"", "unsafe character"},
		{"hive:alicé", "unsafe character"},
		{"", "empty"},
	}
	for _, tc := range bad {
		r := Result{Shares: map[string]*big.Int{tc.name: big.NewInt(1)}, Total: big.NewInt(1)}
		err := ValidateAccounts(r)
		if err == nil {
			t.Fatalf("account %q must be refused", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("account %q: error should mention %q, got: %v", tc.name, tc.want, err)
		}
	}
}

// Ordinary Hive names and multi-colon DIDs must stay legal — last-colon splitting
// parses the latter correctly, so over-tightening would exclude real accounts.
func TestValidateAccounts_AcceptsRealAccounts(t *testing.T) {
	r := Result{Shares: map[string]*big.Int{
		"hive:alice":                         big.NewInt(1),
		"hive:bob.smith-1":                   big.NewInt(2),
		"did:key:z6MkpTHR8VNsBxYAAWHut2Gead": big.NewInt(3),
		"contract:vsc1BdxvBvpwKko8XfgN35iWw": big.NewInt(4),
	}, Total: big.NewInt(10)}
	if err := ValidateAccounts(r); err != nil {
		t.Fatalf("legitimate accounts must be accepted: %v", err)
	}
}

// Muting is EXACT-MATCH on the ledger-domain account, and this pins that.
//
// The reporter's config layer refuses a bare Hive name in `shares.muted` because a
// bare name matches nothing here — `muted[who]` is probed with who == "hive:"+author.
// If this rule were ever relaxed (a normalisation added here, say), that validator
// would go on rejecting configs for a reason that no longer existed, and nothing else
// in the tree would notice. So assert the rule itself, in both directions.
func TestMutedNameMustCarryTheLedgerDomain(t *testing.T) {
	posts := []Post{{Author: "hive:spammer", Permlink: "p", Votes: []Vote{
		{Voter: "hive:good", Weight: bi(100), Order: 0}}}}

	bare := linCfg()
	bare.Muted = []string{"spammer"}
	if _, ok := ComputeShares(posts, bare).Shares["hive:spammer"]; !ok {
		t.Fatal("a BARE muted name must not mute anyone — muting is exact-match on the " +
			"domain-prefixed account. If this now mutes, the config validator that " +
			"rejects bare names is enforcing a rule this package dropped")
	}

	pref := linCfg()
	pref.Muted = []string{"hive:spammer"}
	if _, ok := ComputeShares(posts, pref).Shares["hive:spammer"]; ok {
		t.Fatal("hive:spammer is the documented form and must mute")
	}
}
