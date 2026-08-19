package main

import (
	"strings"
	"testing"
)

// `shares.muted` and `source.exclude` are matched against LEDGER-DOMAIN accounts, so
// a bare Hive name in either list silently does nothing.
//
// Both lists are exact string sets, and both are probed with a name the reporter has
// already prefixed:
//
//	sharecore/compute.go  muted[who]                 who == "hive:"+author
//	hivesrc/hivesrc.go    excl["hive:"+p.Author]     and excl["hive:"+v.Voter]
//
// Nothing normalises them on the way in — config.go's ShareConfig passes
// c.Shares.Muted straight through, and main.go passes c.Source.Exclude straight into
// ExcludeAccounts. So `"muted": ["spammer"]` — the spelling every operator will reach
// for, since it is how the account appears everywhere on Hive — matches no account
// that can ever be scored. The spammer keeps earning, the operator believes they are
// muted, and nothing anywhere reports it. It is the quietest possible failure: the
// run succeeds, the book is well-formed, the epoch finalizes.
//
// Refused rather than auto-prefixed, following shares.app_tax.beneficiary, which
// already demands the domain and says so. Rewriting an operator's account list on
// their behalf would mean the accounts they think they excluded are not the accounts
// the reporter used, and under Attest that divergence is invisible until two
// reporters disagree about a root.
//
// The tests below pin the CONSUMERS as well as the validator: if a future change made
// either list domain-insensitive, the validator alone would be enforcing a rule that
// no longer existed.

func cfgWithMuted(entry string) string {
	return strings.Replace(ExampleConfig, `"muted":             []`,
		`"muted":             ["`+entry+`"]`, 1)
}

func cfgWithExclude(entry string) string {
	return strings.Replace(ExampleConfig, `"exclude":                []`,
		`"exclude":                ["`+entry+`"]`, 1)
}

// THE FINDING, half one: a bare name in shares.muted.
func TestLedgerDomain_BareMutedNameIsRefused(t *testing.T) {
	body := cfgWithMuted("spammer")
	if body == ExampleConfig {
		t.Fatal("fixture did not substitute — the example config's muted key changed shape")
	}
	_, err := LoadConfig(writeCfg(t, body))
	if err == nil {
		t.Fatal("`\"muted\": [\"spammer\"]` must be refused: it is compared against " +
			"\"hive:spammer\" and so mutes nobody, and the operator has no way to tell — " +
			"the run succeeds and the account they meant to mute keeps earning")
	}
	if !strings.Contains(err.Error(), "shares.muted") || !strings.Contains(err.Error(), "hive:") {
		t.Fatalf("the error must name the field and the fix, got: %v", err)
	}
}

// THE FINDING, half two: a bare name in source.exclude.
func TestLedgerDomain_BareExcludeNameIsRefused(t *testing.T) {
	body := cfgWithExclude("botfarm")
	if body == ExampleConfig {
		t.Fatal("fixture did not substitute — the example config's exclude key changed shape")
	}
	_, err := LoadConfig(writeCfg(t, body))
	if err == nil {
		t.Fatal("`\"exclude\": [\"botfarm\"]` must be refused: it is compared against " +
			"\"hive:botfarm\", so the account is neither dropped as an author nor " +
			"stripped of its vote weight")
	}
	if !strings.Contains(err.Error(), "source.exclude") || !strings.Contains(err.Error(), "hive:") {
		t.Fatalf("the error must name the field and the fix, got: %v", err)
	}
}

// The `@spammer` spelling is the other natural guess and is just as inert.
func TestLedgerDomain_AtPrefixedNameIsRefused(t *testing.T) {
	if _, err := LoadConfig(writeCfg(t, cfgWithMuted("@spammer"))); err == nil {
		t.Fatal("`@spammer` carries no ledger domain either and must be refused")
	}
}

// A correctly-spelled entry must still load, or the guard would be unusable.
func TestLedgerDomain_PrefixedNamesLoad(t *testing.T) {
	if _, err := LoadConfig(writeCfg(t, cfgWithMuted("hive:spammer"))); err != nil {
		t.Fatalf("hive:spammer is the documented form and must load: %v", err)
	}
	if _, err := LoadConfig(writeCfg(t, cfgWithExclude("hive:botfarm"))); err != nil {
		t.Fatalf("hive:botfarm is the documented form and must load: %v", err)
	}
}

// An empty entry mutes nothing and is almost certainly a stray comma.
func TestLedgerDomain_EmptyEntryIsRefused(t *testing.T) {
	if _, err := LoadConfig(writeCfg(t, cfgWithMuted(""))); err == nil {
		t.Fatal("an empty entry in shares.muted must be refused rather than ignored")
	}
}

// The consumers this validator exists for are pinned in their OWN packages, where
// the fixtures live: TestMutedNameMustCarryTheLedgerDomain in sharecore and
// TestMapPost_BareExclusionDoesNotMatch in hivesrc. Without those, the validator
// would be free to go on enforcing a rule the compute path had quietly dropped.
