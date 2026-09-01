package setup

import (
	"strings"
	"testing"
)

func ids() Contracts {
	return Contracts{"token": "vsc1TOKEN", "c1": "vsc1C1", "c2": "vsc1C2", "c3": "vsc1C3"}
}

func callsOf(t *testing.T, p *Plan) []Step2Call {
	t.Helper()
	cs, err := p.Calls(ids())
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	return cs
}

func indexOf(cs []Step2Call, action string) int {
	for i, c := range cs {
		if c.Call.Action == action {
			return i
		}
	}
	return -1
}

// Deploying is not something this can do, and pretending otherwise would produce a
// plan that references ids that do not exist.
func TestCalls_RefusesUntilContractsAreDeployed(t *testing.T) {
	_, err := good().Calls(Contracts{"token": "vsc1TOKEN"})
	if err == nil {
		t.Fatal("must refuse to build calls before all four ids exist")
	}
	for _, want := range []string{"c1", "c2", "c3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name every missing contract, got %q", err)
		}
	}
}

// The ordering IS the plan. Each of these pairs is a step whose wrong position is
// not an error at the time — it is a contract that misbehaves later.
func TestCalls_OrderEncodesTheConstraints(t *testing.T) {
	p := good()
	p.C3.StakedBps, p.C3.StakeContract = 5000, "c1"
	p.C1.Allow = []string{"c3"}
	cs := callsOf(t, p)

	must := func(earlier, later, why string) {
		i, j := indexOf(cs, earlier), indexOf(cs, later)
		if i < 0 || j < 0 {
			t.Fatalf("missing call: %s=%d %s=%d", earlier, i, later, j)
		}
		if i >= j {
			t.Fatalf("%s must come before %s — %s", earlier, later, why)
		}
	}
	must("init", "adoptSchedule", "adoptSchedule needs C2's genesis and buckets (constraint 4)")
	must("approve", "adoptSchedule", "the pool must be approved before the schedule is armed")

	// C2.init must follow C1.init: stake has to exist before genesis is set, and
	// C1 is what holds it (constraint 3).
	var c1Init, c2Init int
	for i, c := range cs {
		if c.Call.Action == "init" {
			switch c.Call.ContractID {
			case "vsc1C1":
				c1Init = i
			case "vsc1C2":
				c2Init = i
			}
		}
	}
	if c1Init >= c2Init {
		t.Fatal("C1.init must precede C2.init: C2.init sets genesis, and stake arriving " +
			"after it is zero at both boundaries of epoch 0 (constraint 3)")
	}
}

// Symbolic names are what make a plan checkable before anything exists, so the
// resolution to real ids has to be exact — a bucket pointing at the wrong contract
// sends an epoch's emission somewhere it can never be claimed from.
func TestCalls_ResolvesSymbolicTargets(t *testing.T) {
	p := good()
	p.C2.Buckets = []Bucket{
		{Name: "content", Target: "c3", WeightBps: 5000},
		{Name: "grants", Target: "hive:treasury", WeightBps: 5000},
	}
	p.Channels[0].Bucket = "content"
	cs := callsOf(t, p)

	i := -1
	for n, c := range cs {
		if c.Call.Action == "init" && c.Call.ContractID == "vsc1C2" {
			i = n
		}
	}
	pay := cs[i].Call.Payload
	if !strings.Contains(pay, "content:contract:vsc1C3:5000") {
		t.Fatalf("symbolic c3 must resolve to contract:vsc1C3, got %s", pay)
	}
	if !strings.Contains(pay, "grants:hive:treasury:5000") {
		t.Fatalf("a plain address must pass through untouched, got %s", pay)
	}
}

// allow holds ADDRESSES on chain, not symbolic names — writing "c3" verbatim would
// allowlist an account that does not exist and every staked claim would abort.
func TestCalls_AllowListResolvesToContractAddress(t *testing.T) {
	p := good()
	p.C3.StakedBps, p.C3.StakeContract = 5000, "c1"
	p.C1.Allow = []string{"c3"}
	cs := callsOf(t, p)
	for _, c := range cs {
		if c.Call.Action == "init" && c.Call.ContractID == "vsc1C1" {
			if !strings.Contains(c.Call.Payload, `"allow":"contract:vsc1C3"`) {
				t.Fatalf("allow must resolve to a contract address, got %s", c.Call.Payload)
			}
			return
		}
	}
	t.Fatal("no C1 init call")
}

// required_auths is a flat_set the node re-serialises SORTED. Writing the channel's
// authority list in the same order a cosigning client will build means the two
// cannot disagree about what was configured.
func TestCalls_ReporterAuthIsSorted(t *testing.T) {
	p := good()
	p.Channels[0].Reporter = Auth{Mode: 1, Auth: []string{"hive:zeta", "hive:alpha"}, Threshold: 2}
	cs := callsOf(t, p)
	i := indexOf(cs, "addChannel")
	if !strings.Contains(cs[i].Call.Payload, `"reporterAuth":"hive:alpha,hive:zeta"`) {
		t.Fatalf("authorities must be sorted, got %s", cs[i].Call.Payload)
	}
}

// A step the deployer cannot sign must still appear, with the account that can. A
// runner that silently skipped it would leave C2 unable to draw the pool and the
// failure would surface an epoch later as a starved bucket.
func TestCalls_ForeignSignerIsNamedNotSkipped(t *testing.T) {
	p := good()
	p.C2.Source = "hive:separatepool"
	cs := callsOf(t, p)
	for _, c := range cs {
		if c.Call.Action == "approve" {
			if c.Signer != "hive:separatepool" {
				t.Fatalf("the approve must be attributed to the pool holder, got %q", c.Signer)
			}
			return
		}
	}
	t.Fatal("the pool approve must be present even when the deployer cannot sign it")
}

// Without a stake contract there is no staked split, and emitting stakedBps anyway
// would make claims route through stakeFor and abort.
func TestCalls_NoStakeContractMeansNoStakedBps(t *testing.T) {
	cs := callsOf(t, good()) // good() has no staked payouts
	for _, c := range cs {
		if c.Call.Action == "init" && c.Call.ContractID == "vsc1C3" {
			if strings.Contains(c.Call.Payload, "stakedBps") {
				t.Fatalf("stakedBps must be absent without a stake contract, got %s", c.Call.Payload)
			}
			return
		}
	}
	t.Fatal("no C3 init call")
}
