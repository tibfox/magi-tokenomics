// Package hivesrc reads the Hive layer-1 activity a reporter needs for one epoch
// and maps it onto sharecore's deterministic input types.
//
// Design note on VOTE WEIGHT — this is the key modelling decision:
//
//	WeightHiveRshares: use Hive's own `rshares` from get_active_votes. Hive has
//	  already applied the voter's stake and vote-mana, so we inherit a battle-tested
//	  integer weight and do NOT reimplement mana. Voting power therefore reflects
//	  HIVE stake.
//	WeightTokenStake: a real tribe usually wants voting power to come from ITS OWN
//	  token, not HIVE. In that mode a vote's weight is the voter's staked balance
//	  read from C1 at the epoch snapshot height, scaled by the vote percent.
//
// Both produce integers, so determinism is preserved either way.
package hivesrc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"magi_token/reporter/sharecore"
)

// WeightMode selects where a vote's weight comes from.
type WeightMode string

const (
	WeightHiveRshares WeightMode = "hive_rshares"
	WeightTokenStake  WeightMode = "token_stake"
)

// Transport performs a Hive JSON-RPC call. Swappable so tests need no network.
type Transport interface {
	Call(method string, params any, out any) error
}

// HTTPTransport is the real JSON-RPC transport.
type HTTPTransport struct {
	Endpoint string
	Client   *http.Client
}

func NewHTTPTransport(endpoint string) *HTTPTransport {
	return &HTTPTransport{Endpoint: endpoint, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (h *HTTPTransport) Call(method string, params any, out any) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params, "id": 1,
	})
	if err != nil {
		return err
	}
	resp, err := h.Client.Post(h.Endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("hive rpc %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("hive rpc %s: bad envelope: %w", method, err)
	}
	if env.Error != nil {
		return fmt.Errorf("hive rpc %s: %s", method, env.Error.Message)
	}
	if out == nil {
		return nil
	}
	// UseNumber, not the default float64: rshares run to ~1e15 and float64 loses
	// integer precision above 2^53 (~9e15). A single rounded rshares value would
	// give two reporters different shares for the same input, which is exactly the
	// non-determinism sharecore's integer math exists to prevent.
	dec := json.NewDecoder(bytes.NewReader(env.Result))
	dec.UseNumber()
	return dec.Decode(out)
}

// StakeReader resolves a voter's staked balance (used only by WeightTokenStake).
type StakeReader interface {
	StakeAtHeight(account string, height uint64) (*big.Int, error)
}

// RawPost is the subset of a Hive post we use.
type RawPost struct {
	Author   string `json:"author"`
	Permlink string `json:"permlink"`
	Created  string `json:"created"`
	Category string `json:"category"`
	Depth    int    `json:"depth"`

	// PayoutAt is when voting closes (creation + 7 days on Hive). It is the
	// authoritative field for payout attribution — do not recompute it from Created.
	PayoutAt string `json:"payout_at"`

	// IsPaidout reports that voting has finished and the vote set is final.
	//
	// NOTE: paid-out posts keep their full vote detail indefinitely. A 580-day-old
	// post still returns all 576 votes with time/percent/rshares. There is no
	// pruning deadline on reporting.
	IsPaidout bool `json:"is_paidout"`

	// Stats.IsPinned breaks the feed's ordering — see FetchPosts.
	Stats *struct {
		IsPinned bool `json:"is_pinned"`
	} `json:"stats"`

	// MaxAcceptedPayout is how an author declines their payout: they set it to
	// "0.000 HBD". Hive has no boolean for this, so the amount IS the flag.
	MaxAcceptedPayout string `json:"max_accepted_payout"`

	// JSONMetadata carries the post's tags and the app that published it. It is a
	// STRING containing JSON, not nested JSON, and it is author-controlled — a
	// malformed blob is normal and must never fail an epoch.
	JSONMetadata string `json:"json_metadata"`
}

