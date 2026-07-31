package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"magi_token/reporter/hivesrc"
	"magi_token/reporter/sharecore"
)

// Config is the reporter's whole configuration.
//
// Deliberately absent: any private key. The active WIF is read from the env var
// named by Submit.WifEnv, so a config file can be committed, backed up and shared
// between the machines of an Attest quorum without leaking signing authority.
type Config struct {
	Hive struct {
		// API endpoints. The first is used for reads; all are given to the
		// broadcaster so a single node being down does not stall an epoch.
		API []string `json:"api"`
	} `json:"hive"`

	VSC struct {
		API   string `json:"api"`    // node GraphQL endpoint, for contract state reads
		NetID string `json:"net_id"` // "vsc-mainnet" / "vsc-testnet" — must match the chain
	} `json:"vsc"`

	// Indexer is the LP data source, required only when source.kind = "lp".
	//
	// LP shares cannot come from the DEX itself: it stores balances as current state
	// with no height checkpoints, so a past epoch cannot be priced and paying against
	// live balances would be flash-liquidity gameable. The indexer's add_liq/rem_liq
	// event log is replayed instead — see the lpsrc package doc.
	//
	// Configurable per operator so an operator is not forced onto someone else's
	// indexer, and so the aggregation stays in the reporter: nothing here may depend
	// on an indexer-side view, because a view that differs between deployments turns
	// an Attest quorum into a silent byte-mismatch.
	//
	// CAVEAT — reporters sharing an Attest quorum should point at the SAME indexer.
	// An earlier version of this comment claimed different indexers were safe because
	// the reporter pins its arithmetic to explicit heights. That is wrong: the heights
	// are not the reporter's, they come from the indexer. Its ingestion falls back
	// between a transaction's L1 anchored height and the state-output height when the
	// transaction_pool lookup misses (magi-mongo-indexer fetcher/mongo.go), and that
	// height is part of the per-event dedupe key, so two instances can legitimately
	// assign different heights to the same event and land it in different epochs.
	// Same indexer, same answer; different indexers, no guarantee.
	Indexer struct {
		API      string `json:"api"`       // Hasura GraphQL endpoint, e.g. http://host:8081/v1/graphql
		Secret   string `json:"secret"`    // x-hasura-admin-secret; empty when public
		Pool     string `json:"pool"`      // pool contract id, as indexer_contract_id
		PageSize int    `json:"page_size"` // rows per GraphQL page; 0 = default
		// AllowStale disables the freshness gate. Leave it FALSE unless you accept
		// the consequence: a lagging indexer returns fewer rows rather than an error,
		// so scoring an epoch it has not reached underpays providers irreversibly.
		//
		// The gate can only prove freshness, never staleness — indexer_health reports
		// the last log WRITTEN, not the position SCANNED — so on a quiet chain it
		// refuses epochs whose data is in fact complete. This is the knob for that
		// case, and it is opt-in deliberately: defaulting it on would trade a loud
		// refusal for a silent underpayment.
		AllowStale bool `json:"allow_stale"`
	} `json:"indexer"`

	Contracts struct {
		Distributor string `json:"distributor"` // C3 (content) or C5 (LP)
		Funder      string `json:"funder"`      // C2; empty = a separate keeper pokes it
		Stake       string `json:"stake"`       // C1; required only for token_stake weighting
	} `json:"contracts"`

	Epoch struct {
		// Genesis and Len MUST equal the distributor's cfg_genesis / cfg_epochLen.
		// `reporter epoch` verifies this against on-chain state before anything is
		// submitted, because a mismatch would report on the wrong block range and
		// then finalize it — unrecoverable.
		Genesis uint64 `json:"genesis"`
		Len     uint64 `json:"len"`
		// Lookback is how many closed epochs back the reporter will look for the
		// oldest unfinalized one. 0 uses the default (20).
		//
		// Raise it after extended downtime. Because a run handles ONE epoch, a
		// backlog longer than this window can never be worked off: the oldest
		// outstanding epoch drops below the window and is never selected again.
		Lookback uint64 `json:"lookback"`
	} `json:"epoch"`

	Source struct {
		Tag   string `json:"tag"`
		Limit int    `json:"limit"`
		// Attribution: "cashout" (default) scores a post in the epoch its Hive
		// payout falls in, so every vote is counted exactly once; "created" scores
		// it in the epoch it was posted in, which is prompter but loses every vote
		// cast after the snapshot. See hivesrc.Attribution.
		Attribution string   `json:"attribution"`
		Weight      string   `json:"weight"` // hive_rshares | token_stake
		Exclude     []string `json:"exclude"`
		// Kind selects the data source: "content" (default) reads Hive posts/votes
		// for C3; "lp" replays liquidity events from the indexer for C5. The rest of
		// the pipeline — canonicalisation, pagination, submission, Attest — is shared.
		Kind string `json:"kind"`
	} `json:"source"`

	Shares struct {
		AuthorRewardBps int      `json:"author_reward_bps"`
		AuthorCurve     string   `json:"author_curve"`   // "num/den", e.g. "1/1"
		CurationCurve   string   `json:"curation_curve"` // "1/2" = sqrt = early voters win
		Muted           []string `json:"muted"`
	} `json:"shares"`

	Page struct {
		MaxEntries int `json:"max_entries"`
		MaxBytes   int `json:"max_bytes"`
	} `json:"page"`

	Submit struct {
		Account      string `json:"account"` // reporter Hive account (with or without hive:)
		WifEnv       string `json:"wif_env"`
		RcLimit      int    `json:"rc_limit"`
		ProgressFile string `json:"progress_file"`
		Keeper       bool   `json:"keeper"`       // also poke C2.distributeEpoch
		PullFunding  bool   `json:"pull_funding"` // also call C3.pullFunding
		Finalize     bool   `json:"finalize"`     // also call C3.finalizeEpoch
		// Before sending the irreversible finalizeEpoch, the reporter re-reads the
		// chain and refuses unless every share page is confirmed APPLIED. These bound
		// that wait. Broadcasting is not executing: a page can be accepted by Hive and
		// still revert on L2, and finalizing over the gap pays the whole epoch to
		// whoever happened to land.
		ConfirmTries       int `json:"confirm_tries"`
		ConfirmIntervalSec int `json:"confirm_interval_sec"`
	} `json:"submit"`
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // a typo'd key must not silently take a default
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if len(c.Hive.API) == 0 {
		c.Hive.API = []string{"https://api.hive.blog"}
	}
	if c.Source.Weight == "" {
		c.Source.Weight = string(hivesrc.WeightHiveRshares)
	}
	if c.Source.Attribution == "" {
		c.Source.Attribution = string(hivesrc.AttributeCashout)
	}
	if c.Source.Limit == 0 {
		c.Source.Limit = 1000
	}
	if c.Shares.AuthorCurve == "" {
		c.Shares.AuthorCurve = "1/1"
	}
	if c.Shares.CurationCurve == "" {
		c.Shares.CurationCurve = "1/1"
	}
	// Page limits default to what the contract and RC budget actually allow:
	// submitShares payloads are capped at 4096 bytes by the auth module and each
	// entry costs roughly 80-95 RC to apply (docs/rc-costs.md).
	if c.Page.MaxBytes == 0 {
		c.Page.MaxBytes = 3800
	}
	if c.Page.MaxEntries == 0 {
		c.Page.MaxEntries = 60
	}
	if c.Submit.RcLimit == 0 {
		c.Submit.RcLimit = 60000
	}
	if c.Submit.WifEnv == "" {
		c.Submit.WifEnv = "REPORTER_ACTIVE_WIF"
	}
	if c.Submit.ProgressFile == "" {
		c.Submit.ProgressFile = "reporter-progress.json"
	}
}

