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

		// Downvotes net off the post's total BEFORE the curve, so they reduce what
		// the post earns without ever entering the curation split below — a
		// downvoter is not a curator. A post voted into the ground earns nothing
		// rather than something negative: the contract has no way to take shares
		// back, so the floor is zero.
		net := new(big.Int).Set(total)
		if p.Downweight != nil && p.Downweight.Sign() > 0 {
			net.Sub(net, p.Downweight)
			if net.Sign() <= 0 {
				continue
			}
		}

		postWeight := PowRational(net, cfg.AuthorCurveNum, cfg.AuthorCurveDen)
		if postWeight.Sign() == 0 {
			continue
		}

		// The app tax comes off the top, before author and curators divide what is
		// left, so both bear it in proportion rather than the author alone.
		if p.TaxBps > 0 && cfg.AppTaxBeneficiary != "" {
			tax := new(big.Int).Mul(postWeight, big.NewInt(int64(p.TaxBps)))
			tax.Div(tax, big.NewInt(10000))
			if tax.Sign() > 0 {
				add(cfg.AppTaxBeneficiary, tax)
				postWeight = new(big.Int).Sub(postWeight, tax)
				if postWeight.Sign() <= 0 {
					continue
				}
			}
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

	// Apply the dust threshold LAST, against the finished total.
	//
	// It has to be last: an account's share is the sum of everything it earned across
	// the whole epoch, so judging any single post's contribution would drop someone
	// who earned a little from each of forty posts. That is the opposite of the
	// intent — the threshold is for accounts that earned almost nothing in total, not
	// for small individual votes.
	if cfg.MinShareBps > 0 && total.Sign() > 0 {
		floor := new(big.Int).Mul(total, big.NewInt(int64(cfg.MinShareBps)))
		floor.Div(floor, big.NewInt(10000))
		if floor.Sign() > 0 {
			kept := make(map[string]*big.Int, len(acc))
			newTotal := new(big.Int)
			for _, k := range keys {
				// The app-tax beneficiary is exempt. Its cut is a policy transfer
				// rather than something earned, and a tax that silently vanished
				// because it fell under a dust threshold would be a very confusing
				// way to discover this setting.
				if acc[k].Cmp(floor) < 0 && k != cfg.AppTaxBeneficiary {
					continue
				}
				kept[k] = acc[k]
				newTotal.Add(newTotal, acc[k])
			}
			// Nothing is stranded: payout is funded*share/totalShares, and the
			// dropped shares leave the denominator, so their value redistributes
			// pro-rata to everyone remaining.
			acc, total = kept, newTotal
		}
	}
	return Result{Shares: acc, Total: total}
}
