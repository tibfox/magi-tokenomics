package submit

import "magi_token/reporter/sharecore"

// PlanOpts describes a complete epoch cycle, not just the share pages.
//
// A distributor epoch only pays out if FOUR things happen, in this order:
//
//  1. C2.distributeEpoch   — permissionless keeper poke; mints the epoch's
//     emission and records `owed` for each bucket. Nothing downstream exists
//     until this runs, and nobody else is obliged to run it, so the reporter
//     (which already runs on a schedule) is the natural keeper.
//  2. C3.pullFunding       — pulls this epoch's slice out of C2 into C3 and
//     records funded[epoch].
//  3. C3.submitShares × N  — the report itself.
//  4. C3.finalizeEpoch     — freezes it and opens the challenge window.
//
// The order is not cosmetic: finalizeEpoch aborts unless funded>0 AND
// totalShares>0, and submitShares aborts once the epoch is finalized. So
// funding must precede shares, and finalize must come dead last.
type PlanOpts struct {
	Epoch string

	// DistributorID is C3/C5 — the contract that takes shares and pays claims.
	DistributorID string
	// FunderID is C2. Leave empty to skip the keeper poke (e.g. when a separate
	// keeper process already pokes C2, which is the safer split for tenants who
	// run several distributors off one emission contract).
	FunderID string

	// PullFunding emits step 2. Skip it only if funding is already recorded.
	PullFunding bool
	// Finalize emits step 4. Skip it to submit shares across several runs and
	// finalize later.
	Finalize bool

	Pages   []sharecore.Page
	RcLimit int
}

// BuildFullPlan returns the ordered calls for one epoch cycle.
func BuildFullPlan(o PlanOpts) Plan {
	pl := Plan{Epoch: o.Epoch}

	if o.FunderID != "" {
		pl.Calls = append(pl.Calls, Call{
			ContractID: o.FunderID,
			Action:     "distributeEpoch",
			// no epoch argument: C2 catches up from its own height bookkeeping
			// and is idempotent, so a repeat poke is a no-op rather than a
			// double mint.
			Payload: `{}`,
			RcLimit: o.RcLimit,
			Note:    "keeper poke: mint emission + record bucket owed",
		})
	}
	if o.PullFunding {
		pl.Calls = append(pl.Calls, Call{
			ContractID: o.DistributorID,
			Action:     "pullFunding",
			Payload:    `{"epoch":"` + o.Epoch + `"}`,
			RcLimit:    o.RcLimit,
			Note:       "pull epoch slice from funder",
		})
	}

	// steps 3 and 4 are exactly the share plan
	inner := BuildPlan(o.DistributorID, o.Epoch, o.Pages, o.RcLimit, o.Finalize)
	pl.Calls = append(pl.Calls, inner.Calls...)
	return pl
}