func (c *Config) Validate() error {
	if c.VSC.API == "" {
		return fmt.Errorf("vsc.api is required")
	}
	if c.VSC.NetID == "" {
		return fmt.Errorf("vsc.net_id is required (vsc-mainnet or vsc-testnet)")
	}
	if c.Contracts.Distributor == "" {
		return fmt.Errorf("contracts.distributor is required")
	}
	if c.Epoch.Len == 0 {
		return fmt.Errorf("epoch.len is required and must be > 0")
	}
	if err := c.validateSource(); err != nil {
		return err
	}
	if c.Page.MaxBytes > 4096 {
		return fmt.Errorf("page.max_bytes must be <= 4096 (the auth module's payload cap), got %d", c.Page.MaxBytes)
	}
	if c.Submit.RcLimit <= 0 {
		return fmt.Errorf("submit.rc_limit must be > 0")
	}
	// A FULL page must fit inside the RC limit, or it fails DETERMINISTICALLY.
	//
	// These two validate perfectly in isolation and are incoherent together, which is
	// the worst shape a config can have. Pagination only emits a short page at the
	// very end, so if a full page exceeds rc_limit then EVERY page except the last
	// reverts, every time — while the cheap calls (poke, pull, finalize) all succeed.
	// Before the finalize gate that produced a finalized epoch paying everything to
	// whoever happened to be on the last page; now it produces a loud refusal, but
	// only after burning RC on every failed page. Catch it at config load instead.
	//
	// Measured cost is ~95 RC per entry over a ~200 fixed base (docs/rc-costs.md). The
	// 20% headroom covers metering variance between pages of different byte lengths.
	const perEntryRC, baseRC = 95, 200
	if want := (baseRC + perEntryRC*c.Page.MaxEntries) * 12 / 10; c.Submit.RcLimit < want {
		return fmt.Errorf("submit.rc_limit %d cannot fit a full page of %d entries (needs ~%d: "+
			"~%d RC/entry over a ~%d base, plus headroom). Every full page would revert while the "+
			"cheap calls succeeded — raise rc_limit or lower page.max_entries",
			c.Submit.RcLimit, c.Page.MaxEntries, want, perEntryRC, baseRC)
	}
	if c.Submit.Keeper && c.Contracts.Funder == "" {
		return fmt.Errorf("submit.keeper requires contracts.funder (the C2 contract id)")
	}
	return nil
}

