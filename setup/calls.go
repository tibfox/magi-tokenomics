package setup

import (
	"fmt"
	"sort"
	"strings"

	"magi_token/reporter/submit"
)

// Contracts maps the plan's symbolic names to deployed ids, filled in after the
// four deploys. Keys: token, c1, c2, c3.
type Contracts map[string]string

// Missing lists the symbolic names that have no id yet.
func (c Contracts) Missing() []string {
	var out []string
	for _, k := range []string{"token", "c1", "c2", "c3"} {
		if strings.TrimSpace(c[k]) == "" {
			out = append(out, k)
		}
	}
	return out
}

// addr turns a symbolic name into a ledger address. A name we know is a contract;
// anything already carrying a domain ("hive:alice") is passed through untouched, so
// a bucket can pay a plain account exactly as it can pay a contract.
func (c Contracts) addr(nameOrAddr string) (string, error) {
	s := strings.TrimSpace(nameOrAddr)
	if strings.Contains(s, ":") {
		return s, nil // already an address
	}
	id, ok := c[norm(s)]
	if !ok || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("no deployed id for contract %q", s)
	}
	return "contract:" + id, nil
}

// Step2Call is one executable step: the call plus who must sign it.
type Step2Call struct {
	Signer string // "hive:..." — the account whose active key must sign
	Call   submit.Call
	Why    string
}

// Calls turns a checked plan into the ordered calls that deploy it.
//
// Ordering here is not a convenience, it is the plan: several of these steps are
// only correct in one position, and the wrong position is not an error at the time
// — it is a contract that behaves wrongly later. The reasons ride along on each
// call so a runner can print them as it goes.
//
// Calls that the DEPLOYER cannot sign (a staker's own stake, an approve from a
// separate pool account) are included with their real signer so a runner can stop
// and say who is needed, rather than silently skipping a step the plan depends on.
func (p *Plan) Calls(ids Contracts) ([]Step2Call, error) {
	if miss := ids.Missing(); len(miss) > 0 {
		return nil, fmt.Errorf("deploy first: no id yet for %s", strings.Join(miss, ", "))
	}
	tok, c1, c2, c3 := ids["token"], ids["c1"], ids["c2"], ids["c3"]
	d := p.Deployer
	var out []Step2Call
	add := func(signer, contract, action, payload, why string) {
		out = append(out, Step2Call{
			Signer: signer,
			Call:   submit.Call{ContractID: contract, Action: action, Payload: payload, RcLimit: 60000},
			Why:    why,
		})
	}

	add(d, tok, "init", fmt.Sprintf(`{"name":%q,"symbol":%q,"decimals":%d,"maxSupply":%q}`,
		p.Token.Name, p.Token.Symbol, p.Token.Decimals, p.Token.MaxSupply),
		"constraint 2: only the deploying account may init")

	if m := strings.TrimSpace(p.Token.Mint); m != "" {
		add(d, tok, "mint", fmt.Sprintf(`{"amount":%q}`, m),
			"mint credits the OWNER; there is no `to` field")
	}
	if a := strings.TrimSpace(p.C1.MaxAirdrop); a != "" {
		add(d, tok, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":%q}`, c1, a),
			"the airdrop pays from C1's own balance, so the float must arrive first")
	}

	allow, err := resolveList(ids, p.C1.Allow)
	if err != nil {
		return nil, fmt.Errorf("c1.allow: %w", err)
	}
	add(d, c1, "init", fmt.Sprintf(
		`{"token":%q,"kind":"0","cooldown":"%d","epochLen":"%d","allow":%q,"treasury":%q,`+
			`"guardianMode":"%d","guardianAuth":%q,"guardianThreshold":"%d","maxAirdrop":%q}`,
		tok, p.C1.Cooldown, p.C1.EpochLen, strings.Join(allow, ","), p.C1.Treasury,
		p.C1.Guardian.Mode, strings.Join(p.C1.Guardian.Auth, ","), p.C1.Guardian.Threshold,
		p.C1.MaxAirdrop),
		"constraint 6: carries the allow list, which can never be changed afterwards")

	// The pool holder approves C2. If that is not the deployer, someone else signs.
	add(p.C2.Source, tok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":%q}`, c2, p.Token.Mint),
		"lets C2 draw each epoch's emission; C2 never mints")

	buckets, err := bucketSpec(ids, p.C2.Buckets)
	if err != nil {
		return nil, err
	}
	add(d, c2, "init", fmt.Sprintf(
		`{"token":%q,"kind":"0","epochLen":"%d","maxCatch":"%d","baseAnnual":%q,`+
			`"blocksPerYear":"%d","dustBucket":%q,"source":%q,"buckets":%q}`,
		tok, p.C2.EpochLen, p.C2.MaxCatch, p.C2.BaseAnnual, p.C2.BlocksPerYear,
		p.C2.DustBucket, p.C2.Source, buckets),
		"sets `genesis` to THIS block — every stake must already be in place (constraint 3)")

	yield := yieldBucket(p.C2.Buckets)
	add(d, c1, "adoptSchedule", fmt.Sprintf(`{"funder":%q,"bucket":%q}`, c2, yield),
		"constraint 4: needs C2's genesis and buckets, so it cannot happen at C1.init")

	stakeContract := ""
	if norm(p.C3.StakeContract) != "" {
		if stakeContract, err = idOf(ids, p.C3.StakeContract); err != nil {
			return nil, fmt.Errorf("c3.stake_contract: %w", err)
		}
	}
	c3init := fmt.Sprintf(
		`{"token":%q,"kind":"0","funder":%q,"treasury":%q,"guardianMode":"%d",`+
			`"guardianAuth":%q,"guardianThreshold":"%d"`,
		tok, c2, p.C3.Treasury, p.C3.Guardian.Mode,
		strings.Join(p.C3.Guardian.Auth, ","), p.C3.Guardian.Threshold)
	if stakeContract != "" {
		c3init += fmt.Sprintf(`,"stakeContract":%q,"stakedBps":"%d"`, stakeContract, p.C3.StakedBps)
	}
	add(d, c3, "init", c3init+"}",
		"constraint 7: fixes the guardian set forever; every reporter below must be disjoint")

	for _, ch := range p.Channels {
		pay := fmt.Sprintf(
			`{"channel":%q,"bucket":%q,"window":"%d","reporterMode":"%d","reporterAuth":%q,`+
				`"reporterThreshold":"%d"`,
			ch.Name, ch.Bucket, ch.Window, ch.Reporter.Mode,
			strings.Join(sortedAuth(ch.Reporter.Auth), ","), ch.Reporter.Threshold)
		if r := strings.TrimSpace(ch.Role); r != "" {
			pay += fmt.Sprintf(`,"role":%q`, r)
		}
		why := "channels are append-only; only setPolicy can change one afterwards"
		if pol := strings.TrimSpace(ch.Policy); pol != "" {
			pay += fmt.Sprintf(`,"policy":%q`, pol)
			if ch.Reporter.Mode == 2 {
				why = "constraint 5: attest mode must declare what all reporters are scoring"
			}
		}
		add(d, c3, "addChannel", pay+"}", why)
	}
	return out, nil
}

