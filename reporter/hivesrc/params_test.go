package hivesrc

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// Coverage for the SCOT reward-pool parameters: multiple tags, excluded tags,
// declined payouts, the downvote toggle, the app tax and vote mana.
//
// Each test pins the BEHAVIOUR an operator configures, not the implementation —
// these settings are the whole interface a tribe has to its economics, and a silent
// change to any of them redistributes real money.

// tagTransport serves a different feed per tag, which the shared fakeTransport
// cannot do: it hands out pages in sequence regardless of what was asked for, so a
// multi-tag walk would consume another tag's page and the dedupe would look correct
// while testing nothing.
type tagTransport struct {
	byTag map[string][]RawPost
	votes map[string][]RawVote
	asked []string
}

func (f *tagTransport) Call(method string, params any, out any) error {
	switch method {
	case "bridge.get_ranked_posts":
		m, _ := params.(map[string]any)
		tag, _ := m["tag"].(string)
		// only the first page per tag; a second request for the same tag ends the walk
		if _, seen := m["start_author"]; seen {
			return remarshal([]RawPost{}, out)
		}
		f.asked = append(f.asked, tag)
		return remarshal(f.byTag[tag], out)
	case "condenser_api.get_active_votes":
		p, _ := params.([]any)
		key := fmt.Sprintf("%v/%v", p[0], p[1])
		return remarshal(f.votes[key], out)
	}
	return fmt.Errorf("unexpected method %s", method)
}

// meta2 builds json_metadata in its OBJECT form. Note it assigns straight onto the
// struct field, which is why these fixtures never caught the decode-level bug: they
// exercise parseMeta but skip json.Unmarshal of the post, and that is where an object
// used to be rejected outright. See json_metadata_object_test.go, which goes through
// the wire format instead.
func meta2(tags []string, app string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"tags": tags, "app": app})
	return json.RawMessage(b)
}

func upvote(voter string, rshares int64, when string) RawVote {
	return RawVote{Voter: voter, Rshares: fmt.Sprint(rshares), Percent: 10000, Time: when}
}

func downvote(voter string, rshares int64, when string) RawVote {
	return RawVote{Voter: voter, Rshares: fmt.Sprint(-rshares), Percent: -10000, Time: when}
}

