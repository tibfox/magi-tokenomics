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
	if c.Epoch.Len == 0 || c.Source.Tag == "" {
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
		{"zero epoch len", func(c *Config) { c.Epoch.Len = 0 }, "epoch.len"},
		{"no tag", func(c *Config) { c.Source.Tag = "" }, "source.tag"},
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
	if err := c.Validate(); err != nil {
		t.Fatalf("token_stake with a C1 id should be valid: %v", err)
	}
}

// Defaults must land inside the on-chain limits, so a minimal config is safe.
func TestApplyDefaults_StayWithinChainLimits(t *testing.T) {
	minimal := `{
	  "vsc":       { "api": "http://localhost:8080/graphql", "net_id": "vsc-testnet" },
	  "contracts": { "distributor": "vsc1abc" },
	  "epoch":     { "genesis": 10, "len": 100 },
	  "source":    { "tag": "t" }
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
