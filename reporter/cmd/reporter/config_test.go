package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The shipped example must actually load — a broken `init-config` output would
// send every new operator straight into a parse error.
func TestExampleConfig_Loads(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatalf("init-config output does not load: %v", err)
	}
	if c.Epoch.Len == 0 || len(c.Source.Tags) == 0 {
		t.Fatalf("example config is missing required values: %+v", c)
	}
}

// A typo'd key must not silently take a default: `"finalise": true` quietly
// meaning "do not finalize" would leave epochs frozen open forever.
func TestLoadConfig_RejectsUnknownFields(t *testing.T) {
	body := strings.Replace(ExampleConfig, `"finalize"`, `"finalise"`, 1)
	if body == ExampleConfig {
		t.Fatal("test fixture did not substitute — example config changed shape")
	}
	_, err := LoadConfig(writeCfg(t, body))
	if err == nil || !strings.Contains(err.Error(), "finalise") {
		t.Fatalf("unknown field must be rejected by name, got %v", err)
	}
}

func TestParseCurve(t *testing.T) {
	for _, tc := range []struct {
		in      string
		n, d    int
		wantErr bool
	}{
		{in: "1/1", n: 1, d: 1},
		{in: "1/2", n: 1, d: 2}, // sqrt: early curators win
		{in: "2/1", n: 2, d: 1}, // convex: late curators win
		{in: "3", n: 3, d: 1},   // bare integer means n/1
		{in: " 3 / 2 ", n: 3, d: 2},
		{in: "0/1", wantErr: true},  // exponent 0 would flatten every post to 1
		{in: "1/0", wantErr: true},  // division by zero
		{in: "-1/2", wantErr: true}, // negative exponent is not integer math
		{in: "a/b", wantErr: true},
		{in: "", wantErr: true},
	} {
		n, d, err := parseCurve(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q: expected an error, got %d/%d", tc.in, n, d)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.in, err)
		}
		if n != tc.n || d != tc.d {
			t.Fatalf("%q: got %d/%d want %d/%d", tc.in, n, d, tc.n, tc.d)
		}
	}
}

func TestShareConfig_TranslatesCurves(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	sc := c.ShareConfig()
	if sc.CurationCurveNum != 1 || sc.CurationCurveDen != 2 {
		t.Fatalf("curation curve not translated: %d/%d", sc.CurationCurveNum, sc.CurationCurveDen)
	}
	if sc.AuthorRewardBps != 5000 {
		t.Fatalf("author bps: %d", sc.AuthorRewardBps)
	}
}

