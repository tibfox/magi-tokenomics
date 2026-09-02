package main

import "testing"

// lookback is an OPERATOR's setting, not a policy one, and hashing it locked
// reporters out of their own recovery path.
//
// policySections states the rule plainly: "The split is by whether a setting can
// change the numbers, not by whether it looks like policy", and excludes everything
// under Submit as per-operator. Lookback fails that rule on its own terms. It feeds
// exactly one thing — windowFloor, which decides how far back the DEFAULT epoch
// search looks. It cannot move a single share: once an epoch is chosen, the book
// comes from the window (genesis/len), Source, Shares and Page.
//
// Hashing it had three consequences, and the second is the one that matters:
//
//  1. Two honest reporters with different lookbacks compute identical books for the
//     same epoch and are still refused as divergent.
//  2. The reporter's OWN advice after downtime is "Raise epoch.lookback (currently N)
//     and re-run to work them off." Following it changed the digest, so the reporter
//     was then refused for a policy mismatch — and the fix, setPolicy, is OWNER-only.
//     An operator recovering from downtime could not follow the instruction the tool
//     had just printed without the channel owner intervening.
//  3. In Attest mode, raising lookback silently drops that reporter out of the
//     quorum: it stops matching, so it stops voting, and the epoch stalls.
func TestPolicyDigest_LookbackIsNotPolicy(t *testing.T) {
	base := func(lookback uint64) *Config {
		c := &Config{}
		c.Epoch.Genesis, c.Epoch.Len, c.Epoch.Lookback = 5945496, 50, lookback
		c.Source.Kind = "lp"
		return c
	}

	// The recovery case verbatim: a reporter that has been down raises lookback so
	// the backlog is reachable again.
	quiet, err := base(0).PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}
	recovering, err := base(5000).PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}
	if quiet != recovering {
		t.Fatalf("raising lookback changed the policy digest (%s -> %s).\n"+
			"That refuses a reporter for following the advice the tool prints after "+
			"downtime, and setPolicy is owner-only, so it cannot fix itself.",
			quiet[:16], recovering[:16])
	}
}

// The narrowing must not go too far: genesis and len decide which blocks the epoch
// covers at all, so they must still be part of what two reporters agree on.
func TestPolicyDigest_WindowStillCounts(t *testing.T) {
	mk := func(genesis, length uint64) string {
		c := &Config{}
		c.Epoch.Genesis, c.Epoch.Len = genesis, length
		c.Source.Kind = "lp"
		d, err := c.PolicyDigest()
		if err != nil {
			t.Fatalf("PolicyDigest: %v", err)
		}
		return d
	}
	ref := mk(5945496, 50)
	if mk(5945497, 50) == ref {
		t.Fatal("genesis must change the digest — it decides which blocks the epoch covers")
	}
	if mk(5945496, 51) == ref {
		t.Fatal("epoch length must change the digest — it decides the window")
	}
}
