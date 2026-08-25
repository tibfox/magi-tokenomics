package setup

import "testing"

// good returns a plan that deploys cleanly, so each test can break exactly one
// thing. A shared fixture that already violates something would let a test pass for
// the wrong reason.
func good() *Plan {
	p := &Plan{Deployer: "hive:deployer"}
	p.Token.Name, p.Token.Symbol, p.Token.MaxSupply = "MAGI", "MAGI", "100000000"
	p.Token.Mint = "1000000"
	p.C1.Cooldown, p.C1.EpochLen = 100, 50
	p.C1.Treasury = "hive:treasury"
	p.C1.Guardian = Auth{Mode: 0, Auth: []string{"hive:guard"}, Threshold: 1}
	p.C2.EpochLen, p.C2.BaseAnnual, p.C2.BlocksPerYear = 50, "1000000", 1000
	p.C2.MaxCatch, p.C2.DustBucket, p.C2.Source = 5, "content", "hive:pool"
	p.C2.Buckets = []Bucket{
		{Name: "content", Target: "c3", WeightBps: 5000},
		{Name: "yield", Target: "c1", WeightBps: 5000},
	}
	p.C3.Treasury = "hive:treasury"
	p.C3.Guardian = Auth{Mode: 0, Auth: []string{"hive:guard"}, Threshold: 1}
	p.Channels = []Channel{{
		Name: "content", Bucket: "content", Window: 50, Role: "content",
		Reporter: Auth{Mode: 0, Auth: []string{"hive:reporter"}, Threshold: 1},
	}}
	return p
}

func find(probs []Problem, field string) *Problem {
	for i := range probs {
		if probs[i].Field == field {
			return &probs[i]
		}
	}
	return nil
}

func TestCheck_CleanPlanHasNoProblems(t *testing.T) {
	if probs := good().Check(); len(probs) != 0 {
		for _, p := range probs {
			t.Errorf("unexpected: %s", p)
		}
		t.Fatal("a clean plan must produce no problems, or every other test here is meaningless")
	}
}

// The trap that cost the first testnet deployment its staked payouts. `allow` is
// written only at C1.init; get it wrong and claims abort at the point of PAYMENT,
// which is discovered when the first earner tries to get paid.
func TestCheck_StakedPayoutsRequireC3InAllow(t *testing.T) {
	p := good()
	p.C3.StakedBps, p.C3.StakeContract = 5000, "c1" // want staked payouts...
	// ...but forget c1.allow, exactly as the real deployment did.
	probs := p.Check()
	got := find(probs, "c1.allow")
	if got == nil {
		t.Fatalf("staked payouts with an empty allow list must be refused; got %v", probs)
	}
	if got.Constraint != 6 {
		t.Fatalf("should cite constraint 6, cites %d", got.Constraint)
	}
	// And it must pass once the list is right.
	p.C1.Allow = []string{"c3"}
	if got := find(p.Check(), "c1.allow"); got != nil {
		t.Fatalf("allow=[c3] with staked payouts must be accepted, got %s", got)
	}
}

// The mirror image: an allowlist with nothing to use it is dead config that reads
// as a live capability.
func TestCheck_AllowWithoutStakedPayoutsIsRefused(t *testing.T) {
	p := good()
	p.C1.Allow = []string{"c3"} // but StakedBps stays 0
	if got := find(p.Check(), "c1.allow"); got == nil {
		t.Fatal("an allowlist with no staked payouts configured must be refused as dead config")
	}
}

// The trap that made a cosigned channel impossible on the first deployment, and
// which I then walked into a second time on v3 while holding the constraint in mind.
func TestCheck_ReporterAndGuardianMustBeDisjoint(t *testing.T) {
	p := good()
	p.C3.Guardian.Auth = []string{"hive:alice"}
	p.Channels[0].Reporter.Auth = []string{"hive:alice", "hive:bob"}
	p.Channels[0].Reporter.Threshold = 2
	p.Channels[0].Reporter.Mode = 1

	got := find(p.Check(), "channels.content.reporter.auth")
	if got == nil {
		t.Fatal("a reporter that is also a guardian must be refused BEFORE C3.init makes the guardian immutable")
	}
	if got.Constraint != 7 {
		t.Fatalf("should cite constraint 7, cites %d", got.Constraint)
	}
	// Case must not matter: the chain compares exact strings, and an operator who
	// writes one set capitalised differently still has the same account.
	p.C3.Guardian.Auth = []string{"HIVE:Alice"}
	if find(p.Check(), "channels.content.reporter.auth") == nil {
		t.Fatal("overlap must be detected regardless of case")
	}
}