// SourceKind is the data source driving share computation.
const (
	SourceContent = "content" // Hive posts and votes -> C3
	SourceLP      = "lp"      // indexer liquidity events -> C5
)

// Kind returns the configured source kind, defaulting to content.
func (c *Config) Kind() string {
	if c.Source.Kind == "" {
		return SourceContent
	}
	return c.Source.Kind
}

// validateSource applies the checks that belong to one source kind only. Content
// requires a tag and a weighting/attribution policy; LP requires an indexer and a
// pool, and has no use for either — validating them anyway would force operators to
// carry meaningless content settings in an LP config.
func (c *Config) validateSource() error {
	switch c.Kind() {
	case SourceLP:
		if c.Indexer.API == "" {
			return fmt.Errorf("source.kind=lp requires indexer.api (the Hasura GraphQL endpoint)")
		}
		if c.Indexer.Pool == "" {
			return fmt.Errorf("source.kind=lp requires indexer.pool (the pool contract id)")
		}
		if c.Indexer.PageSize < 0 {
			return fmt.Errorf("indexer.page_size must be >= 0 (0 selects the default)")
		}
		return nil
	case SourceContent:
		if c.Source.Tag == "" {
			return fmt.Errorf("source.tag is required")
		}
		switch hivesrc.WeightMode(c.Source.Weight) {
		case hivesrc.WeightHiveRshares:
		case hivesrc.WeightTokenStake:
			if c.Contracts.Stake == "" {
				return fmt.Errorf("source.weight=token_stake requires contracts.stake (the C1 contract id)")
			}
		default:
			return fmt.Errorf("source.weight must be %q or %q, got %q",
				hivesrc.WeightHiveRshares, hivesrc.WeightTokenStake, c.Source.Weight)
		}
		switch hivesrc.Attribution(c.Source.Attribution) {
		case hivesrc.AttributeCashout, hivesrc.AttributeCreated:
		default:
			return fmt.Errorf("source.attribution must be %q or %q, got %q",
				hivesrc.AttributeCashout, hivesrc.AttributeCreated, c.Source.Attribution)
		}
		if c.Shares.AuthorRewardBps < 0 || c.Shares.AuthorRewardBps > 10000 {
			return fmt.Errorf("shares.author_reward_bps must be 0..10000, got %d", c.Shares.AuthorRewardBps)
		}
		if _, _, err := parseCurve(c.Shares.AuthorCurve); err != nil {
			return fmt.Errorf("shares.author_curve: %w", err)
		}
		if _, _, err := parseCurve(c.Shares.CurationCurve); err != nil {
			return fmt.Errorf("shares.curation_curve: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("source.kind must be %q or %q, got %q", SourceContent, SourceLP, c.Source.Kind)
	}
}

// parseCurve reads a "num/den" rational exponent. A bare "2" means "2/1".
func parseCurve(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	num, den := s, "1"
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num, den = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, 0, fmt.Errorf("bad numerator in %q", s)
	}
	d, err := strconv.Atoi(den)
	if err != nil {
		return 0, 0, fmt.Errorf("bad denominator in %q", s)
	}
	if n <= 0 || d <= 0 {
		return 0, 0, fmt.Errorf("curve %q: numerator and denominator must both be > 0", s)
	}
	return n, d, nil
}

// ShareConfig converts the file's curve strings into sharecore's integer form.
func (c *Config) ShareConfig() sharecore.Config {
	an, ad, _ := parseCurve(c.Shares.AuthorCurve)
	cn, cd, _ := parseCurve(c.Shares.CurationCurve)
	return sharecore.Config{
		AuthorRewardBps:  c.Shares.AuthorRewardBps,
		AuthorCurveNum:   an,
		AuthorCurveDen:   ad,
		CurationCurveNum: cn,
		CurationCurveDen: cd,
		Muted:            c.Shares.Muted,
	}
}

// ExampleConfig is what `reporter init-config` writes.
const ExampleConfig = `{
  "hive":      { "api": ["https://api.hive.blog"] },
  "vsc":       { "api": "https://api.vsc.eco/api/v1/graphql", "net_id": "vsc-mainnet" },
  "contracts": {
    "distributor": "vsc1...C3",
    "funder":      "vsc1...C2",
    "stake":       ""
  },
  "epoch":  { "genesis": 0, "len": 28800 },
  "source": {
    "tag":         "yourtribe",
    "limit":       1000,
    "attribution": "cashout",
    "weight":      "hive_rshares",
    "exclude":     []
  },
  "shares": {
    "author_reward_bps": 5000,
    "author_curve":      "1/1",
    "curation_curve":    "1/2",
    "muted":             []
  },
  "page":   { "max_entries": 60, "max_bytes": 3800 },
  "submit": {
    "account":       "hive:yourreporter",
    "wif_env":       "REPORTER_ACTIVE_WIF",
    "rc_limit":      60000,
    "progress_file": "reporter-progress.json",
    "keeper":        true,
    "pull_funding":  true,
    "finalize":      true,
    "confirm_tries": 6,
    "confirm_interval_sec": 15
  }
}
`

// ExampleLPConfig is what `reporter init-config lp` writes: the same pipeline fed
// from the indexer's liquidity events instead of Hive, for a C5 instance.
//
// Note what is ABSENT — no tag, no weighting, no curation curve, no author split.
// Those are content policy; an LP epoch is priced purely on how much liquidity was
// held at both boundaries. `hive.api` is still needed because epoch selection reads
// the chain head.
const ExampleLPConfig = `{
  "hive":      { "api": ["https://api.hive.blog"] },
  "vsc":       { "api": "https://api.vsc.eco/api/v1/graphql", "net_id": "vsc-mainnet" },
  "indexer":   {
    "api":       "https://indexer.example.com/v1/graphql",
    "secret":    "",
    "pool":      "vsc1...POOL",
    "page_size": 1000
  },
  "contracts": {
    "distributor": "vsc1...C5",
    "funder":      "vsc1...C2",
    "stake":       ""
  },
  "epoch":  { "genesis": 0, "len": 28800 },
  "source":  { "kind": "lp" },
  "page":   { "max_entries": 12, "max_bytes": 3500 },
  "submit": {
    "rc_limit":      10000,
    "keeper":        false,
    "pull_funding":  true,
    "finalize":      true,
    "confirm_tries": 6,
    "confirm_interval_sec": 15,
    "progress_file": "reporter-progress.json"
  }
}
`
