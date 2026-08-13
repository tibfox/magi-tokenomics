package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"magi_token/reporter/hivesrc"
	"magi_token/reporter/sharecore"
	"magi_token/reporter/submit"
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
		// ChainID selects which HIVE chain signatures are made over. Empty means
		// mainnet, which is correct for production. This is NOT vsc.net_id: a VSC
		// network can run on a Hive chain that is not mainnet, and signing against
		// the wrong one makes the signature recover to a different key — the node
		// then reports "missing required active authority", which reads as a
		// permissions problem rather than a chain mismatch.
		ChainID string `json:"chain_id"`
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
		Distributor string `json:"distributor"` // the distributor contract
		// Channel names the reward channel on that distributor. One deployed
		// distributor serves several — content, LP, whatever a tenant adds — each with
		// its own share book, funding bucket and reporter authority, so a reporter has
		// to say which one it is writing. Required.
		Channel string `json:"channel"`
		Funder  string `json:"funder"` // C2; empty = a separate keeper pokes it
		Stake   string `json:"stake"`  // C1; required only for token_stake weighting
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
		// Tags are the tags or communities that pull a post into this pool. A post
		// matching ANY of them is indexed once — matching several does not pay twice.
		Tags  []string `json:"tags"`
		Limit int      `json:"limit"`
		// ExcludedTags are checked AFTER Tags: a post carrying any of them is dropped
		// even if it also carries an included tag. This is the escape hatch for a tag
		// that is broad enough to be useful and broad enough to drag in noise.
		ExcludedTags []string `json:"excluded_tags"`
		// A post is ALWAYS scored in the epoch its Hive payout falls in, once voting
		// has closed, so every vote is counted exactly once by its weight. There is
		// no setting for scoring earlier — see the rule at the top of hivesrc.
		Weight string `json:"weight"` // hive_rshares | token_stake
		// Exclude is a list of ACCOUNTS, not tags — they earn nothing and their votes
		// carry no weight. ExcludedTags above is the tag-shaped filter.
		Exclude []string `json:"exclude"`
		// Kind selects the data source: "content" (default) reads Hive posts/votes
		// for C3; "lp" replays liquidity events from the indexer for C5. The rest of
		// the pipeline — canonicalisation, pagination, submission, Attest — is shared.
		Kind string `json:"kind"`
		// CashoutDays is how long a post collects votes before it pays. It sets both
		// the window this pool reads and the vote cutoff, so changing it changes which
		// votes count, not merely when. 0 selects Hive's own 7 days.
		CashoutDays int `json:"cashout_days"`
		// IgnoreDeclinedPayout=true pays an author who declined their Hive payout.
		// The default (false) honours the decline, which is what an author who set
		// max_accepted_payout to zero asked for.
		IgnoreDeclinedPayout bool `json:"ignore_declined_payout"`
		// DisableDownvotes=true drops negative votes entirely, so a downvote cannot
		// reduce a payout. False lets them net off against the positive rshares —
		// see hivesrc for what a downvote can and cannot do to curation.
		DisableDownvotes bool `json:"disable_downvotes"`
		// Vote mana, and ONLY meaningful for weight=token_stake.
		//
		// hive_rshares already carries Hive's own mana inside the rshares figure, so
		// setting these there would tax a vote twice. token_stake has no such budget:
		// without one an account votes at full stake as often as it likes, which is
		// why these are required in that mode rather than optional.
		//
		// Consumption is in hundredths of a percent of full power, matching SCOT:
		// 200 = 2% per full vote = 50 votes to empty.
		VoteRegenDays            int `json:"vote_regen_days"`
		VotePowerConsumption     int `json:"vote_power_consumption"`
		DownvoteRegenDays        int `json:"downvote_regen_days"`
		DownvotePowerConsumption int `json:"downvote_power_consumption"`
	} `json:"source"`

	Shares struct {
		AuthorRewardBps int      `json:"author_reward_bps"`
		AuthorCurve     string   `json:"author_curve"`   // "num/den", e.g. "1/1"
		CurationCurve   string   `json:"curation_curve"` // "1/2" = sqrt = early voters win
		Muted           []string `json:"muted"`
		// StakedBps is the share of every payout delivered as STAKE rather than
		// liquid tokens. The reporter only records it so `epoch` can cross-check the
		// distributor's own setting — the split itself happens on-chain at claim,
		// because that is the only place the tokens exist to be split.
		StakedBps int `json:"staked_bps"`
		// AppTax skims a percentage from posts published outside a designated app
		// and pays it to Beneficiary.
		//
		// The `app` it matches on is SELF-DECLARED in the post's json_metadata: a
		// client can put any string there, so this shapes the behaviour of ordinary
		// users on ordinary front-ends and does nothing to anyone posting via the
		// API. Treat it as an incentive, never as enforcement.
		AppTax struct {
			Bps         int      `json:"bps"`
			Apps        []string `json:"apps"`        // designated apps, matched on the part before "/"
			Beneficiary string   `json:"beneficiary"` // where the skim goes; "hive:acct"
		} `json:"app_tax"`
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
	if c.Contracts.Channel == "" {
		return fmt.Errorf("contracts.channel is required — the distributor's reward channel this reporter writes")
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
	// The cost constants live in submit/rccost.go, next to nothing else, because they
	// describe the CONTRACT and not the reporter — and because itest binds them to a
	// real metered measurement. They have gone stale twice (channel-scoping, then
	// event emission); a stale one makes this check UNDER-estimate a page, so a config
	// loads cleanly and then reverts every full page it sends.
	if want := submit.RCForPage(c.Page.MaxEntries); c.Submit.RcLimit < want {
		return fmt.Errorf("submit.rc_limit %d cannot fit a full page of %d entries (needs ~%d: "+
			"~%d RC/entry over a ~%d base, plus headroom). Every full page would revert while the "+
			"cheap calls succeeded — raise rc_limit or lower page.max_entries",
			c.Submit.RcLimit, c.Page.MaxEntries, want, submit.RCPerEntry, submit.RCBase)
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
		if len(c.Source.Tags) == 0 {
			return fmt.Errorf("source.tags is required (at least one tag or community)")
		}
		if len(c.Source.Tags) > MaxTags {
			return fmt.Errorf("source.tags allows at most %d, got %d", MaxTags, len(c.Source.Tags))
		}
		if len(c.Source.ExcludedTags) > MaxTags {
			return fmt.Errorf("source.excluded_tags allows at most %d, got %d", MaxTags, len(c.Source.ExcludedTags))
		}
		// A tag on both lists can never index anything: excluded is applied after
		// included, so the pool would silently read an empty feed.
		for _, in := range c.Source.Tags {
			for _, ex := range c.Source.ExcludedTags {
				if in == ex {
					return fmt.Errorf("source.tags and source.excluded_tags both contain %q — "+
						"exclusion is applied second, so that tag could never index a post", in)
				}
			}
		}
		if c.Source.CashoutDays < 0 || c.Source.CashoutDays > 30 {
			return fmt.Errorf("source.cashout_days must be 1..30 (0 selects Hive's 7), got %d",
				c.Source.CashoutDays)
		}
		if err := c.validateAppTax(); err != nil {
			return err
		}
		switch hivesrc.WeightMode(c.Source.Weight) {
		case hivesrc.WeightHiveRshares:
			// Hive's rshares already embed Hive's mana. A second budget here would
			// charge the same vote twice, so refuse rather than silently ignore.
			if c.Source.VoteRegenDays != 0 || c.Source.VotePowerConsumption != 0 ||
				c.Source.DownvoteRegenDays != 0 || c.Source.DownvotePowerConsumption != 0 {
				return fmt.Errorf("source.weight=hive_rshares does not use the vote-mana settings — " +
					"rshares already carry Hive's own mana, so applying a second budget would " +
					"charge the same vote twice")
			}
		case hivesrc.WeightTokenStake:
			if c.Contracts.Stake == "" {
				return fmt.Errorf("source.weight=token_stake requires contracts.stake (the C1 contract id)")
			}
			if err := c.validateMana(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("source.weight must be %q or %q, got %q",
				hivesrc.WeightHiveRshares, hivesrc.WeightTokenStake, c.Source.Weight)
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
		// Only when a rate is actually set: passing a beneficiary with no rate would
		// put an account into the share book that never earns, and validateAppTax
		// already refuses a rate without a beneficiary.
		AppTaxBeneficiary: func() string {
			if c.Shares.AppTax.Bps > 0 {
				return c.Shares.AppTax.Beneficiary
			}
			return ""
		}(),
	}
}

// ExampleConfig is what `reporter init-config` writes.
const ExampleConfig = `{
  "hive":      { "api": ["https://api.hive.blog"] },
  "vsc":       { "api": "https://api.vsc.eco/api/v1/graphql", "net_id": "vsc-mainnet" },
  "contracts": {
    "distributor": "vsc1...DIST",
    "channel":     "content",
    "funder":      "vsc1...C2",
    "stake":       ""
  },
  "epoch":  { "genesis": 0, "len": 28800 },
  "source": {
    "tags":                   ["yourtribe"],
    "excluded_tags":          [],
    "limit":                  1000,
    "weight":                 "hive_rshares",
    "exclude":                [],
    "cashout_days":           7,
    "ignore_declined_payout": false,
    "disable_downvotes":      false
  },
  "shares": {
    "author_reward_bps": 5000,
    "author_curve":      "1/1",
    "curation_curve":    "1/2",
    "muted":             [],
    "staked_bps":        0,
    "app_tax":           { "bps": 0, "apps": [], "beneficiary": "" }
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
    "distributor": "vsc1...DIST",
    "channel":     "lp",
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

// MaxTags mirrors the tag limit tribes are used to from SCOT's admin panel. It is a
// policy cap rather than a technical one: each tag costs one feed walk per epoch.
const MaxTags = 5

// validateMana checks the four token_stake vote-budget settings.
//
// They are REQUIRED in that mode, not optional. token_stake weighs a vote by the
// voter's staked balance, and with no budget attached that weight is reusable without
// limit — one account can vote every post in the epoch at full stake. The budget is
// what makes a vote cost something.
func (c *Config) validateMana() error {
	type k struct {
		name string
		val  int
		hi   int
	}
	for _, f := range []k{
		{"vote_regen_days", c.Source.VoteRegenDays, 30},
		{"downvote_regen_days", c.Source.DownvoteRegenDays, 30},
	} {
		if f.val < 1 || f.val > f.hi {
			return fmt.Errorf("source.%s must be 1..%d when weight=token_stake, got %d",
				f.name, f.hi, f.val)
		}
	}
	for _, f := range []k{
		{"vote_power_consumption", c.Source.VotePowerConsumption, 10000},
		{"downvote_power_consumption", c.Source.DownvotePowerConsumption, 10000},
	} {
		if f.val < 1 || f.val > f.hi {
			return fmt.Errorf("source.%s must be 1..%d (hundredths of a percent) "+
				"when weight=token_stake, got %d", f.name, f.hi, f.val)
		}
	}
	// A downvote budget is only spendable if downvotes count at all.
	if c.Source.DisableDownvotes && c.Source.DownvotePowerConsumption > 0 {
		// not an error — SCOT panels ship this combination — but it is inert, and an
		// operator tuning a number that does nothing deserves to know.
		return nil
	}
	return nil
}

// validateAppTax rejects a tax that cannot be collected. Every field is load-bearing:
// a rate with no beneficiary skims into nothing, and a rate with no designated app
// taxes every post in the pool including those from the operator's own front-end.
func (c *Config) validateAppTax() error {
	t := c.Shares.AppTax
	if t.Bps == 0 && len(t.Apps) == 0 && t.Beneficiary == "" {
		return nil // not configured: capability follows config
	}
	if t.Bps < 1 || t.Bps > 10000 {
		return fmt.Errorf("shares.app_tax.bps must be 1..10000 when app_tax is configured, got %d", t.Bps)
	}
	if len(t.Apps) == 0 {
		return fmt.Errorf("shares.app_tax.apps is required when a tax rate is set — " +
			"with no designated app every post is taxed, including those from your own front-end")
	}
	if t.Beneficiary == "" {
		return fmt.Errorf("shares.app_tax.beneficiary is required when a tax rate is set — " +
			"the skim has to be paid to someone or it is burned")
	}
	if !strings.HasPrefix(t.Beneficiary, "hive:") {
		return fmt.Errorf("shares.app_tax.beneficiary must carry a ledger domain, e.g. hive:acct, got %q",
			t.Beneficiary)
	}
	for _, a := range t.Apps {
		if a == "" {
			return fmt.Errorf("shares.app_tax.apps contains an empty entry")
		}
	}
	return nil
}

// PayoutWindow is how long a post collects votes before it pays.
//
// It sets BOTH the creation window the feed is walked over and the vote cutoff, so
// it decides which votes count, not merely when they are counted. Zero selects
// Hive's own seven days, which is what a pool wants unless it deliberately runs a
// shorter cycle than the chain it reads.
func (c *Config) PayoutWindow() time.Duration {
	if c.Source.CashoutDays > 0 {
		return time.Duration(c.Source.CashoutDays) * 24 * time.Hour
	}
	return hivesrc.DefaultPayoutPeriod
}