// Attest is the mode where reporters can disagree, so the channel must say what
// they are all scoring. addChannel aborts without it and says nothing useful.
func TestCheck_AttestChannelNeedsAPolicyDigest(t *testing.T) {
	p := good()
	p.Channels[0].Reporter = Auth{Mode: 2, Auth: []string{"hive:a", "hive:b", "hive:c"}, Threshold: 2}
	got := find(p.Check(), "channels.content.policy")
	if got == nil || got.Constraint != 5 {
		t.Fatalf("attest mode without a policy digest must be refused citing constraint 5, got %v", got)
	}
	p.Channels[0].Policy = "a96ce952"
	if find(p.Check(), "channels.content.policy") != nil {
		t.Fatal("a declared policy must satisfy the check")
	}
}

// A threshold above the authority count can never be reached, so the channel is
// dead the moment it is created — and channels are append-only.
func TestCheck_ThresholdMustBeReachable(t *testing.T) {
	p := good()
	p.Channels[0].Reporter = Auth{Mode: 2, Auth: []string{"hive:a", "hive:b"}, Threshold: 3, }
	p.Channels[0].Policy = "d"
	if find(p.Check(), "channels.content.reporter.threshold") == nil {
		t.Fatal("an unreachable threshold must be refused")
	}
}

func TestCheck_ScheduleAndEmission(t *testing.T) {
	t.Run("cooldown must exceed epoch length (R15)", func(t *testing.T) {
		p := good()
		p.C1.Cooldown = p.C1.EpochLen
		if find(p.Check(), "c1.cooldown") == nil {
			t.Fatal("cooldown == epochLen must be refused")
		}
	})
	t.Run("C1 and C2 must agree on epoch length", func(t *testing.T) {
		p := good()
		p.C2.EpochLen = 60
		got := find(p.Check(), "c2.epoch_len")
		if got == nil || got.Constraint != 4 {
			t.Fatalf("a mismatch must be refused citing constraint 4, got %v", got)
		}
	})
	t.Run("emission must not round to zero", func(t *testing.T) {
		p := good()
		p.C2.BaseAnnual, p.C2.BlocksPerYear = "10", 1000000
		if find(p.Check(), "c2.base_annual") == nil {
			t.Fatal("an emission that rounds to zero must be refused: pokes would mark " +
				"epochs funded forever while paying nobody")
		}
	})
}

func TestCheck_BucketsAndChannels(t *testing.T) {
	t.Run("weights must sum to 10000", func(t *testing.T) {
		p := good()
		p.C2.Buckets[0].WeightBps = 4000
		if find(p.Check(), "c2.buckets") == nil {
			t.Fatal("weights summing to 9000 must be refused")
		}
	})
	t.Run("one bucket cannot fund two channels", func(t *testing.T) {
		p := good()
		p.Channels = append(p.Channels, Channel{
			Name: "second", Bucket: "content", Window: 50,
			Reporter: Auth{Mode: 0, Auth: []string{"hive:r2"}, Threshold: 1},
		})
		if find(p.Check(), "channels.second.bucket") == nil {
			t.Fatal("a bucket already funding a channel must be refused for a second")
		}
	})
	t.Run("dust bucket must name a real bucket", func(t *testing.T) {
		p := good()
		p.C2.DustBucket = "nosuch"
		if find(p.Check(), "c2.dust_bucket") == nil {
			t.Fatal("a dust bucket naming nothing must be refused")
		}
	})
	t.Run("window must not exceed 10x the epoch", func(t *testing.T) {
		p := good()
		p.Channels[0].Window = 10*p.C2.EpochLen + 1
		if find(p.Check(), "channels.content.window") == nil {
			t.Fatal("an oversized immutable window must be refused")
		}
	})
}

// Every problem must be reported in one pass. These are paid for in deploy fees:
// finding them one per attempt costs 10 HBD and a round trip each.
func TestCheck_ReportsEveryProblemAtOnce(t *testing.T) {
	p := good()
	p.C1.Cooldown = 1                   // R15
	p.C2.Buckets[0].WeightBps = 1       // weights
	p.C2.DustBucket = "nosuch"          // dust
	p.C3.Treasury = ""                  // required
	probs := p.Check()
	if len(probs) < 4 {
		t.Fatalf("expected at least 4 problems in one pass, got %d: %v", len(probs), probs)
	}
}
