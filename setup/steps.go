package setup

import "fmt"

// Step is one action in the deployment, in the order it must happen.
type Step struct {
	N       int
	Actor   string // who signs it
	Target  string // symbolic contract, or "" for a note
	Action  string
	Why     string // the constraint or reason this step sits HERE and not elsewhere
}

func (s Step) String() string {
	if s.Target == "" {
		return fmt.Sprintf("%2d. %s", s.N, s.Action)
	}
	return fmt.Sprintf("%2d. %-14s %s.%s", s.N, s.Actor, s.Target, s.Action)
}

// Steps returns the ordered plan.
//
// The order is the point. Every entry carries WHY it sits where it does, because
// each of the reorderings that look harmless costs something permanent: staking
// after C2.init silently strands epoch 0's yield, initialising C1 before C3 exists
// makes staked payouts impossible forever, and picking C3's guardian before the
// reporter set is known can rule out a cosigned channel entirely.
func (p *Plan) Steps() []Step {
	var out []Step
	n := 0
	add := func(actor, target, action, why string) {
		n++
		out = append(out, Step{N: n, Actor: actor, Target: target, Action: action, Why: why})
	}
	d := p.Deployer

	add(d, "", "deploy token, c1, c2, c3 — ALL FOUR, before any init",
		"constraint 6: C1.init needs C3's id for its allow list, and allow is immutable. "+
			"Deploying all four first is the only order that leaves that option open.")
	add(d, "", "deposit HBD for RC",
		"constraint 1: each deploy costs 10 HBD of L1 balance, so deposit AFTER deploying "+
			"or there is nothing left to pay the fee with.")
	add(d, "token", "init", "constraint 2: only the deploying account may init.")
	add(d, "token", "mint", "mint credits the OWNER; there is no `to` field.")
	if p.C1.MaxAirdrop != "" {
		add(d, "token", "transfer -> contract:c1 (airdrop float)",
			"the airdrop pays from C1's OWN balance, so the float must arrive before "+
				"the deployer's supply is spent elsewhere.")
	}
	add(d, "token", "transfer -> "+p.C2.Source+" (emission pool)",
		"C2 draws the pool by allowance from this account; it never mints.")
	add(d, "c1", "init", "constraint 6: carries the allow list, which can never be changed.")
	if p.C1.MaxAirdrop != "" {
		add(d, "c1", "airdropBatch", "seeds holders from the float transferred above.")
	}
	add("stakers", "token", "approve -> contract:c1", "staking is allowance-based.")
	add("stakers", "c1", "stake",
		"constraint 3: stake MUST land before C2.init. Yield credits "+
			"min(stakeAt(start), stakeAt(end)), so stake arriving after genesis is zero at "+
			"both boundaries and epoch 0's yield is funded but permanently unclaimable.")
	add(p.C2.Source, "token", "approve -> contract:c2",
		"lets C2 draw each epoch's emission from the pool.")
	add(d, "c2", "init", "sets `genesis` to THIS block — the clock starts here.")
	add(d, "c1", "adoptSchedule", "constraint 4: needs C2's genesis and buckets, so it "+
		"cannot happen at C1.init. Owner-only, once, and pullFunding refuses until it has run.")
	add(d, "c3", "init", "constraint 7: fixes the guardian set FOREVER. Every reporter "+
		"below must be disjoint from it, and addChannel is where you find out.")
	for _, ch := range p.Channels {
		why := "channels are append-only: only setPolicy can change afterwards."
		if ch.Reporter.Mode == 2 {
			why = "constraint 5: attest mode MUST carry a policy digest. " + why
		}
		add(d, "c3", "addChannel "+ch.Name, why)
	}
	add("", "", "do NOT hand token ownership to C2",
		"C2 has no entrypoint that could use it, so a handover strands the token's own "+
			"mint/pause/changeOwner permanently.")
	return out
}
