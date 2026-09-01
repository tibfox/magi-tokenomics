package setup

import (
	"fmt"
	"math/big"
	"strings"
)

// Check returns every reason this plan must not be deployed.
//
// It returns ALL problems rather than the first, because these are paid for in
// deploy fees: finding three at once costs one read, finding them one per attempt
// costs 30 HBD and three rounds of waiting. Nothing here touches the network — a
// plan is checkable before an account is even funded.
func (p *Plan) Check() []Problem {
	var probs []Problem
	add := func(c int, field, format string, a ...any) {
		probs = append(probs, Problem{Constraint: c, Field: field, Detail: fmt.Sprintf(format, a...)})
	}

	// ---- the two that were hit for real -----------------------------------

	// Constraint 6. `allow` is written only in C1.Init. With stakedBps set, every
	// claim routes part of its value through C1.stakeFor, which aborts for callers
	// outside the list — so an empty allow does not disable staked payouts, it makes
	// every claim abort AT THE POINT OF PAYMENT.
	wantsStaked := p.C3.StakedBps > 0 || norm(p.C3.StakeContract) != ""
	if wantsStaked {
		if p.C3.StakedBps <= 0 || p.C3.StakedBps > 10000 {
			add(6, "c3.staked_bps", "must be 1..10000 when stake_contract is set, got %d", p.C3.StakedBps)
		}
		if norm(p.C3.StakeContract) == "" {
			add(6, "c3.stake_contract", "staked_bps is set but no stake contract is named")
		}
		if !contains(p.C1.Allow, "c3") {
			add(6, "c1.allow",
				"staked payouts need \"c3\" in C1's allow list, and allow is written ONLY at "+
					"C1.init — nothing can add to it later. Without it every claim on a "+
					"staked channel aborts at the point of payment, not at configuration time")
		}
	} else if len(p.C1.Allow) > 0 {
		add(6, "c1.allow",
			"allow lists %v but no staked payouts are configured — the two must agree, "+
				"or the allowlist is dead config that looks live", p.C1.Allow)
	}

	// Constraint 7. Reporter and guardian must be disjoint so one coalition cannot
	// both publish fraud and refuse to cancel it. The guardian is immutable at
	// C3.init while the clash only surfaces at addChannel, several steps later.
	for _, ch := range p.Channels {
		if bad := overlap(ch.Reporter.Auth, p.C3.Guardian.Auth); len(bad) > 0 {
			add(7, "channels."+ch.Name+".reporter.auth",
				"%v are also C3 guardians. addChannel refuses this, and the guardian is "+
					"already immutable by then — pick the guardian LAST, and count the keys "+
					"you actually hold before spending one on it", bad)
		}
		if bad := overlap(ch.Reporter.Auth, []string{p.C3.Treasury}); len(bad) > 0 {
			add(0, "channels."+ch.Name+".reporter.auth",
				"the pinned treasury %v must not be a reporter", bad)
		}
	}
	if bad := overlap(p.C1.Guardian.Auth, []string{p.C1.Treasury}); len(bad) > 0 {
		add(0, "c1.treasury", "%v is also a C1 guardian — a sweep would be a drain", bad)
	}
	if bad := overlap(p.C3.Guardian.Auth, []string{p.C3.Treasury}); len(bad) > 0 {
		add(0, "c3.treasury", "%v is also a C3 guardian — a sweep would be a drain", bad)
	}

	// Constraint 5. Attest is the mode where reporters can disagree, so the channel
	// has to declare what they are all scoring. addChannel aborts without it and the
	// message does not say why.
	for _, ch := range p.Channels {
		if ch.Reporter.Mode == 2 && strings.TrimSpace(ch.Policy) == "" {
			add(5, "channels."+ch.Name+".policy",
				"an attest-mode channel MUST declare a policy digest "+
					"(`reporter policy-digest -config F`)")
		}
		if ch.Reporter.Mode < 0 || ch.Reporter.Mode > 2 {
			add(0, "channels."+ch.Name+".reporter.mode", "must be 0 (single), 1 (cosigned) or 2 (attest)")
		}
		if n := len(nonEmpty(ch.Reporter.Auth)); ch.Reporter.Threshold < 1 || ch.Reporter.Threshold > n {
			add(0, "channels."+ch.Name+".reporter.threshold",
				"threshold %d is not 1..%d — a threshold above the authority count can "+
					"never be reached and the channel is dead on arrival",
				ch.Reporter.Threshold, n)
		}
	}

	// ---- schedule and emission --------------------------------------------

	// R15: an unstake must not outlive the epoch it was counted in.
	if p.C1.Cooldown <= p.C1.EpochLen {
		add(0, "c1.cooldown", "must be greater than c1.epoch_len (%d), got %d (R15)",
			p.C1.EpochLen, p.C1.Cooldown)
	}
	if p.C1.EpochLen != p.C2.EpochLen {
		add(4, "c2.epoch_len",
			"C1 and C2 must agree on epoch length (%d vs %d) — adoptSchedule refuses a "+
				"mismatch, and R15's cooldown check would otherwise be made against the "+
				"wrong epoch", p.C1.EpochLen, p.C2.EpochLen)
	}
	if p.C2.EpochLen <= 0 {
		add(0, "c2.epoch_len", "must be > 0")
	}
	if p.C2.BlocksPerYear <= 0 {
		add(0, "c2.blocks_per_year", "must be > 0")
	}
	if p.C2.MaxCatch < 1 || p.C2.MaxCatch > 1000 {
		add(0, "c2.max_catch", "must be 1..1000, got %d", p.C2.MaxCatch)
	}
	// A zero per-epoch emission does not fail visibly: pokes keep marking epochs
	// done while funding nothing, forever, each reporting success.
	if base, ok := new(big.Int).SetString(strings.TrimSpace(p.C2.BaseAnnual), 10); !ok {
		add(0, "c2.base_annual", "must be a decimal integer, got %q", p.C2.BaseAnnual)
	} else if p.C2.EpochLen > 0 && p.C2.BlocksPerYear > 0 {
		per := new(big.Int).Mul(base, big.NewInt(int64(p.C2.EpochLen)))
		per.Div(per, big.NewInt(int64(p.C2.BlocksPerYear)))
		if per.Sign() <= 0 {
			add(0, "c2.base_annual",
				"emission rounds to zero: base_annual*epoch_len/blocks_per_year < 1, so "+
					"every poke would mark epochs funded while paying nobody")
		}
	}

	// The airdrop float is transferred out of the mint before anything else. Sized
	// at or above the whole mint it leaves nothing to approve to C2, and the first
	// poke starves a pool that was never funded — which reads on chain as a working
	// deployment that simply never pays.
	mint, mintOK := new(big.Int).SetString(strings.TrimSpace(p.Token.Mint), 10)
	if strings.TrimSpace(p.Token.Mint) != "" && !mintOK {
		add(0, "token.mint", "must be a decimal integer, got %q", p.Token.Mint)
	}
	if air, ok := new(big.Int).SetString(strings.TrimSpace(p.C1.MaxAirdrop), 10); ok && mintOK {
		if air.Cmp(mint) >= 0 {
			add(0, "c1.max_airdrop",
				"the airdrop float (%s) is not less than the mint (%s) — it is transferred "+
					"to C1 before the pool is approved, so emission would have nothing to draw",
				air, mint)
		}
	}
	if maxSup, ok := new(big.Int).SetString(strings.TrimSpace(p.Token.MaxSupply), 10); ok && mintOK {
		if mint.Cmp(maxSup) > 0 {
			add(0, "token.mint", "mint (%s) exceeds max_supply (%s)", mint, maxSup)
		}
	}

	// ---- buckets and channels ---------------------------------------------

	sum, names := 0, map[string]bool{}
	for _, b := range p.C2.Buckets {
		if norm(b.Name) == "" {
			add(0, "c2.buckets", "a bucket has no name")
			continue
		}
		if names[norm(b.Name)] {
			add(0, "c2.buckets."+b.Name, "duplicate bucket name — names identify allocations")
		}
		names[norm(b.Name)] = true
		sum += b.WeightBps
		if norm(b.Target) == "c2" {
			add(0, "c2.buckets."+b.Name, "a bucket cannot pay C2 itself (R21)")
		}
	}
	if len(p.C2.Buckets) == 0 {
		add(0, "c2.buckets", "at least one bucket is required")
	} else if sum != 10000 {
		add(0, "c2.buckets", "weights must sum to 10000 bps, got %d", sum)
	}
	if d := norm(p.C2.DustBucket); d != "" && !names[d] {
		add(0, "c2.dust_bucket", "%q names no configured bucket", p.C2.DustBucket)
	}
	if norm(p.C2.Source) == "c2" {
		add(0, "c2.source", "the source cannot be C2 itself — a token cannot approve itself")
	}
	if norm(p.C2.Source) == "" {
		add(0, "c2.source", "required: the account whose approved balance funds emission")
	}

	usedBucket := map[string]string{}
	for _, ch := range p.Channels {
		if norm(ch.Name) == "" {
			add(0, "channels", "a channel has no name")
			continue
		}
		if strings.ContainsAny(ch.Name, "|:,") {
			add(0, "channels."+ch.Name, "a channel name must not contain | : or ,")
		}
		if !names[norm(ch.Bucket)] {
			add(0, "channels."+ch.Name+".bucket", "%q is not a configured C2 bucket", ch.Bucket)
		} else if prev, dup := usedBucket[norm(ch.Bucket)]; dup {
			add(0, "channels."+ch.Name+".bucket",
				"bucket %q already funds channel %q — each bucket may fund only one", ch.Bucket, prev)
		} else {
			usedBucket[norm(ch.Bucket)] = ch.Name
		}
		if ch.Window <= 0 {
			add(0, "channels."+ch.Name+".window", "must be > 0")
		} else if p.C2.EpochLen > 0 && ch.Window > 10*p.C2.EpochLen {
			add(0, "channels."+ch.Name+".window",
				"%d is more than 10x the epoch length (%d) — an immutable window that "+
					"long is a typo", ch.Window, p.C2.EpochLen)
		}
		if r := norm(ch.Role); r != "" && r != "content" && r != "lp" {
			add(0, "channels."+ch.Name+".role", "must be \"content\" or \"lp\" if set, got %q", ch.Role)
		}
	}

	// ---- basics ------------------------------------------------------------
	if norm(p.Deployer) == "" {
		add(2, "deployer", "required: the account that deploys is the only one that may init")
	}
	if norm(p.C3.Treasury) == "" {
		add(0, "c3.treasury", "required — it is the pinned destination for cancel/sweep")
	}
	for _, f := range []struct{ name, val string }{
		{"token.name", p.Token.Name}, {"token.symbol", p.Token.Symbol},
		{"token.max_supply", p.Token.MaxSupply},
	} {
		if strings.TrimSpace(f.val) == "" {
			add(0, f.name, "required")
		}
	}
	return probs
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if norm(s) == norm(want) {
			return true
		}
	}
	return false
}

func nonEmpty(list []string) []string {
	var out []string
	for _, s := range list {
		if norm(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