func TestValidate_RequiredAndBoundedFields(t *testing.T) {
	base := func() *Config {
		c, err := LoadConfig(writeCfg(t, ExampleConfig))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no vsc api", func(c *Config) { c.VSC.API = "" }, "vsc.api"},
		{"no net id", func(c *Config) { c.VSC.NetID = "" }, "net_id"},
		{"no distributor", func(c *Config) { c.Contracts.Distributor = "" }, "distributor"},
		{"no channel", func(c *Config) { c.Contracts.Channel = "" }, "channel"},
		{"zero epoch len", func(c *Config) { c.Epoch.Len = 0 }, "epoch.len"},
		{"no tag", func(c *Config) { c.Source.Tags = nil }, "source.tag"},
		{"bad weight mode", func(c *Config) { c.Source.Weight = "vibes" }, "source.weight"},
		{"token_stake without C1", func(c *Config) {
			c.Source.Weight = "token_stake"
			c.Contracts.Stake = ""
		}, "contracts.stake"},
		{"bps too high", func(c *Config) { c.Shares.AuthorRewardBps = 10001 }, "author_reward_bps"},
		{"bad author curve", func(c *Config) { c.Shares.AuthorCurve = "x" }, "author_curve"},
		{"bad curation curve", func(c *Config) { c.Shares.CurationCurve = "1/0" }, "curation_curve"},
		// 4096 is the auth module's hard payload cap; a larger page would abort
		// on chain after the RC was already spent.
		{"page over the payload cap", func(c *Config) { c.Page.MaxBytes = 5000 }, "max_bytes"},
		{"zero rc limit", func(c *Config) { c.Submit.RcLimit = -1 }, "rc_limit"},
		{"keeper without funder", func(c *Config) {
			c.Submit.Keeper = true
			c.Contracts.Funder = ""
		}, "contracts.funder"},
	} {
		c := base()
		tc.mutate(c)
		err := c.Validate()
		if err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error should mention %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestValidate_TokenStakeWithC1IsAccepted(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	c.Source.Weight = "token_stake"
	c.Contracts.Stake = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	// token_stake REQUIRES a vote budget: a staked balance is not consumed by being
	// used, so without one the same stake votes every post at full weight.
	c.Source.VoteRegenDays = 5
	c.Source.VotePowerConsumption = 200
	c.Source.DownvoteRegenDays = 30
	c.Source.DownvotePowerConsumption = 200
	if err := c.Validate(); err != nil {
		t.Fatalf("token_stake with a C1 id should be valid: %v", err)
	}
}

// Defaults must land inside the on-chain limits, so a minimal config is safe.
func TestApplyDefaults_StayWithinChainLimits(t *testing.T) {
	minimal := `{
	  "vsc":       { "api": "http://localhost:8080/graphql", "net_id": "vsc-testnet" },
	  "contracts": { "distributor": "vsc1abc", "channel": "content" },
	  "epoch":     { "genesis": 10, "len": 100 },
	  "source":    { "tags": ["t"] }
	}`
	c, err := LoadConfig(writeCfg(t, minimal))
	if err != nil {
		t.Fatalf("a minimal config should load: %v", err)
	}
	if c.Page.MaxBytes > 4096 {
		t.Fatalf("default page.max_bytes %d exceeds the auth payload cap", c.Page.MaxBytes)
	}
	if c.Page.MaxEntries <= 0 || c.Submit.RcLimit <= 0 {
		t.Fatalf("defaults not applied: %+v", c.Page)
	}
	if c.Submit.WifEnv == "" {
		t.Fatal("wif_env must default to a name, else the key can never be found")
	}
	if c.Source.Weight != "hive_rshares" {
		t.Fatalf("default weight mode: %q", c.Source.Weight)
	}
	if len(c.Hive.API) == 0 {
		t.Fatal("a hive endpoint must default")
	}
}

func TestLoadConfig_MissingFileIsAnError(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing config file must error")
	}
}

// LP mode reads the indexer, not Hive. It must validate WITHOUT the content-only
// settings, or every LP operator has to carry a meaningless tag and curve config.
func TestValidate_LPModeNeedsIndexerNotContentSettings(t *testing.T) {
	lp := func() *Config {
		c, err := LoadConfig(writeCfg(t, ExampleConfig))
		if err != nil {
			t.Fatal(err)
		}
		c.Source.Kind = SourceLP
		c.Indexer.API = "http://indexer:8081/v1/graphql"
		c.Indexer.Pool = "vsc1pool"
		// deliberately strip everything content-specific
		c.Source.Tags = nil
		c.Source.Weight = ""
		c.Shares.AuthorCurve = ""
		c.Shares.CurationCurve = ""
		return c
	}
	if err := lp().Validate(); err != nil {
		t.Fatalf("an LP config with no content settings must validate, got: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no indexer api", func(c *Config) { c.Indexer.API = "" }, "indexer.api"},
		{"no pool", func(c *Config) { c.Indexer.Pool = "" }, "indexer.pool"},
		{"negative page size", func(c *Config) { c.Indexer.PageSize = -1 }, "page_size"},
	} {
		c := lp()
		tc.mutate(c)
		err := c.Validate()
		if err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error should mention %q, got %v", tc.name, tc.want, err)
		}
	}
}

// Splitting validation by kind must not let a typo through as "content".
func TestValidate_UnknownSourceKindIsRejected(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	c.Source.Kind = "liquidity" // close, but not the accepted value
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "source.kind") {
		t.Fatalf("an unknown kind must be rejected, got: %v", err)
	}
}

// Kind defaults to content, so every existing config keeps its meaning.
func TestKind_DefaultsToContentAndStillValidatesTag(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.Kind != "" {
		t.Fatalf("the example config should not set a kind, got %q", c.Source.Kind)
	}
	if c.Kind() != SourceContent {
		t.Fatalf("Kind() = %q, want %q", c.Kind(), SourceContent)
	}
	// and the content-only rules must still bite in the default mode
	c.Source.Tags = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "source.tag") {
		t.Fatalf("content mode must still require source.tag, got: %v", err)
	}
}

// The LP example must load AND validate, or `init-config lp` hands operators a file
// that fails on first use.
func TestExampleLPConfig_LoadsAndValidates(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleLPConfig))
	if err != nil {
		t.Fatalf("LP example config failed to load: %v", err)
	}
	if c.Kind() != SourceLP {
		t.Fatalf("Kind() = %q, want %q", c.Kind(), SourceLP)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("LP example config failed to validate: %v", err)
	}
	if c.Indexer.Pool == "" || c.Indexer.API == "" {
		t.Fatal("the LP example must carry an indexer api and pool")
	}
	// The LP example must not smuggle in content policy it cannot honour.
	if len(c.Source.Tags) != 0 {
		t.Fatalf("LP example should not set source.tag, got %v", c.Source.Tags)
	}
	// pull_funding has no default — it is a plain bool, so omitting it yields false
	// and the distributor never gets funded. The epoch then reports shares against
	// zero funding and pays nobody, silently. Both examples must set it.
	if !c.Submit.PullFunding {
		t.Fatal("LP example must set submit.pull_funding, or C5 is never funded")
	}
	if !c.Submit.Finalize {
		t.Fatal("LP example must set submit.finalize, or the epoch never closes")
	}
}

