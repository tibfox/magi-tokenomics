package main

import (
	"reflect"
	"sort"
	"testing"
)

// The sections of Config that do NOT go into the policy digest, with the reason.
//
// Everything else must be covered by policySections(). This is the drift guard: a
// new top-level section is a compile-time-invisible way to add a setting that
// changes the book while escaping the digest, and the whole guarantee rests on
// nothing escaping it. Adding a section here is a deliberate statement that it
// cannot change what an epoch pays.
var notPolicy = map[string]string{
	"Hive":      "API endpoints; two honest mirrors serve the same chain data",
	"VSC":       "API endpoints; same reason",
	"Indexer":   "endpoint. NOT as safe — see the policy package doc: two indexer instances can assign an event different heights, so LP quorums must share one. A URL comparison would reject honest mirrors without fixing that",
	"Contracts": "which contracts to talk to; verifyChainConfig already compares these against the chain directly, and a wrong one is caught there",
	"Submit":    "per-operator: own account, own WIF, own progress file, own RC limit. Cannot change what the epoch pays",
	"Epoch":     "represented in the digest by EpochWindow (genesis + len). The remaining field, Lookback, is operational — see notPolicyField",
}

// notPolicyField excuses individual fields inside an otherwise-hashed section.
var notPolicyField = map[string]string{
	"Epoch.Lookback": "how far back the DEFAULT epoch search looks. It feeds windowFloor " +
		"and nothing else, so it cannot move a share. Hashing it refused a reporter for " +
		"following the recovery advice the tool itself prints, and setPolicy is owner-only",
}

// Every top-level section is either in the digest or explicitly excused.
func TestPolicy_EverySectionIsClassified(t *testing.T) {
	var cfg Config
	inDigest := map[string]bool{}
	for _, s := range cfg.policySections() {
		inDigest[reflect.TypeOf(s).Name()] = true
	}
	// The sections are anonymous structs, so match by the FIELD name on Config
	// rather than the type name.
	covered := map[string]bool{}
	cv := reflect.ValueOf(&cfg).Elem()
	for _, s := range cfg.policySections() {
		st := reflect.TypeOf(s)
		for i := 0; i < cv.NumField(); i++ {
			if cv.Type().Field(i).Type == st {
				covered[cv.Type().Field(i).Name] = true
			}
		}
	}

	var unclassified []string
	for i := 0; i < cv.NumField(); i++ {
		name := cv.Type().Field(i).Name
		if covered[name] {
			if why, excused := notPolicy[name]; excused {
				t.Errorf("%s is BOTH in the digest and listed as not-policy (%q) — decide which", name, why)
			}
			continue
		}
		if _, excused := notPolicy[name]; excused {
			continue
		}
		unclassified = append(unclassified, name)
	}
	sort.Strings(unclassified)
	for _, name := range unclassified {
		t.Errorf("config section %q is neither in policySections() nor listed in notPolicy: "+
			"if it can change what an epoch pays it MUST be in the digest, and if it cannot, "+
			"say so in notPolicy. Leaving it unclassified is how the guarantee rots", name)
	}

	if len(covered) == 0 {
		t.Fatal("policySections() covered nothing — the test is not observing what it thinks")
	}
	t.Logf("%d sections in the digest, %d excused", len(covered), len(notPolicy))
}

// Every field of every covered section must move the real digest — the same
// property the policy package proves on a sample, asserted against the actual
// Config so a new setting cannot slip in unhashed.
func TestPolicy_EveryRealFieldChangesTheDigest(t *testing.T) {
	base := loadedSample()
	want, err := base.PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}

	var paths [][]int
	var collect func(prefix []int, rt reflect.Type)
	collect = func(prefix []int, rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue
			}
			p := append(append([]int{}, prefix...), i)
			if f.Type.Kind() == reflect.Struct {
				collect(p, f.Type)
				continue
			}
			paths = append(paths, p)
		}
	}
	cv := reflect.ValueOf(base).Elem()
	for i := 0; i < cv.NumField(); i++ {
		name := cv.Type().Field(i).Name
		if _, excused := notPolicy[name]; excused {
			continue
		}
		collect([]int{i}, cv.Type().Field(i).Type)
	}
	if len(paths) == 0 {
		t.Fatal("no policy fields found — the walk proves nothing")
	}

	for _, p := range paths {
		cp := *base
		v := reflect.ValueOf(&cp).Elem()
		name := ""
		for _, i := range p {
			name += "." + v.Type().Field(i).Name
			v = v.Field(i)
		}
		mutateField(t, v)
		got, err := cp.PolicyDigest()
		if err != nil {
			t.Fatalf("%s: PolicyDigest: %v", name[1:], err)
		}
		if _, excused := notPolicyField[name[1:]]; excused {
			continue
		}
		if got == want {
			t.Errorf("%s does not affect the policy digest: two reporters differing only in "+
				"this setting would both pass the chain check and then score the epoch "+
				"differently", name[1:])
		}
	}
	t.Logf("%d policy fields exercised, every one moves the digest", len(paths))
}

// Reordering the lists whose order cannot change the book must not change the
// real digest either — a spurious refusal gets the check switched off.
func TestPolicy_TagOrderDoesNotChangeTheRealDigest(t *testing.T) {
	a := loadedSample()
	a.Source.Tags = []string{"magi", "hive", "vsc"}
	a.Shares.Muted = []string{"hive:spammer", "hive:bot"}
	b := loadedSample()
	b.Source.Tags = []string{"vsc", "magi", "hive"}
	b.Shares.Muted = []string{"hive:bot", "hive:spammer"}

	da, err := a.PolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.PolicyDigest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatal("listing the same tags in a different order changed the digest — honest " +
			"reporters would be refused for a cosmetic difference")
	}
}

func loadedSample() *Config {
	c := &Config{}
	c.Epoch.Genesis = 1000
	c.Epoch.Len = 1200
	c.Epoch.Lookback = 20
	c.Source.Tags = []string{"magi"}
	c.Source.ExcludedTags = []string{"nsfw"}
	c.Source.Exclude = []string{"hive:excluded"}
	c.Source.Limit = 1000
	c.Source.Weight = "hive_rshares"
	c.Source.Kind = "content"
	c.Source.CashoutDays = 7
	c.Shares.AuthorRewardBps = 5000
	c.Shares.AuthorCurve = "1/1"
	c.Shares.CurationCurve = "1/2"
	c.Shares.Muted = []string{"hive:spammer"}
	c.Shares.MinShareBps = 5
	c.Shares.StakedBps = 5000
	c.Shares.AppTax.Bps = 500
	c.Shares.AppTax.Apps = []string{"peakd"}
	c.Shares.AppTax.Beneficiary = "hive:treasury"
	c.Page.MaxEntries = 60
	c.Page.MaxBytes = 4096
	return c
}

func mutateField(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Slice:
		grown := reflect.MakeSlice(v.Type(), 0, v.Len()+1)
		grown = reflect.AppendSlice(grown, v)
		grown = reflect.Append(grown, reflect.ValueOf("added-by-the-test"))
		v.Set(grown)
	default:
		t.Fatalf("cannot mutate a %s — extend mutateField rather than skipping it, or a "+
			"field of this kind silently escapes the guarantee", v.Kind())
	}
}