// pinned reports whether the community has pinned this post to the top of its feed.
func (p RawPost) pinned() bool { return p.Stats != nil && p.Stats.IsPinned }

// declined reports that the author refused their Hive payout by zeroing
// max_accepted_payout. Anything unparseable or absent counts as NOT declined: the
// field is often omitted by lighter API shapes, and defaulting to "declined" there
// would silently stop paying everyone.
func (p RawPost) declined() bool {
	s := strings.TrimSpace(p.MaxAcceptedPayout)
	if s == "" {
		return false
	}
	amount := s
	if i := strings.IndexByte(amount, ' '); i >= 0 {
		amount = amount[:i]
	}
	for _, r := range amount {
		if r >= '1' && r <= '9' {
			return false // any non-zero digit means a payout is still accepted
		}
	}
	return true
}

// meta is the parsed json_metadata. Author-controlled, so every field is optional
// and a parse failure yields the zero value rather than an error.
type meta struct {
	Tags []string `json:"tags"`
	App  string   `json:"app"`
}

// parseMeta decodes json_metadata defensively.
//
// `app` is usually a string ("peakd/2023.1") but some clients write an object or an
// array, and `tags` is occasionally a bare string rather than a list. Both are
// decoded leniently because this is untrusted author input on a path that must not
// fail an epoch.
func (p RawPost) parseMeta() meta {
	var m meta
	if p.JSONMetadata == "" {
		return m
	}
	var loose struct {
		Tags any `json:"tags"`
		App  any `json:"app"`
	}
	if err := json.Unmarshal([]byte(p.JSONMetadata), &loose); err != nil {
		return m
	}
	switch t := loose.Tags.(type) {
	case string:
		m.Tags = []string{t}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				m.Tags = append(m.Tags, s)
			}
		}
	}
	switch a := loose.App.(type) {
	case string:
		m.App = a
	case map[string]any:
		// {"name":"peakd","version":"..."} — seen in the wild
		if s, ok := a["name"].(string); ok {
			m.App = s
		}
	}
	return m
}

// tagSet is every tag this post can be matched on: its category (which carries the
// community for a community post) plus its declared tags, lowercased.
func (p RawPost) tagSet() map[string]bool {
	out := map[string]bool{}
	if c := strings.ToLower(strings.TrimSpace(p.Category)); c != "" {
		out[c] = true
	}
	for _, t := range p.parseMeta().Tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out[t] = true
		}
	}
	return out
}

// hasAnyTag reports whether the post carries any of the given tags.
func (p RawPost) hasAnyTag(tags []string) bool {
	if len(tags) == 0 {
		return false
	}
	set := p.tagSet()
	for _, t := range tags {
		if set[strings.ToLower(strings.TrimSpace(t))] {
			return true
		}
	}
	return false
}

// RawVote is the subset of get_active_votes we use.
type RawVote struct {
	Voter   string `json:"voter"`
	Rshares any    `json:"rshares"` // node returns string or number depending on version
	Percent int    `json:"percent"`
	Time    string `json:"time"`
}

