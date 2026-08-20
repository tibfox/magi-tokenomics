package hivesrc

import (
	"math/big"
	"sort"
	"time"
)

// Vote mana for token_stake weighting.
//
// WHY IT EXISTS. hive_rshares inherits Hive's own mana: the rshares figure a node
// reports has already been shrunk by how much the voter has been voting. token_stake
// has no such thing — it reads a staked balance, and a balance is not consumed by
// being used. Without a budget the same stake votes every post in an epoch at full
// weight, so curation rewards go to whoever automates voting hardest and the
// curation curve stops meaning anything.
//
// THE MODEL, mirroring SCOT's parameters exactly:
//
//	power        0..manaFull, in hundredths of a percent (10000 = 100%)
//	regenerates  manaFull over RegenDays, linearly with elapsed time
//	a vote costs power * Consumption/manaFull * percent/manaFull
//	a vote is worth its raw weight scaled by the power REMAINING when it was cast
//
// Cost scales with current power, as on Hive, so power approaches zero rather than
// hitting it — a heavy voter is throttled continuously rather than cut off.
//
// Upvotes and downvotes draw on SEPARATE pools, which is why there are four
// parameters. A tribe can make downvoting cheap and plentiful or slow and rationed
// without touching how often people can upvote.
//
// DETERMINISM. Replay is over the epoch's own vote set, in canonical (time, voter,
// post) order, every voter starting at full power. No clock is read and no state
// carries between runs, so any machine reproduces it byte-identically — which Attest
// and the challenge window both require. The cost is that the budget resets each
// epoch; see ManaPolicy's doc comment for why that trade was made.
const manaFull = 10000

// manaVote is one vote seen during the epoch, before mana is applied.
type manaVote struct {
	voter    string
	when     time.Time
	percent  int  // hundredths of a percent, negative for a downvote
	down     bool
	key      string // voter|author|permlink — unique, a voter votes a post once
}

// manaScales replays the epoch's votes and returns, per vote key, the voter's
// remaining power at the moment they cast it, in hundredths of a percent.
//
// Votes are grouped per voter and replayed in time order. A missing key means full
// power, so a caller that never ran this gets unscaled weights.
func manaScales(votes []manaVote, p ManaPolicy) map[string]int64 {
	byVoter := map[string][]manaVote{}
	for _, v := range votes {
		byVoter[v.voter] = append(byVoter[v.voter], v)
	}
	// voters in sorted order: the result is a map, but building it in a fixed order
	// keeps any future change to this function reproducible by construction.
	names := make([]string, 0, len(byVoter))
	for n := range byVoter {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make(map[string]int64, len(votes))
	for _, n := range names {
		vs := byVoter[n]
		sort.SliceStable(vs, func(i, j int) bool {
			if !vs[i].when.Equal(vs[j].when) {
				return vs[i].when.Before(vs[j].when)
			}
			return vs[i].key < vs[j].key
		})

		up, down := int64(manaFull), int64(manaFull)
		var lastUp, lastDown time.Time
		for _, v := range vs {
			pool, regenDays, cost := &up, p.RegenDays, p.Consumption
			last := &lastUp
			if v.down {
				pool, regenDays, cost = &down, p.DownvoteRegenDays, p.DownvoteConsumption
				last = &lastDown
			}
			if !last.IsZero() {
				*pool = regenerate(*pool, v.when.Sub(*last), regenDays)
			}
			*last = v.when

			out[v.key] = *pool

			pct := int64(v.percent)
			if pct < 0 {
				pct = -pct
			}
			if pct > manaFull {
				pct = manaFull
			}
			// spend = power * consumption/10000 * percent/10000, integer throughout
			spend := *pool * int64(cost) / manaFull
			spend = spend * pct / manaFull
			if spend > *pool {
				spend = *pool
			}
			*pool -= spend
		}
	}
	return out
}

// regenerate adds the power accrued over d, capped at full.
//
// Integer division truncates, so a voter is never credited more than they earned.
// A non-positive regenDays would divide by zero; it is rejected at config load, and
// treated as "no regeneration" here rather than panicking on a path that must never
// take down an epoch.
func regenerate(power int64, d time.Duration, regenDays int) int64 {
	if regenDays <= 0 || d <= 0 {
		return power
	}
	period := int64(regenDays) * 24 * 3600
	gained := int64(d.Seconds()) * manaFull / period
	power += gained
	if power > manaFull {
		power = manaFull
	}
	return power
}

// applyMana scales a raw vote weight by the power the voter had left.
func applyMana(w *big.Int, scale int64) *big.Int {
	if w == nil || scale >= manaFull {
		return w
	}
	if scale <= 0 {
		return new(big.Int)
	}
	out := new(big.Int).Mul(w, big.NewInt(scale))
	return out.Div(out, big.NewInt(manaFull))
}