// sortedAuth orders an authority list. Hive's required_auths is a flat_set that the
// node re-serialises SORTED; signing an unsorted list yields a digest the node never
// computes and the second authority reads as unsigned. Keeping the CONFIGURED list
// sorted too means what is written on chain matches what a cosigning client builds.
func sortedAuth(in []string) []string {
	out := append([]string(nil), nonEmpty(in)...)
	sort.Strings(out)
	return out
}

func idOf(ids Contracts, name string) (string, error) {
	id, ok := ids[norm(name)]
	if !ok || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("no deployed id for %q", name)
	}
	return id, nil
}

func resolveList(ids Contracts, in []string) ([]string, error) {
	var out []string
	for _, s := range nonEmpty(in) {
		a, err := ids.addr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func bucketSpec(ids Contracts, bs []Bucket) (string, error) {
	var parts []string
	for _, b := range bs {
		target, err := ids.addr(b.Target)
		if err != nil {
			return "", fmt.Errorf("bucket %q: %w", b.Name, err)
		}
		parts = append(parts, fmt.Sprintf("%s:%s:%d", b.Name, target, b.WeightBps))
	}
	return strings.Join(parts, ","), nil
}

// yieldBucket finds the bucket that pays C1. adoptSchedule needs its NAME, and a
// deployment that has none simply never calls adoptSchedule — staking still works,
// there is just no yield to pull.
func yieldBucket(bs []Bucket) string {
	for _, b := range bs {
		if norm(b.Target) == "c1" {
			return b.Name
		}
	}
	return ""
}