// Options configures one epoch's collection.
type Options struct {
	// Tags are the tribe tags or communities to walk. Each is a separate feed
	// request; a post appearing under several is collected ONCE.
	Tags           []string
	Limit          int        // max posts to consider
	Mode           WeightMode // where vote weight comes from
	SnapshotHeight uint64     // for WeightTokenStake
	Stake          StakeReader
	// ExcludeAccounts are dropped entirely (SCOT muting / app filters).
	ExcludeAccounts []string
	// ExcludedTags drop a post that carries any of them, applied AFTER the Tags
	// walk. Matched against the post's category and its json_metadata tags.
	ExcludedTags []string
	// PayoutWindow is how long voting stays open before a post pays. Zero means
	// Hive's own DefaultPayoutPeriod.
	PayoutWindow time.Duration
	// IgnoreDeclinedPayout pays authors who declined their Hive payout. Default
	// false honours the decline.
	IgnoreDeclinedPayout bool
	// DisableDownvotes drops negative votes instead of letting them net off.
	DisableDownvotes bool
	// Mana is the token_stake vote budget. Nil disables it, which is correct for
	// hive_rshares, where the rshares already carry Hive's own mana.
	Mana *ManaPolicy
	// AppTax, when non-nil, marks posts published outside the designated apps so
	// the share computation can skim them.
	AppTax *AppTaxPolicy
	// ManaScale is filled in by Collect after it has seen the whole epoch: mana is
	// spent across every post a voter touched, so it cannot be computed one post at
	// a time. Nil means full power for every vote.
	ManaScale map[string]int64

	// Since/Until bound the CREATION-time window used to walk the feed. The caller
	// shifts this back by the payout period, because the feed can only be paged by
	// creation time while membership is decided by payout time.
	//
	// Zero values disable the filter (useful for tests); the CLI always sets both.
	Since, Until time.Time

	// PayoutSince/PayoutUntil bound the epoch by PAYOUT time and are the actual
	// membership test.
	PayoutSince, PayoutUntil time.Time
}

// A POST IS SCORED IN THE EPOCH ITS PAYOUT FALLS IN — i.e. once Hive has closed
// voting, seven days after posting. This is what SCOT and Hive itself do, and it is
// the only mode offered, deliberately.
//
// Scoring a post in the epoch it was CREATED in would be prompter and is quietly
// unfair: voting stays open for seven days, so a post created in the last minute of
// an epoch would be scored with almost none of its votes while one created at the
// start gets a full epoch's worth, and every vote cast after the snapshot would be
// counted by nobody, ever. There is no configuration for that.
//
// The cost is that rewards lag one payout period behind posting, exactly as they do
// natively on Hive.

// DefaultPayoutPeriod is how long Hive itself keeps voting open on a post. A pool
// may shorten or lengthen its own window via Options.PayoutWindow; this is what it
// gets when it does not.
const DefaultPayoutPeriod = 7 * 24 * time.Hour

// payoutWindow resolves the configured window, falling back to Hive's.
func (o Options) payoutWindow() time.Duration {
	if o.PayoutWindow > 0 {
		return o.PayoutWindow
	}
	return DefaultPayoutPeriod
}

// ManaPolicy is the token_stake vote budget: a regenerating allowance that makes a
// vote cost something.
//
// Without it, weighing a vote by staked balance lets one account vote every post in
// the epoch at full weight, because nothing is consumed. SCOT's numbers are mirrored
// directly: Consumption is in HUNDREDTHS OF A PERCENT of full power, so 200 = 2% per
// full vote = 50 votes to empty, and RegenDays is how long 0 -> 100% takes.
//
// ★ SCOPE OF THE SIMULATION, which is a real limitation and not a rounding detail.
// SCOT keeps mana in its own persistent state, carried across epochs forever. The
// reporter holds no state between runs by design — chain state decides what still
// needs doing — so mana here is replayed WITHIN the epoch's own vote set, every
// voter starting at full power. An account that exhausted itself in the previous
// epoch therefore starts fresh in this one.
//
// That is deterministic, which is what Attest and the challenge window require, and
// it prices voting WITHIN an epoch correctly. It does not price it ACROSS epochs.
// Making it continuous would mean either reporter-side state (which two Attest
// machines could disagree about) or mana in contract state (a write per vote), and
// neither is worth it until an epoch is short enough for cross-epoch carryover to
// matter.
type ManaPolicy struct {
	RegenDays            int
	Consumption          int // hundredths of a percent per full vote
	DownvoteRegenDays    int
	DownvoteConsumption  int
}