// The same trap in the content example.
func TestExampleConfig_PullsFunding(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Submit.PullFunding {
		t.Fatal("content example must set submit.pull_funding")
	}
}

// rc_limit and page.max_entries validate perfectly in isolation and can be incoherent
// together — the worst shape a config can have. Pagination only emits a short page at
// the very end, so a full page that exceeds rc_limit means EVERY page but the last
// reverts, every time, while the cheap calls (poke, pull, finalize) all succeed.
func TestValidate_PageMustFitTheRcLimit(t *testing.T) {
	base := func() *Config {
		c, err := LoadConfig(writeCfg(t, ExampleConfig))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// 60 entries needs ~7,140 RC with headroom; 2,000 cannot carry it
	c := base()
	c.Page.MaxEntries = 60
	c.Submit.RcLimit = 2000
	err := c.Validate()
	if err == nil {
		t.Fatal("a page that cannot fit the rc limit must be rejected at config load")
	}
	for _, want := range []string{"cannot fit a full page", "60 entries", "rc_limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}

	// the shipped defaults must be coherent — otherwise every operator hits this
	if err := base().Validate(); err != nil {
		t.Fatalf("the example config must be internally coherent: %v", err)
	}

	// and lowering max_entries is a valid fix, not just raising rc_limit
	c = base()
	c.Page.MaxEntries = 5
	c.Submit.RcLimit = 2000
	if err := c.Validate(); err != nil {
		t.Fatalf("a small page inside a small rc limit must be accepted: %v", err)
	}
}

// A post is ALWAYS scored after its voting period ends. There is no setting for
// scoring it earlier, and a config carrying one must fail loudly rather than be
// ignored: a silently-dropped `attribution` key would leave an operator believing
// they had configured something that does not exist.
func TestConfig_AttributionIsNotConfigurable(t *testing.T) {
	raw := `{
	  "vsc":       {"api":"http://x","net_id":"vsc-testnet"},
	  "contracts": {"distributor":"vsc1c3","channel":"content"},
	  "epoch":     {"len":100},
	  "source":    {"tags":["t"],"attribution":"created"},
	  "submit":    {"rc_limit":60000}
	}`
	f := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(f, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(f)
	if err == nil {
		t.Fatal("a config setting `attribution` must be rejected — the option does not exist")
	}
	if !strings.Contains(err.Error(), "attribution") {
		t.Fatalf("the error must name the offending key so it is fixable, got: %v", err)
	}
}

// The rc_limit/max_entries coherence check must cover EVERY page size, not just the
// default one. It is sized from measurement (docs/rc-costs.md), and the measurement
// moved when share keys became channel-scoped: the fixed base went from ~200 to ~465,
// so a base left at 200 demanded 354 RC for a one-entry page that really costs ~697.
// A config could then pass load and revert every single page — precisely the failure
// this check exists to catch.
//
// Pinned against the measured costs so a future change to the key layout that raises
// the base has to update the constant rather than silently under-covering small pages.
func TestConfig_RcLimitCheckCoversEveryPageSize(t *testing.T) {
	measured := map[int]int{1: 697, 10: 1376, 30: 3310, 60: 5942}
	for entries, cost := range measured {
		// the largest rc_limit the validator still rejects
		lo := 0
		for n := 1; n <= 20000; n++ {
			c := baseValidCfg(t)
			c.Page.MaxEntries = entries
			c.Submit.RcLimit = n
			if c.Validate() == nil {
				lo = n
				break
			}
		}
		if lo == 0 {
			t.Fatalf("%d entries: no rc_limit was ever accepted", entries)
		}
		if lo < cost {
			t.Fatalf("%d entries: the smallest ACCEPTED rc_limit is %d, but a page really "+
				"costs %d — the check under-covers this page size, so a config would load "+
				"and then revert every page", entries, lo, cost)
		}
	}
}

// baseValidCfg is a config that validates, for tests that then break one field.
func baseValidCfg(t *testing.T) *Config {
	t.Helper()
	c, err := LoadConfig(writeCfg(t, ExampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
