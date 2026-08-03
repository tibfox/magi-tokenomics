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
}

// pinned reports whether the community has pinned this post to the top of its feed.
func (p RawPost) pinned() bool { return p.Stats != nil && p.Stats.IsPinned }

// RawVote is the subset of get_active_votes we use.
type RawVote struct {
	Voter   string `json:"voter"`
	Rshares any    `json:"rshares"` // node returns string or number depending on version
	Percent int    `json:"percent"`
	Time    string `json:"time"`
}

// Options configures one epoch's collection.
type Options struct {
	Tag            string     // tribe tag, e.g. "mytribe"
	Limit          int        // max posts to consider
	Mode           WeightMode // where vote weight comes from
	SnapshotHeight uint64     // for WeightTokenStake
	Stake          StakeReader
	// ExcludeAccounts are dropped entirely (SCOT muting / app filters).
	ExcludeAccounts []string

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

// PayoutPeriod is how long Hive keeps voting open on a post.
const PayoutPeriod = 7 * 24 * time.Hour

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
	for _, p := range raw {
		if p.Author == "" || p.Permlink == "" || excl["hive:"+p.Author] {
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
		post, err := MapPost(p, votes, opt, excl)
		if err != nil {
			return nil, err
		}
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
func FetchPosts(tr Transport, opt Options) ([]RawPost, error) {
	const pageMax = 20 // hard node limit; verified against api.hive.blog
	limit := opt.Limit
	if limit <= 0 {
		limit = 1000
	}

	var (
		out                        []RawPost
		startAuthor, startPermlink string
		seen                       = map[string]bool{}
	)
	for len(out) < limit {
		req := map[string]any{"sort": "created", "tag": opt.Tag, "limit": pageMax}
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

	post := sharecore.Post{Author: "hive:" + p.Author, Permlink: p.Permlink}
	for i, v := range ordered {
		w, err := voteWeight(v, opt)
		if err != nil {
			return post, err
		}
		if w == nil || w.Sign() <= 0 {
			continue // downvotes and zero-weight votes contribute nothing
		}
		post.Votes = append(post.Votes, sharecore.Vote{
			Voter: "hive:" + v.Voter, Weight: w, Order: i,
		})
	}
	return post, nil
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
		if st == nil || st.Sign() <= 0 || v.Percent <= 0 {
			return new(big.Int), nil
		}
		// stake * percent / 10000  (Hive vote percent is in hundredths of a %)
		w := new(big.Int).Mul(st, big.NewInt(int64(v.Percent)))
		return w.Div(w, big.NewInt(10000)), nil
	default: // WeightHiveRshares
		return parseRshares(v.Rshares), nil
	}
}

// parseRshares accepts the string or numeric form nodes may return. Negative
// (downvote) rshares clamp to zero: the contract cannot take shares away, and
// downvote policy is applied by producing NET non-negative shares.
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
	if n.Sign() < 0 {
		return new(big.Int)
	}
	return n
}
