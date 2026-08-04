package submit

import (
	"testing"

	"magi_token/reporter/sharecore"
)

// Ordering is a safety property, not a style choice: pullFunding stamps the anchor
// for the guardian's stale-rescue deadline, so it must come AFTER the pages and
// before finalize. Pulling first spends the whole rescue window paginating.
func TestBuildFullPlan_PagesPrecedeFundingAndFinalizeIsLast(t *testing.T) {
	pl := BuildFullPlan(PlanOpts{
		Channel: "content",
		Epoch:   "7", DistributorID: "vsc1C3", FunderID: "vsc1C2",
		PullFunding: true, Finalize: true, RcLimit: 10000,
		Pages: []sharecore.Page{
			{Index: 0, Entries: "hive:a:1", Count: 1},
			{Index: 1, Entries: "hive:b:2", Count: 1},
		},
	})
	var order []string
	for _, c := range pl.Calls {
		order = append(order, c.Action)
	}
	want := []string{"distributeEpoch", "submitShares", "submitShares", "pullFunding", "finalizeEpoch"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call %d is %s, want %s (full order %v)", i, order[i], want[i], order)
		}
	}
}

// With finalize off, pullFunding must still land after the pages and stay last.
func TestBuildFullPlan_OrderingHoldsWithoutFinalize(t *testing.T) {
	pl := BuildFullPlan(PlanOpts{
		Channel: "content",
		Epoch:   "7", DistributorID: "vsc1C3", PullFunding: true, Finalize: false,
		Pages: []sharecore.Page{{Index: 0, Entries: "hive:a:1", Count: 1}},
	})
	if n := len(pl.Calls); n < 2 || pl.Calls[n-1].Action != "pullFunding" {
		t.Fatalf("pullFunding must be last when finalize is off, got %v", pl.Calls)
	}
}
