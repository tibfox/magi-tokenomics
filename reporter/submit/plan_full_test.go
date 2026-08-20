package submit

import (
	"strings"
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

// The commitment's place in the order is a safety property. finalizeEpoch refuses
// an epoch with no root, so submitRoot has to land before it — and after the pages,
// because the root has to commit to leaves that are already published.
func TestBuildFullPlan_RootLandsAfterThePagesAndBeforeFinalize(t *testing.T) {
	pl := BuildFullPlan(PlanOpts{
		Channel: "content", Epoch: "7", DistributorID: "vsc1C3", FunderID: "vsc1C2",
		PullFunding: true, Finalize: true, RcLimit: 10000,
		Root:        "aa" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
		TotalShares: "3", Accounts: 2,
		Pages: []sharecore.Page{
			{Index: 0, Entries: "hive:a:1", Count: 1},
			{Index: 1, Entries: "hive:b:2", Count: 1},
		},
	})
	var order []string
	for _, c := range pl.Calls {
		order = append(order, c.Action)
	}
	want := []string{"distributeEpoch", "submitShares", "submitShares", "submitRoot",
		"pullFunding", "finalizeEpoch"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call %d is %s, want %s (full order %v)", i, order[i], want[i], order)
		}
	}
	var rootCall Call
	for _, c := range pl.Calls {
		if c.Action == "submitRoot" {
			rootCall = c
		}
	}
	for _, want := range []string{`"channel":"content"`, `"epoch":"7"`, `"totalShares":"3"`, `"accounts":"2"`} {
		if !contains(rootCall.Payload, want) {
			t.Errorf("submitRoot payload is missing %s: %s", want, rootCall.Payload)
		}
	}
}

// An empty Root used to drop submitRoot from the plan silently, and the plan then
// went on to finalize an epoch nothing could claim. On chain the run dies at
// finalize with the pages already published and the funding already pulled.
func TestPlanOptsValidate_RefusesPagesWithNoCommitment(t *testing.T) {
	base := PlanOpts{
		Channel: "content", Epoch: "7", DistributorID: "vsc1C3", Finalize: true,
		Root:        "aa0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
		TotalShares: "3", Accounts: 2,
		Pages: []sharecore.Page{{Index: 0, Entries: "hive:a:1", Count: 1}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a complete plan was refused: %v", err)
	}
	for name, mut := range map[string]func(*PlanOpts){
		"no root":          func(o *PlanOpts) { o.Root = "" },
		"short root":       func(o *PlanOpts) { o.Root = "abc" },
		"uppercase root":   func(o *PlanOpts) { o.Root = strings.ToUpper(base.Root) },
		"non-hex root":     func(o *PlanOpts) { o.Root = base.Root[:63] + "z" },
		"no denominator":   func(o *PlanOpts) { o.TotalShares = "" },
		"zero denominator": func(o *PlanOpts) { o.TotalShares = "0" },
		"no accounts":      func(o *PlanOpts) { o.Accounts = 0 },
	} {
		o := base
		mut(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted, but this plan cannot succeed on chain", name)
		}
	}
	// nothing to publish is not an error — an empty epoch has nothing to commit to
	empty := base
	empty.Pages, empty.Root, empty.TotalShares, empty.Accounts = nil, "", "", 0
	if err := empty.Validate(); err != nil {
		t.Errorf("an epoch with no pages was refused: %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
