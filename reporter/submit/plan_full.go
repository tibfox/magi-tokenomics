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
	// Channel is the distributor reward channel these shares belong to. One
	// distributor serves several, each with its own share book and reporter.
	Channel string
	Epoch   string

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

	// Root is the merkle commitment for this epoch's share book, and TotalShares the
	// denominator every claim divides by. Both are REQUIRED for a claimable epoch:
	// the pages above only publish the leaves for the indexer, and without a root no
	// claim can verify against anything.
	//
	// The plan puts submitRoot after the pages and before finalize. Order matters for
	// the same reason it always did — finalizeEpoch now refuses an epoch with no root,
	// because finalizing one would lock its funding away with nothing able to claim it.
	Root        string
	TotalShares string
	Accounts    int
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
	// SHARES BEFORE FUNDING — deliberately, and the order matters.
	//
	// pullFunding stamps `fundedAt|<ep>`, which anchors the guardian's stale-rescue
	// deadline. Pulling first starts that clock and then spends the whole window
	// paginating: at ~80-95 RC per entry against a 4096-byte page cap, a large report
	// is many transactions. It is worst for a BACKLOG epoch — one refill poke can
	// fund up to maxCatch epochs at once, so their deadlines all land in the same
	// window and a guardian (or an automated stale-cancel monitor) can cancel them
	// out from under a reporter that is submitting in good faith.
	//
	// submitShares does not require funding — it gates only on the epoch still being
	// open (c3-distributor/contract/main.go:126) — so the pages can go first and the
	// clock starts as late as possible. finalizeEpoch still requires funded>0, so
	// pullFunding simply has to precede it, which it does.
	inner := BuildPlan(o.DistributorID, o.Channel, o.Epoch, o.Pages, o.RcLimit, o.Finalize)
	pages, finalize := inner.Calls, []Call(nil)
	if n := len(pages); n > 0 && pages[n-1].Action == "finalizeEpoch" {
		pages, finalize = pages[:n-1], pages[n-1:]
	}
	pl.Calls = append(pl.Calls, pages...)
	if o.Root != "" {
		pl.Calls = append(pl.Calls, Call{
			ContractID: o.DistributorID,
			Action:     "submitRoot",
			Payload: `{"channel":"` + o.Channel + `","epoch":"` + o.Epoch +
				`","root":"` + o.Root + `","totalShares":"` + o.TotalShares +
				`","accounts":"` + itoaPlan(o.Accounts) + `"}`,
			RcLimit: o.RcLimit,
			Note:    "commit the share-book root — claims verify against this",
		})
	}

	if o.PullFunding {
		pl.Calls = append(pl.Calls, Call{
			ContractID: o.DistributorID,
			Action:     "pullFunding",
			Payload:    `{"channel":"` + o.Channel + `","epoch":"` + o.Epoch + `"}`,
			RcLimit:    o.RcLimit,
			Note:       "pull epoch slice from funder (after the pages, so the stale clock starts late)",
		})
	}
	pl.Calls = append(pl.Calls, finalize...)
	return pl
}

// itoaPlan avoids pulling strconv in for one conversion.
func itoaPlan(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