// AppTaxPolicy skims from posts published outside a designated app.
//
// The `app` is self-declared in json_metadata, so this is an incentive aimed at
// ordinary clients, never an enforcement mechanism — anyone posting via the API can
// claim any app they like.
type AppTaxPolicy struct {
	Bps         int
	Apps        []string // matched on the part before "/", so "peakd/2023.1" matches "peakd"
	Beneficiary string
}

// designated reports whether a post's declared app is one of the blessed ones.
func (p *AppTaxPolicy) designated(app string) bool {
	name := app
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, a := range p.Apps {
		if strings.ToLower(strings.TrimSpace(a)) == name {
			return true
		}
	}
	return false
}

// Collect fetches the epoch's posts and votes and maps them to sharecore input.
// The returned slice is sorted so downstream determinism does not depend on the
// node's response ordering.
func Collect(tr Transport, opt Options) ([]sharecore.Post, error) {
	raw, err := FetchPosts(tr, opt)
	if err != nil {
		return nil, err
	}
	excl := map[string]bool{}
	for _, e := range opt.ExcludeAccounts {
		excl[e] = true
	}

	out := make([]sharecore.Post, 0, len(raw))
	kept := make([]postVotes, 0, len(raw))
	for _, p := range raw {
		if p.Author == "" || p.Permlink == "" || excl["hive:"+p.Author] {
			continue
		}
		// Exclusion beats inclusion: a post reached through an indexed tag is still
		// dropped if it carries an excluded one.
		if p.hasAnyTag(opt.ExcludedTags) {
			continue
		}
		// An author who zeroed max_accepted_payout declined their Hive payout, and by
		// default this pool honours that. Skipping the post entirely rather than
		// zeroing only the author's cut is deliberate: paying curators to farm a post
		// whose author wanted no payout is the loophole that makes the setting
		// pointless.
		if !opt.IgnoreDeclinedPayout && p.declined() {
			continue
		}
		// The whole point of payout attribution is that voting has CLOSED, so a post
		// that has not paid out yet means the reporter ran before the epoch's posts
		// finished. Refuse rather than freeze a partial vote set into a finalized
		// epoch; `run` is idempotent, so re-running shortly after fixes it.
		if !p.IsPaidout {
			return nil, fmt.Errorf(
				"@%s/%s has not paid out yet (payout_at %s) — voting is still open, so this "+
					"epoch cannot be scored yet. Re-run once its posts have paid out",
				p.Author, p.Permlink, p.PayoutAt)
		}
		var votes []RawVote
		if err := tr.Call("condenser_api.get_active_votes",
			[]any{p.Author, p.Permlink}, &votes); err != nil {
			return nil, fmt.Errorf("votes for @%s/%s: %w", p.Author, p.Permlink, err)
		}
		kept = append(kept, postVotes{p, votes})
	}

	// Mana is spent across EVERY post a voter touched this epoch, so it can only be
	// replayed once the whole epoch's votes are in hand. This is why collection is
	// two passes rather than mapping each post as it arrives.
	if opt.Mana != nil {
		scales, err := replayMana(kept, opt, excl)
		if err != nil {
			return nil, err
		}
		opt.ManaScale = scales
	}

	for _, pv := range kept {
		post, err := MapPost(pv.post, pv.votes, opt, excl)
		if err != nil {
			return nil, err
		}
		// A post with no positive votes can still be a rewardable item once
		// downvotes exist — but with nothing to divide it earns nothing either way,
		// so it is dropped here rather than carried as an empty entry.
		if len(post.Votes) > 0 {
			out = append(out, post)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Author != out[j].Author {
			return out[i].Author < out[j].Author
		}
		return out[i].Permlink < out[j].Permlink
	})
	return out, nil
}

