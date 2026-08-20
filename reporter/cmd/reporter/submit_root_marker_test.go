package main

import (
	"strconv"
	"testing"

	"magi_token/reporter/submit"
)

// submitRoot needs a progress marker, like every other call in the plan.
//
// chainApplied decides what a run still has to do, and it had cases for
// distributeEpoch/pullFunding, submitShares and finalizeEpoch — but none for
// submitRoot, and it never asked the chain for root|<ch>|<ep>. PageOf returns -1 for
// any non-submitShares call, so out["submitRoot/-1"] was simply never created, and a
// map lookup of a missing key is false. BuildFullPlan emits submitRoot for every epoch
// that has earners (PlanOpts.Validate refuses an empty root once there are pages), so
// the call sat in `remaining` permanently.
//
// What that cost on every re-run of an epoch whose root was already committed:
//
//   - one guaranteed-failing L1 custom_json and its RC — SubmitRoot aborts on
//     "root already submitted for this epoch", after the RC is spent, and the
//     broadcaster returns an L1 txid with no L2 receipt, so nothing noticed;
//   - a false "recorded locally but the chain does not show it applied — retrying"
//     line, because MarkDone stores submitRoot/-1 and the chain never agreed;
//   - the "epoch already fully submitted" exit was unreachable for any real epoch,
//     since one call always remained.
//
// The marker is root|<ch>|<ep>, which the contract writes with set(rk, root) in the
// same `if committed` block that writes ssdone| for a page — so in attest mode it
// appears exactly when the quorum converges, and this reads it the same way pages are
// read. A presence check is sufficient BECAUSE assertBookMatchesChain runs first on
// the same path (main.go:925, before chainApplied at :943) and already refuses a run
// whose plan root differs from the committed one. A non-empty root| here is therefore
// guaranteed to be OUR root, and this cannot mask a divergence.

func rootMarkerPlan() submit.Plan {
	pl := submit.Plan{Epoch: "7"}
	pl.Calls = append(pl.Calls, submit.Call{
		Action: "pullFunding", Payload: `{"channel":"content","epoch":"7"}`})
	pl.Calls = append(pl.Calls, submit.Call{
		Action:  "submitShares",
		Payload: `{"channel":"content","epoch":"7","page":"0","entries":"hive:a:1"}`})
	pl.Calls = append(pl.Calls, submit.Call{
		Action: "submitRoot",
		Payload: `{"channel":"content","epoch":"7","root":"` + rootA +
			`","totalShares":"100"}`})
	pl.Calls = append(pl.Calls, submit.Call{
		Action: "finalizeEpoch", Payload: `{"channel":"content","epoch":"7"}`})
	return pl
}

// THE FINDING: the root is on chain and the run still counts submitRoot as outstanding.
//
// This fails if EITHER half of the fix is missing — the fake only answers keys that
// were actually requested, so omitting root| from chainApplied's key list leaves the
// value empty and the call unapplied, exactly as omitting the switch case does.
func TestSubmitRootMarker_CommittedRootCountsAsApplied(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|7":   "50000",
		"ssdone|content|7|0": "1",
		"root|content|7":     rootA,
		"ch_rMode|content":   "0",
	}, nil)

	applied, _, err := a.chainApplied(rootMarkerPlan())
	if err != nil {
		t.Fatal(err)
	}
	if !applied["submitRoot/-1"] {
		t.Fatal("root|content|7 is committed on chain, so submitRoot must count as applied: " +
			"leaving it in `remaining` re-broadcasts a call the contract aborts on " +
			"(\"root already submitted for this epoch\"), burning its RC every run and " +
			"printing a false retry note, and it makes the \"epoch already fully " +
			"submitted\" exit unreachable for every epoch that has earners")
	}
	// The epoch is NOT finalized, so this must be the marker talking and not the
	// blanket `if finalized` sweep at the end of chainApplied.
	if applied["finalizeEpoch/-1"] {
		t.Fatal("status|content|7 is unset, so finalizeEpoch must still be outstanding — " +
			"if this is true the assertion above proved nothing")
	}
}

// The other direction: an uncommitted root must still be submitted. Guards against a
// fix that marks submitRoot done unconditionally, which would skip the one call that
// makes an epoch claimable and then finalize it — finalizeEpoch refuses an epoch with
// no root, so the run would die with the pages published and the funding pulled.
func TestSubmitRootMarker_MissingRootIsStillOutstanding(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|7":   "50000",
		"ssdone|content|7|0": "1",
		"ch_rMode|content":   "0",
	}, nil)

	applied, _, err := a.chainApplied(rootMarkerPlan())
	if err != nil {
		t.Fatal(err)
	}
	if applied["submitRoot/-1"] {
		t.Fatal("no root|content|7 on chain, so submitRoot is the one call that still has " +
			"to happen: skipping it would finalize an epoch nothing can ever claim against")
	}
}

// The finding's headline consequence, end to end: with every marker on chain there
// must be nothing left to do. This is the "epoch already fully submitted" exit, which
// was dead for any epoch with earners because submitRoot never cleared.
func TestSubmitRootMarker_FullySubmittedEpochHasNothingRemaining(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|7":   "50000",
		"ssdone|content|7|0": "1",
		"root|content|7":     rootA,
		"status|content|7":   "finalized",
		"ch_rMode|content":   "0",
	}, nil)

	pl := rootMarkerPlan()
	applied, _, err := a.chainApplied(pl)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pl.Calls {
		k := c.Action + "/" + strconv.Itoa(submit.PageOf(c))
		if !applied[k] {
			t.Fatalf("%s is still outstanding on a fully submitted epoch — the run would "+
				"re-broadcast it and the \"epoch already fully submitted\" exit stays dead", k)
		}
	}
}