// A post carried by several indexed tags is worth exactly one post. Paying it once
// per matching tag would multiply a tribe's payout by however many of its own tags
// an author happened to list.
func TestParams_MultiTagCollectsAPostOnce(t *testing.T) {
	shared := post("alice", "cross-posted", "2026-07-25T12:00:00")
	shared.JSONMetadata = meta2([]string{"bbh", "inleo"}, "peakd/1")
	only := post("bob", "one-tag", "2026-07-25T12:00:00")
	only.JSONMetadata = meta2([]string{"drip"}, "peakd/1")

	tr := &tagTransport{
		byTag: map[string][]RawPost{
			"bbh":   {shared},
			"inleo": {shared}, // the SAME post reachable under a second tag
			"drip":  {only},
		},
		votes: map[string][]RawVote{
			"alice/cross-posted": {upvote("v1", 100, "2026-07-25T12:30:00")},
			"bob/one-tag":        {upvote("v2", 100, "2026-07-25T12:30:00")},
		},
	}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := Collect(tr, Options{
		Tags: []string{"bbh", "inleo", "drip"}, Mode: WeightHiveRshares,
		Since: since, Until: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 posts (the cross-posted one counted once), got %d: %+v", len(got), got)
	}
	if len(tr.asked) != 3 {
		t.Fatalf("every tag must be walked, got %v", tr.asked)
	}
}

// Exclusion is applied AFTER inclusion, so a post reachable through an indexed tag
// is still dropped when it carries an excluded one. The reverse order would make
// the setting useless: everything the pool indexes arrives via an included tag.
func TestParams_ExcludedTagDropsAnOtherwiseIndexedPost(t *testing.T) {
	keep := post("alice", "wanted", "2026-07-25T12:00:00")
	keep.JSONMetadata = meta2([]string{"bbh"}, "peakd/1")
	drop := post("spammer", "unwanted", "2026-07-25T12:00:00")
	drop.JSONMetadata = meta2([]string{"bbh", "nsfw"}, "peakd/1")

	tr := &tagTransport{
		byTag: map[string][]RawPost{"bbh": {keep, drop}},
		votes: map[string][]RawVote{
			"alice/wanted":      {upvote("v1", 100, "2026-07-25T12:30:00")},
			"spammer/unwanted":  {upvote("v2", 100, "2026-07-25T12:30:00")},
		},
	}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := Collect(tr, Options{
		Tags: []string{"bbh"}, ExcludedTags: []string{"nsfw"},
		Mode: WeightHiveRshares, Since: since, Until: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Author != "hive:alice" {
		t.Fatalf("the excluded post must be dropped, got %+v", got)
	}
}

// An author who zeroed max_accepted_payout declined their payout on Hive, and the
// default honours that. The whole post is skipped, not just the author's cut —
// paying curators to farm a post whose author wanted nothing is the loophole that
// would make the setting pointless.
func TestParams_DeclinedPayoutIsHonouredByDefaultAndIgnorableOnRequest(t *testing.T) {
	declined := post("alice", "declined", "2026-07-25T12:00:00")
	declined.MaxAcceptedPayout = "0.000 HBD"
	normal := post("bob", "normal", "2026-07-25T12:00:00")
	normal.MaxAcceptedPayout = "1000000.000 HBD"

	newTr := func() *tagTransport {
		return &tagTransport{
			byTag: map[string][]RawPost{"bbh": {declined, normal}},
			votes: map[string][]RawVote{
				"alice/declined": {upvote("v1", 100, "2026-07-25T12:30:00")},
				"bob/normal":     {upvote("v2", 100, "2026-07-25T12:30:00")},
			},
		}
	}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")
	base := Options{Tags: []string{"bbh"}, Mode: WeightHiveRshares, Since: since, Until: until}

	got, err := Collect(newTr(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Author != "hive:bob" {
		t.Fatalf("a declined payout must be honoured by default, got %+v", got)
	}

	opt := base
	opt.IgnoreDeclinedPayout = true
	got, err = Collect(newTr(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ignore_declined_payout=true must pay the declining author too, got %d", len(got))
	}
}

// The downvote toggle, both ways. Disabled, a downvote is invisible; enabled, it
// nets off the post's total but its caster earns no curation — a downvoter is not
// a curator, and letting them draw from the curation pool would pay people to
// attack posts.
func TestParams_DownvoteToggleDecidesWhetherNegativeVotesCount(t *testing.T) {
	p := post("alice", "contested", "2026-07-25T12:00:00")
	tr := func() *tagTransport {
		return &tagTransport{
			byTag: map[string][]RawPost{"bbh": {p}},
			votes: map[string][]RawVote{"alice/contested": {
				upvote("fan", 1000, "2026-07-25T12:30:00"),
				downvote("hater", 400, "2026-07-25T12:40:00"),
			}},
		}
	}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")
	base := Options{Tags: []string{"bbh"}, Mode: WeightHiveRshares, Since: since, Until: until}

	off := base
	off.DisableDownvotes = true
	got, err := Collect(tr(), off)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Downweight.Sign() != 0 {
		t.Fatalf("disabled downvotes must leave no trace, got downweight %v", got[0].Downweight)
	}
	if n := len(got[0].Votes); n != 1 {
		t.Fatalf("the downvoter must never become a curator, got %d votes", n)
	}

	on, err := Collect(tr(), base)
	if err != nil {
		t.Fatal(err)
	}
	if on[0].Downweight.Cmp(big.NewInt(400)) != 0 {
		t.Fatalf("enabled downvotes must net off, got %v", on[0].Downweight)
	}
	if n := len(on[0].Votes); n != 1 {
		t.Fatalf("a downvoter still earns no curation, got %d curators", n)
	}
}

// The app tax marks posts published outside the designated apps. Matching is on the
// part before the slash, because clients append their version.
func TestParams_AppTaxMarksOnlyUndesignatedApps(t *testing.T) {
	blessed := post("alice", "on-app", "2026-07-25T12:00:00")
	blessed.JSONMetadata = meta2([]string{"bbh"}, "yourtribe/1.2.3")
	outside := post("bob", "off-app", "2026-07-25T12:00:00")
	outside.JSONMetadata = meta2([]string{"bbh"}, "peakd/2023.1")

	tr := &tagTransport{
		byTag: map[string][]RawPost{"bbh": {blessed, outside}},
		votes: map[string][]RawVote{
			"alice/on-app":  {upvote("v1", 100, "2026-07-25T12:30:00")},
			"bob/off-app":   {upvote("v2", 100, "2026-07-25T12:30:00")},
		},
	}
	since, _ := ParseHiveTime("2026-07-25T00:00:00")
	until, _ := ParseHiveTime("2026-07-25T23:59:59")

	got, err := Collect(tr, Options{
		Tags: []string{"bbh"}, Mode: WeightHiveRshares, Since: since, Until: until,
		AppTax: &AppTaxPolicy{Bps: 1000, Apps: []string{"yourtribe"}, Beneficiary: "hive:treasury"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		switch p.Author {
		case "hive:alice":
			if p.TaxBps != 0 {
				t.Fatalf("a post from the designated app must not be taxed, got %d bps", p.TaxBps)
			}
		case "hive:bob":
			if p.TaxBps != 1000 {
				t.Fatalf("a post from another app must carry the tax, got %d bps", p.TaxBps)
			}
		}
	}
}

// Mana is what makes a token_stake vote cost something. Without it a staked balance
// votes every post in the epoch at full weight, because a balance is not consumed by
// being used — the curation curve would then reward whoever automates hardest.
func TestParams_ManaThrottlesARepeatedVoter(t *testing.T) {
	at := func(s string) manaVote {
		when, err := ParseHiveTime(s)
		if err != nil {
			t.Fatal(err)
		}
		return manaVote{voter: "greedy", when: when, percent: 10000, key: s}
	}
	// five full votes, one minute apart: too little time to regenerate anything
	votes := []manaVote{
		at("2026-07-25T12:00:00"), at("2026-07-25T12:01:00"), at("2026-07-25T12:02:00"),
		at("2026-07-25T12:03:00"), at("2026-07-25T12:04:00"),
	}
	got := manaScales(votes, ManaPolicy{RegenDays: 5, Consumption: 2000})

	prev := int64(manaFull + 1)
	for _, v := range votes {
		s := got[v.key]
		if s >= prev {
			t.Fatalf("power must fall with every vote: %s got %d after %d", v.key, s, prev)
		}
		prev = s
	}
	if got[votes[0].key] != manaFull {
		t.Fatalf("the first vote is cast at full power, got %d", got[votes[0].key])
	}
	// 20% consumption compounding five times leaves roughly 41% (0.8^4)
	if last := got[votes[4].key]; last > 4500 || last < 3500 {
		t.Fatalf("five 20%% votes should leave ~41%% power, got %d", last)
	}
}

// Power regenerates with elapsed time, so the same votes spread out cost nothing in
// the long run. This is the half that makes the budget a RATE rather than a quota.
func TestParams_ManaRegeneratesOverTime(t *testing.T) {
	mk := func(day int) manaVote {
		when, _ := ParseHiveTime(fmt.Sprintf("2026-07-%02dT12:00:00", day))
		return manaVote{voter: "steady", when: when, percent: 10000, key: fmt.Sprint(day)}
	}
	votes := []manaVote{mk(1), mk(11), mk(21)}
	got := manaScales(votes, ManaPolicy{RegenDays: 5, Consumption: 2000})

	// ten days apart with a five-day full refill: every vote lands at full power
	for _, v := range votes {
		if got[v.key] != manaFull {
			t.Fatalf("vote %s should be at full power after a full refill, got %d", v.key, got[v.key])
		}
	}
}

// Upvotes and downvotes draw on SEPARATE pools — that is why there are four
// parameters rather than two. Exhausting one must not touch the other.
func TestParams_UpvoteAndDownvotePoolsAreIndependent(t *testing.T) {
	when, _ := ParseHiveTime("2026-07-25T12:00:00")
	votes := []manaVote{
		{voter: "a", when: when, percent: 10000, key: "up1"},
		{voter: "a", when: when.Add(60*time.Second), percent: 10000, key: "up2"},
		{voter: "a", when: when.Add(120*time.Second), percent: -10000, down: true, key: "dn1"},
	}
	got := manaScales(votes, ManaPolicy{
		RegenDays: 5, Consumption: 5000, DownvoteRegenDays: 30, DownvoteConsumption: 200,
	})
	if got["up2"] >= manaFull {
		t.Fatalf("the second upvote must be cheaper-powered, got %d", got["up2"])
	}
	if got["dn1"] != manaFull {
		t.Fatalf("spending upvote power must not touch the downvote pool, got %d", got["dn1"])
	}
}