// FetchPosts pages through the tag feed and returns the posts created inside the
// epoch window, oldest-first.
//
// Two node behaviours dictate this loop, both verified against api.hive.blog:
//
//  1. `limit` is capped at 20 — asking for more is a hard "Assert Exception:
//     limit = N outside valid range [1:20]", not a silent clamp. So a normal
//     epoch needs many calls.
//  2. The feed is NOT strictly newest-first. A community's PINNED posts are
//     hoisted to the top of page 1 regardless of age, so the very first entry can
//     be years old. Stopping at the first post older than Since — the obvious
//     implementation — therefore returns nothing at all for any community that
//     pins anything. Pinned posts are excluded from the ordering decision (they
//     are still scored if they happen to fall inside the window).
// FetchPosts walks every configured tag and returns the union.
//
// One feed request per tag: bridge.get_ranked_posts takes a single tag, so five tags
// is five walks. A post carried by several tags is collected ONCE — `collected`
// spans the tags for exactly that reason, while each walk keeps its own `seen` for
// cursor-repeat protection so one tag's dedupe cannot end another tag's walk early.
func FetchPosts(tr Transport, opt Options) ([]RawPost, error) {
	limit := opt.Limit
	if limit <= 0 {
		limit = 1000
	}
	collected := map[string]bool{}
	var out []RawPost
	for _, tag := range opt.Tags {
		got, err := fetchTag(tr, opt, tag, limit, collected)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	// Canonical order BEFORE any truncation: with several tags merged, ordering by
	// arrival would make the result depend on tag order in the config, and two
	// Attest machines with the same tags listed differently would disagree.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		if out[i].Author != out[j].Author {
			return out[i].Author < out[j].Author
		}
		return out[i].Permlink < out[j].Permlink
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func fetchTag(tr Transport, opt Options, tag string, limit int, collected map[string]bool) ([]RawPost, error) {
	const pageMax = 20 // hard node limit; verified against api.hive.blog

	var (
		out                        []RawPost
		startAuthor, startPermlink string
		seen                       = map[string]bool{}
	)
	for len(out) < limit {
		req := map[string]any{"sort": "created", "tag": tag, "limit": pageMax}
		if startAuthor != "" {
			req["start_author"] = startAuthor
			req["start_permlink"] = startPermlink
		}
		var page []RawPost
		if err := tr.Call("bridge.get_ranked_posts", req, &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}

		// Did this page contain any ORGANIC (non-pinned) post at or after Since?
		// Once a whole page is older than the window we are past it and can stop.
		sawOrganic, sawInRange := false, false
		for _, p := range page {
			key := p.Author + "/" + p.Permlink
			// paging repeats the cursor post; also guards against a node looping
			if seen[key] {
				continue
			}
			seen[key] = true

			if p.Depth != 0 {
				continue // comments are not posts; SCOT scores them separately
			}
			created, err := ParseHiveTime(p.Created)
			if err != nil {
				// A post we cannot place in time cannot be attributed to an epoch
				// deterministically, so fail loudly rather than silently drop it.
				return nil, fmt.Errorf("post @%s/%s: %w", p.Author, p.Permlink, err)
			}

			if !p.pinned() {
				sawOrganic = true
				if opt.Since.IsZero() || !created.Before(opt.Since) {
					sawInRange = true
				}
			}

			if !opt.Until.IsZero() && created.After(opt.Until) {
				continue // newer than this epoch — will be picked up next run
			}
			if !opt.Since.IsZero() && created.Before(opt.Since) {
				continue // older than this epoch; may still be a pinned outlier
			}
			// The creation window above only bounds the
			// WALK. Membership is decided by payout time, which is authoritative
			// and need not be exactly created+7d.
			if !opt.PayoutSince.IsZero() {
				paid, perr := ParseHiveTime(p.PayoutAt)
				if perr != nil {
					return nil, fmt.Errorf("post @%s/%s payout_at: %w", p.Author, p.Permlink, perr)
				}
				if paid.Before(opt.PayoutSince) || paid.After(opt.PayoutUntil) {
					continue
				}
			}
			// already collected under an earlier tag — indexed once, paid once
			if collected[key] {
				continue
			}
			collected[key] = true
			out = append(out, p)
			if len(out) >= limit {
				break
			}
		}
		if len(out) >= limit {
			break
		}

		// Stop once we have walked entirely past the window. Requiring sawOrganic
		// means a page consisting only of pinned posts never ends the walk.
		if sawOrganic && !sawInRange {
			break
		}

		last := page[len(page)-1]
		if last.Author == startAuthor && last.Permlink == startPermlink {
			break // node is not advancing; do not spin
		}
		startAuthor, startPermlink = last.Author, last.Permlink
	}

	// oldest-first is the natural reading order for an epoch report
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out, nil
}

// VoteCutoff returns the instant after which a vote no longer counts for this post.
//
// This is what makes a report REPRODUCIBLE, and it is not optional. Hive keeps
// recording votes after a post's payout — a real post paid out on 2023-03-16 and
// still took a vote on 2023-03-20. Without a fixed cutoff, anyone recomputing the
// epoch later would see a larger vote set and get different numbers, which would
// silently void the challenge window (a verifier could never reproduce the report)
// and break Attest quorum (two machines running days apart would never agree).
//
// A zero cutoff means "no cutoff" and is only for tests.
func VoteCutoff(p RawPost, opt Options) (time.Time, error) {
	if p.PayoutAt == "" {
		return time.Time{}, fmt.Errorf(
			"@%s/%s has no payout_at, so its vote set cannot be frozen", p.Author, p.Permlink)
	}
	return ParseHiveTime(p.PayoutAt)
}

// MapPost converts one raw post + its votes into sharecore form. Exported so the
// mapping (the part with real logic) is unit-testable without any network.
func MapPost(p RawPost, votes []RawVote, opt Options, excl map[string]bool) (sharecore.Post, error) {
	cutoff, err := VoteCutoff(p, opt)
	if err != nil {
		return sharecore.Post{}, err
	}

	// order votes by time, then voter, so vote Order is reproducible
	ordered := make([]RawVote, 0, len(votes))
	for _, v := range votes {
		if v.Voter == "" || excl["hive:"+v.Voter] {
			continue
		}
		if !cutoff.IsZero() {
			cast, terr := ParseHiveTime(v.Time)
			if terr != nil {
				return sharecore.Post{}, fmt.Errorf(
					"@%s/%s: vote by %s has an unusable time %q: %w",
					p.Author, p.Permlink, v.Voter, v.Time, terr)
			}
			if cast.After(cutoff) {
				continue // cast after voting closed; earns nothing, and counting it
			} //          would make the report depend on when it was generated
		}
		ordered = append(ordered, v)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Time != ordered[j].Time {
			return ordered[i].Time < ordered[j].Time
		}
		return ordered[i].Voter < ordered[j].Voter
	})

	post := sharecore.Post{
		Author:     "hive:" + p.Author,
		Permlink:   p.Permlink,
		Downweight: new(big.Int),
	}
	// A post published outside the designated apps is marked here; the skim itself
	// happens in sharecore, which owns the arithmetic.
	if opt.AppTax != nil && !opt.AppTax.designated(p.parseMeta().App) {
		post.TaxBps = opt.AppTax.Bps
	}

	for i, v := range ordered {
		w, err := voteWeight(v, opt)
		if err != nil {
			return post, err
		}
		if w == nil || w.Sign() == 0 {
			continue // zero-weight vote: no stake, or a 0% vote
		}
		w = applyMana(w, opt.manaScaleFor(p, v))

		if w.Sign() < 0 {
			// Downvote. It nets off the post's total but never joins Votes, so it
			// cannot draw curation rewards — see sharecore.Post.Downweight.
			if opt.DisableDownvotes {
				continue
			}
			post.Downweight.Add(post.Downweight, new(big.Int).Neg(w))
			continue
		}
		post.Votes = append(post.Votes, sharecore.Vote{
			Voter: "hive:" + v.Voter, Weight: w, Order: i,
		})
	}
	return post, nil
}

