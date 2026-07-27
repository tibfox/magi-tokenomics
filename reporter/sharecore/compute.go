package sharecore

import (
	"math/big"
	"sort"
)

// ComputeShares turns one epoch's observed activity into per-account share
// weights, SCOT-style:
//
//	postWeight   = rshares_total ^ authorCurve
//	author gets    postWeight * AuthorRewardBps / 10000
//	curators split the remainder in proportion to their marginal curation weight
//	               C(cum_after) - C(cum_before),  C(v) = v ^ curationCurve
//
// The marginal-curve form is what rewards voting EARLY: an equal-weight vote cast
// first captures a bigger slice of a convex curve than the same vote cast last.
//
// Pure and deterministic: integer math, sorted output, no I/O.
func ComputeShares(posts []Post, cfg Config) Result {
	muted := make(map[string]bool, len(cfg.Muted))
	for _, m := range cfg.Muted {
		muted[m] = true
	}

	acc := make(map[string]*big.Int)
	add := func(who string, amt *big.Int) {
		if amt == nil || amt.Sign() <= 0 || muted[who] || who == "" {
			return
		}
		if cur, ok := acc[who]; ok {
			cur.Add(cur, amt)
			return
		}
		acc[who] = new(big.Int).Set(amt)
	}

	// Process posts in a canonical order so any accumulated rounding is identical
	// across machines (author+permlink is unique).
	ordered := make([]Post, len(posts))
	copy(ordered, posts)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Author != ordered[j].Author {
			return ordered[i].Author < ordered[j].Author
		}
		return ordered[i].Permlink < ordered[j].Permlink
	})

	for _, p := range ordered {
		// votes in their observed sequence — curation order is economically meaningful
		votes := make([]Vote, len(p.Votes))
		copy(votes, p.Votes)
		sort.SliceStable(votes, func(i, j int) bool {
			if votes[i].Order != votes[j].Order {
				return votes[i].Order < votes[j].Order
			}
			return votes[i].Voter < votes[j].Voter
		})

		total := new(big.Int)
		for _, v := range votes {
			if v.Weight != nil && v.Weight.Sign() > 0 {
				total.Add(total, v.Weight)
			}
		}
		if total.Sign() == 0 {
			continue // unvoted post earns nothing
		}

		postWeight := PowRational(total, cfg.AuthorCurveNum, cfg.AuthorCurveDen)
		if postWeight.Sign() == 0 {
			continue
		}

		authorCut := new(big.Int).Mul(postWeight, big.NewInt(int64(cfg.AuthorRewardBps)))
		authorCut.Div(authorCut, big.NewInt(10000))
		curatorPool := new(big.Int).Sub(postWeight, authorCut)

		add(p.Author, authorCut)
		if curatorPool.Sign() <= 0 {
			continue
		}

		// marginal curation weights along the curve
		cum := new(big.Int)
		prevCurve := new(big.Int)
		type cw struct {
			voter string
			w     *big.Int
		}
		weights := make([]cw, 0, len(votes))
		curveTotal := new(big.Int)
		for _, v := range votes {
			if v.Weight == nil || v.Weight.Sign() <= 0 {
				continue
			}
			cum.Add(cum, v.Weight)
			cur := PowRational(cum, cfg.CurationCurveNum, cfg.CurationCurveDen)
			marg := new(big.Int).Sub(cur, prevCurve)
			prevCurve = cur
			if marg.Sign() <= 0 {
				continue
			}
			weights = append(weights, cw{v.Voter, marg})
			curveTotal.Add(curveTotal, marg)
		}
		if curveTotal.Sign() == 0 {
			// nothing measurable to split — give it to the author so it isn't lost
			add(p.Author, curatorPool)
			continue
		}
		for _, w := range weights {
			share := new(big.Int).Mul(curatorPool, w.w)
			share.Div(share, curveTotal) // floor: dust stays undistributed, never over-allocated
			add(w.voter, share)
		}
	}

	total := new(big.Int)
	keys := make([]string, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		total.Add(total, acc[k])
	}
	return Result{Shares: acc, Total: total}
}
