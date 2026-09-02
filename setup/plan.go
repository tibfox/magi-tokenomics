// Package setup turns a deployment description into an ordered, checked plan.
//
// The seven ordering constraints in the README are all real, and prose did not stop
// the first real testnet deployment from breaking two of them. Both were
// immutable-at-init choices whose consequence only appears several steps later:
// C1's `allow` list decided whether claims could ever pay, and C3's guardian decided
// whether a cosigned channel was possible at all — and each was discovered long
// after the transaction that fixed it.
//
// So the checks here run BEFORE anything is deployed. Contracts are named
// symbolically ("c1", "c3") rather than by id, which is what lets a whole plan be
// validated while it is still free to change: an id only exists after 10 HBD has
// been spent, and by then the mistake is already permanent.
package setup

import (
	"fmt"
	"sort"
	"strings"
)

// Auth is a multi-party authority set: mode 0 Single, 1 Cosigned, 2 Attest.
type Auth struct {
	Mode      int      `json:"mode"`
	Auth      []string `json:"auth"`
	Threshold int      `json:"threshold"`
}

// Bucket is one slice of an epoch's emission. Target is a SYMBOLIC contract name
// ("c1", "c3") or a literal ledger address ("hive:treasury").
type Bucket struct {
	Name      string `json:"name"`
	Target    string `json:"target"`
	WeightBps int    `json:"weight_bps"`
}

// Channel is one reward stream on the distributor.
type Channel struct {
	Name     string `json:"name"`
	Bucket   string `json:"bucket"`
	Window   int    `json:"window"`
	Role     string `json:"role"`
	Reporter Auth   `json:"reporter"`
	Policy   string `json:"policy"`
}

// Plan is everything a deployment needs, in one file.
type Plan struct {
	Deployer string `json:"deployer"`

	Token struct {
		Name      string `json:"name"`
		Symbol    string `json:"symbol"`
		Decimals  int    `json:"decimals"`
		MaxSupply string `json:"max_supply"`
		Mint      string `json:"mint"`
	} `json:"token"`

	C1 struct {
		Cooldown   int      `json:"cooldown"`
		EpochLen   int      `json:"epoch_len"`
		Allow      []string `json:"allow"`
		Treasury   string   `json:"treasury"`
		Guardian   Auth     `json:"guardian"`
		MaxAirdrop string   `json:"max_airdrop"`
	} `json:"c1"`

	C2 struct {
		EpochLen      int      `json:"epoch_len"`
		BaseAnnual    string   `json:"base_annual"`
		BlocksPerYear int      `json:"blocks_per_year"`
		MaxCatch      int      `json:"max_catch"`
		DustBucket    string   `json:"dust_bucket"`
		Source        string   `json:"source"`
		Buckets       []Bucket `json:"buckets"`
	} `json:"c2"`

	C3 struct {
		Treasury     string `json:"treasury"`
		Guardian     Auth   `json:"guardian"`
		StakedBps    int    `json:"staked_bps"`
		StakeContract string `json:"stake_contract"` // symbolic, usually "c1"; empty = no staked payouts
	} `json:"c3"`

	Channels []Channel `json:"channels"`
}

// Problem is one refusal, tied to the constraint it comes from so the message can
// point at the paragraph that explains it rather than restating it badly.
type Problem struct {
	Constraint int    // README constraint number, 0 for checks that are not numbered
	Field      string
	Detail     string
}

func (p Problem) String() string {
	where := p.Field
	if p.Constraint > 0 {
		where = fmt.Sprintf("%s (constraint %d)", p.Field, p.Constraint)
	}
	return fmt.Sprintf("%s: %s", where, p.Detail)
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// overlap returns the members two authority sets share, lowercased and sorted so a
// message is stable regardless of the order they were written in.
func overlap(a, b []string) []string {
	inA := map[string]bool{}
	for _, x := range a {
		if x = norm(x); x != "" {
			inA[x] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, y := range b {
		if y = norm(y); y != "" && inA[y] && !seen[y] {
			out = append(out, y)
			seen[y] = true
		}
	}
	sort.Strings(out)
	return out
}

// Template is a plan that passes Check, so an operator starts from something that
// works and edits it, rather than assembling one field at a time and discovering the
// ordering rules by hitting them.
func Template() *Plan {
	p := &Plan{Deployer: "hive:youraccount"}
	p.Token.Name, p.Token.Symbol, p.Token.Decimals = "Your Token", "YOURS", 0
	p.Token.MaxSupply, p.Token.Mint = "100000000", "1000000"
	p.C1.Cooldown, p.C1.EpochLen = 201600, 28800 // 7 days cooldown, daily epochs
	p.C1.Treasury = "hive:yourtreasury"
	p.C1.Guardian = Auth{Mode: 0, Auth: []string{"hive:yourguardian"}, Threshold: 1}
	p.C1.MaxAirdrop = "100000" // 10% of the mint; the rest funds emission
	p.C1.Allow = []string{"c3"}
	p.C2.EpochLen, p.C2.BaseAnnual, p.C2.BlocksPerYear = 28800, "3650000", 10512000
	p.C2.MaxCatch, p.C2.DustBucket, p.C2.Source = 5, "content", "hive:yourpool"
	p.C2.Buckets = []Bucket{
		{Name: "content", Target: "c3", WeightBps: 6000},
		{Name: "yield", Target: "c1", WeightBps: 4000},
	}
	p.C3.Treasury = "hive:yourtreasury"
	p.C3.Guardian = Auth{Mode: 0, Auth: []string{"hive:yourguardian"}, Threshold: 1}
	p.C3.StakedBps, p.C3.StakeContract = 5000, "c1"
	p.Channels = []Channel{{
		Name: "content", Bucket: "content", Window: 28800, Role: "content",
		Reporter: Auth{Mode: 0, Auth: []string{"hive:yourreporter"}, Threshold: 1},
	}}
	return p
}