// manaScaleFor returns the voter's remaining power for this vote, or full power when
// no mana policy is in force.
func (o Options) manaScaleFor(p RawPost, v RawVote) int64 {
	if o.ManaScale == nil {
		return manaFull
	}
	if s, ok := o.ManaScale[manaKey(v.Voter, p.Author, p.Permlink)]; ok {
		return s
	}
	return manaFull
}

// manaKey identifies one vote. A voter votes a given post at most once, so the
// triple is unique within an epoch.
func manaKey(voter, author, permlink string) string {
	return voter + "|" + author + "|" + permlink
}

func voteWeight(v RawVote, opt Options) (*big.Int, error) {
	switch opt.Mode {
	case WeightTokenStake:
		if opt.Stake == nil {
			return nil, fmt.Errorf("token_stake mode requires a StakeReader")
		}
		st, err := opt.Stake.StakeAtHeight("hive:"+v.Voter, opt.SnapshotHeight)
		if err != nil {
			return nil, err
		}
		if st == nil || st.Sign() <= 0 || v.Percent == 0 {
			return new(big.Int), nil
		}
		// stake * percent / 10000  (Hive vote percent is in hundredths of a %).
		// A negative percent is a downvote and keeps its sign: MapPost decides
		// whether that nets off the post or is discarded.
		w := new(big.Int).Mul(st, big.NewInt(int64(v.Percent)))
		return w.Quo(w, big.NewInt(10000)), nil
	default: // WeightHiveRshares
		return parseRshares(v.Rshares), nil
	}
}

// parseRshares accepts the string or numeric form nodes may return.
//
// The sign is PRESERVED. Downvote policy is decided in MapPost — dropped when
// downvotes are disabled, netted against the post's total when they are not — and a
// clamp here would take that choice away by making every downvote invisible. The
// post's final weight still floors at zero: the contract cannot take shares away.
func parseRshares(v any) *big.Int {
	n := new(big.Int)
	switch t := v.(type) {
	case string:
		if _, ok := n.SetString(t, 10); !ok {
			return new(big.Int)
		}
	case float64:
		n.SetInt64(int64(t))
	case json.Number:
		if _, ok := n.SetString(t.String(), 10); !ok {
			return new(big.Int)
		}
	default:
		return new(big.Int)
	}
	return n
}

// postVotes pairs a fetched post with its votes between Collect's two passes.
type postVotes struct {
	post  RawPost
	votes []RawVote
}

// replayMana builds the per-vote power scale for the whole epoch.
//
// Only votes that would actually count are replayed: excluded accounts and votes
// cast after the cutoff spend nothing, because they earn nothing. Including them
// would let a muted account drain a voter's budget.
func replayMana(kept []postVotes, opt Options, excl map[string]bool) (map[string]int64, error) {
	var all []manaVote
	for _, pv := range kept {
		cutoff, err := VoteCutoff(pv.post, opt)
		if err != nil {
			return nil, err
		}
		for _, v := range pv.votes {
			if v.Voter == "" || excl["hive:"+v.Voter] || v.Percent == 0 {
				continue
			}
			down := v.Percent < 0
			if down && opt.DisableDownvotes {
				continue // a discarded downvote costs nothing
			}
			cast, terr := ParseHiveTime(v.Time)
			if terr != nil {
				return nil, fmt.Errorf("@%s/%s: vote by %s has an unusable time %q: %w",
					pv.post.Author, pv.post.Permlink, v.Voter, v.Time, terr)
			}
			if !cutoff.IsZero() && cast.After(cutoff) {
				continue
			}
			all = append(all, manaVote{
				voter:   v.Voter,
				when:    cast,
				percent: v.Percent,
				down:    down,
				key:     manaKey(v.Voter, pv.post.Author, pv.post.Permlink),
			})
		}
	}
	return manaScales(all, *opt.Mana), nil
}
