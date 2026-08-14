// Command reporter is the off-chain share reporter for the magi-tokenomics
// framework: it reads an epoch's Hive activity, computes per-account shares
// deterministically, and pushes them to a C3/C5 distributor contract.
//
// It is deliberately built as four separable steps so each can be inspected before
// anything is signed:
//
//	reporter epoch       — where are we? verifies config against on-chain state
//	reporter compute      — what would be reported? prints the canonical shares
//	reporter plan         — what would be sent? prints the exact custom_json bodies
//	reporter run          — send it (DRY-RUN unless -broadcast is passed)
//
// `run` is idempotent and resumable: progress is recorded per (epoch,action,page)
// before it can be lost, and the contract independently refuses a repeated page,
// so a crash mid-epoch costs nothing but a restart.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"magi_token/reporter/broadcast"
	"magi_token/reporter/hivesrc"
	"magi_token/reporter/lpsrc"
	"magi_token/reporter/sharecore"
	"magi_token/reporter/submit"
	"magi_token/reporter/vscapi"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `reporter — magi-tokenomics share reporter

usage: reporter <command> [flags]

commands:
  init-config [lp]         write an example config to stdout ("lp" for a C5/LP one)
  epoch     -config F      show head/epoch state and verify config against chain
  compute   -config F      fetch + compute an epoch's shares (no chain writes)
  plan      -config F      print the ordered calls that would be broadcast
  run       -config F      execute the plan (dry-run unless -broadcast)
  proof     -config F      print an account's share + merkle proof for an epoch
  root      -entries "..."  commit-root for a share list you push by hand

common flags:
  -config F                path to the config file (required except init-config)
  -epoch N                 target epoch; default = oldest closed-but-unfinalized
  -broadcast               (run only) actually sign and send
  -json                    machine-readable output where applicable
`

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := args[0], args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to config file")
	epochFlag := fs.String("epoch", "", "target epoch (default: oldest closed-but-unfinalized)")
	doBroadcast := fs.Bool("broadcast", false, "actually sign and broadcast")
	asJSON := fs.Bool("json", false, "machine-readable output")
	acctFlag := fs.String("account", "", "(proof only) the account to prove")

	switch cmd {
	case "init-config":
		if len(os.Args) > 2 && os.Args[2] == "lp" {
			fmt.Print(ExampleLPConfig)
			return nil
		}
		fmt.Print(ExampleConfig)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "root":
		// no config needed: this is pure arithmetic over the list given
		fsRoot := flag.NewFlagSet("root", flag.ContinueOnError)
		entries := fsRoot.String("entries", "", `"hive:a:100,hive:b:50"`)
		acct := fsRoot.String("account", "", "also emit this account's proof")
		asJSONRoot := fsRoot.Bool("json", false, "machine-readable output")
		if err := fsRoot.Parse(rest); err != nil {
			return err
		}
		return (&app{cfg: &Config{}}).cmdRoot(*entries, *acct, *asJSONRoot)
	case "epoch", "compute", "plan", "run", "proof":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}

	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required for %q", cmd)
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return err
	}

	app, err := newApp(cfg)
	if err != nil {
		return err
	}

	switch cmd {
	case "epoch":
		return app.cmdEpoch(*asJSON)
	case "compute":
		return app.cmdCompute(*epochFlag, *asJSON)
	case "plan":
		return app.cmdPlan(*epochFlag, *asJSON)
	case "run":
		return app.cmdRun(*epochFlag, *doBroadcast)
	case "proof":
		return app.cmdProof(*epochFlag, *acctFlag, *asJSON)
	}
	return nil
}

// ---- app -----------------------------------------------------------------

// stateReader is the slice of vscapi.Client this command actually needs. Kept as an
// interface so the finalize gate — the one piece here that decides whether an
// irreversible call goes out — is testable without a chain.
type stateReader interface {
	StateGet(contractID string, keys []string) (map[string]string, error)
}

type app struct {
	cfg  *Config
	hive hivesrc.Transport
	vsc  *vscapi.Client
	// state defaults to vsc; tests substitute it.
	state stateReader
}

func (a *app) reader() stateReader {
	if a.state != nil {
		return a.state
	}
	return a.vsc
}

func newApp(cfg *Config) (*app, error) {
	return &app{
		cfg:  cfg,
		hive: hivesrc.NewHTTPTransport(cfg.Hive.API[0]),
		vsc:  vscapi.New(cfg.VSC.API),
	}, nil
}

// resolveEpoch turns the -epoch flag into a concrete epoch, defaulting to the
// oldest closed-but-unfinalized one (see oldestUnfinalized for why).
func (a *app) resolveEpoch(flagVal string) (uint64, uint64, error) {
	head, err := hivesrc.HeadBlock(a.hive)
	if err != nil {
		return 0, 0, err
	}
	if flagVal != "" {
		ep, err := strconv.ParseUint(strings.TrimSpace(flagVal), 10, 64)
		if err != nil {
			return 0, head, fmt.Errorf("-epoch must be a non-negative integer: %w", err)
		}
		// Refuse an epoch that has not closed: submitting a partial report and then
		// finalizing it would permanently lock out the rest of that epoch.
		//
		// Bounds-check first. A large -epoch wraps the arithmetic to a small,
		// plausible-looking block range, which would sail through the head test below
		// and score real blocks under a nonsense epoch index.
		_, end, ok := hivesrc.EpochBounds(a.cfg.Epoch.Genesis, a.cfg.Epoch.Len, ep)
		if !ok {
			return 0, head, fmt.Errorf("epoch %d is out of range for genesis %d and epochLen %d "+
				"(its block range overflows)", ep, a.cfg.Epoch.Genesis, a.cfg.Epoch.Len)
		}
		if head < end {
			return 0, head, fmt.Errorf("epoch %d has not closed yet (ends at block %d, head is %d)", ep, end, head)
		}
		return ep, head, nil
	}
	latest, err := hivesrc.LatestClosedEpoch(a.cfg.Epoch.Genesis, a.cfg.Epoch.Len, head)
	if err != nil {
		return 0, head, err
	}
	ep, err := a.oldestUnfinalized(latest)
	return ep, head, err
}

// oldestUnfinalized picks the OLDEST closed-but-unfinalized epoch within the
// lookback window, falling back to the latest closed epoch if all are done.
//
// Targeting the oldest rather than the newest matters for two reasons:
//
//	ATTEST QUORUM. In Attest mode N machines must push byte-identical pages for the
//	  same epoch. If each simply took "the latest closed epoch", two machines run
//	  minutes apart across an epoch boundary would target DIFFERENT epochs and the
//	  quorum would never be reached. Converging on the oldest outstanding epoch is
//	  stable: every machine picks the same one regardless of when it runs.
//	CATCH-UP. After downtime, epochs are processed in order instead of the gap being
//	  skipped forever — which also matches the contracts, where C2 accrues bucket
//	  owed per epoch and C3 pulls per epoch.
func (a *app) oldestUnfinalized(latest uint64) (uint64, error) {
	// The window bounds a single batched state read. It is CONFIGURABLE because a
	// hardcoded one silently strands work: resolveEpoch returns ONE epoch per run, so
	// with daily epochs and a daily cron a backlog clears exactly as fast as it
	// accrues and never shrinks. The moment an outstanding epoch falls below `first`
	// it is never selected again — its funding just sits there until a guardian
	// notices. (The old comment pointed at an `Epoch.Lookback` config field that did
	// not exist.)
	first := a.windowFloor(latest)
	keys := make([]string, 0, latest-first+1)
	for ep := first; ep <= latest; ep++ {
		keys = append(keys, a.chKey("status", strconv.FormatUint(ep, 10)))
	}
	state, err := a.vsc.StateGet(a.cfg.Contracts.Distributor, keys)
	if err != nil {
		return 0, err
	}
	for ep := first; ep <= latest; ep++ {
		if state[a.chKey("status", strconv.FormatUint(ep, 10))] == "" {
			return ep, nil
		}
	}
	// Everything in the window is finalized/cancelled. Return the latest closed
	// epoch so the caller reports "already fully submitted" rather than an error.
	return latest, nil
}

// windowFloor is the oldest epoch the lookback window will consider.
func (a *app) windowFloor(latest uint64) uint64 {
	lookback := a.cfg.Epoch.Lookback
	if lookback <= 0 {
		lookback = defaultLookback
	}
	if latest < lookback {
		return 0
	}
	return latest - lookback + 1
}

// strandedBelowWindow reports epochs that fell out of the lookback window while still
// unfinalized. They will never be selected again, so nothing else will ever mention
// them: their funding sits on the distributor until a guardian runs the stale rescue,
// which pays the treasury and not the earners. Bounded so the diagnostic cannot
// itself become an unbounded scan.
func (a *app) strandedBelowWindow(first uint64) []uint64 {
	if first == 0 {
		return nil
	}
	const probe = 20
	lo := uint64(0)
	if first > probe {
		lo = first - probe
	}
	keys := make([]string, 0, first-lo)
	for ep := lo; ep < first; ep++ {
		keys = append(keys, a.chKey("status", strconv.FormatUint(ep, 10)))
	}
	if len(keys) == 0 {
		return nil
	}
	state, err := a.reader().StateGet(a.cfg.Contracts.Distributor, keys)
	if err != nil {
		// A diagnostic must not break the run — but it must not go quiet either.
		// Returning nil here would print "nothing stranded" when the truth is "could
		// not tell", which is the same silent-zero shape this whole audit kept finding.
		fmt.Printf("note: could not check for stranded epochs below the lookback window: %v\n", err)
		return nil
	}
	var out []uint64
	for ep := lo; ep < first; ep++ {
		if state[a.chKey("status", strconv.FormatUint(ep, 10))] == "" {
			out = append(out, ep)
		}
	}
	return out
}

// verifyChainConfig compares the local epoch schedule against the distributor's
// own state. A silent mismatch here is the worst failure mode in the whole
// service: the report would cover the wrong block range and then be finalized.
func (a *app) verifyChainConfig() []string {
	var problems []string
	want := map[string]uint64{
		"cfg_genesis":  a.cfg.Epoch.Genesis,
		"cfg_epochLen": a.cfg.Epoch.Len,
	}
	got, err := a.reader().StateGet(a.cfg.Contracts.Distributor,
		[]string{"cfg_genesis", "cfg_epochLen", "cfg_funder", "cfg_role"})
	if err != nil {
		return []string{fmt.Sprintf("could not read distributor state: %v", err)}
	}
	// A content distributor and an LP one are the SAME CODE deployed twice, so every
	// other cross-check here passes for either. Without this, a swapped distributor id
	// scores an epoch from the wrong data source — Hive posts written into the LP
	// contract, or liquidity into the content one — and then finalizes it. The label
	// is optional, so an unset one is not a problem, only a disagreement is.
	if role := got["cfg_role"]; role != "" && role != a.cfg.Kind() {
		problems = append(problems, fmt.Sprintf(
			"ROLE MISMATCH: this reporter is configured as source.kind=%q but the distributor "+
				"at %s declares cfg_role=%q — check contracts.distributor, you are about to "+
				"report the wrong kind of data into it",
			a.cfg.Kind(), a.cfg.Contracts.Distributor, role))
	}
	for k, w := range want {
		raw, ok := got[k]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is not set on %s — is it initialised?", k, a.cfg.Contracts.Distributor))
			continue
		}
		v, perr := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("%s on chain is not a uint: %q", k, raw))
			continue
		}
		if v != w {
			problems = append(problems, fmt.Sprintf("%s MISMATCH: config says %d, chain says %d", k, w, v))
		}
	}
	// The keeper poke only makes sense against the distributor's actual funder.
	if a.cfg.Contracts.Funder != "" {
		if f, ok := got["cfg_funder"]; ok && f != a.cfg.Contracts.Funder {
			problems = append(problems, fmt.Sprintf("cfg_funder MISMATCH: config says %s, chain says %s",
				a.cfg.Contracts.Funder, f))
		}
	}
	return problems
}

func (a *app) cmdEpoch(asJSON bool) error {
	head, err := hivesrc.HeadBlock(a.hive)
	if err != nil {
		return err
	}
	g, el := a.cfg.Epoch.Genesis, a.cfg.Epoch.Len
	var current uint64
	if head >= g {
		current = (head - g) / el
	}
	closed, closedErr := hivesrc.LatestClosedEpoch(g, el, head)
	problems := a.verifyChainConfig()

	if asJSON {
		out := map[string]any{
			"head": head, "genesis": g, "epoch_len": el,
			"current_epoch": current, "problems": problems,
		}
		if closedErr == nil {
			out["latest_closed_epoch"] = closed
			if t, terr := a.oldestUnfinalized(closed); terr == nil {
				out["target_epoch"] = t
			}
			// Epochs that fell out of the window while still unfinalized will never
			// be selected again, so this is the only place they are ever mentioned.
			if st := a.strandedBelowWindow(a.windowFloor(closed)); len(st) > 0 {
				out["stranded_epochs"] = st
			}
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	fmt.Printf("head block          %d\n", head)
	fmt.Printf("genesis / epochLen  %d / %d\n", g, el)
	fmt.Printf("current epoch       %d  (blocks %d..%d, in progress)\n",
		current, g+current*el, g+(current+1)*el-1)
	target, targetErr := closed, closedErr
	if closedErr != nil {
		fmt.Printf("latest closed       none (%v)\n", closedErr)
	} else {
		fmt.Printf("latest closed epoch %d  (blocks %d..%d)\n",
			closed, g+closed*el, g+(closed+1)*el-1)
		target, targetErr = a.oldestUnfinalized(closed)
		if targetErr != nil {
			fmt.Printf("default target      unknown (%v)\n", targetErr)
		} else {
			note := ""
			if target != closed {
				note = "  (catching up)"
			}
			fmt.Printf("default target      epoch %d  (blocks %d..%d)%s\n",
				target, g+target*el, g+(target+1)*el-1, note)
		}
		// A run handles ONE epoch, so anything below the window is never selected
		// again and nothing else will ever mention it. Its funding sits on the
		// distributor until a guardian runs the stale rescue — which pays the
		// treasury, not the earners.
		if st := a.strandedBelowWindow(a.windowFloor(closed)); len(st) > 0 {
			fmt.Printf("\nSTRANDED            epochs %v are unfinalized and BELOW the lookback window\n", st)
			fmt.Printf("                    they will never be targeted again. Raise epoch.lookback\n")
			fmt.Printf("                    (currently %d) and re-run to work them off.\n", a.cfg.Epoch.Lookback)
		}
	}

	// Show where the target epoch actually stands on chain, so an operator can see
	// whether it still needs funding, shares or finalizing.
	if targetErr == nil {
		eps := strconv.FormatUint(target, 10)
		st, err := a.vsc.StateGet(a.cfg.Contracts.Distributor,
			[]string{a.chKey("status", eps), a.chKey("funded", eps),
				a.chKey("totalShares", eps), a.chKey("chal", eps)})
		if err == nil {
			fmt.Printf("\nepoch %s on chain:\n", eps)
			fmt.Printf("  status       %s\n", orDash(st[a.chKey("status", eps)], "open"))
			fmt.Printf("  funded       %s\n", orDash(st[a.chKey("funded", eps)], "0"))
			fmt.Printf("  totalShares  %s\n", orDash(st[a.chKey("totalShares", eps)], "0"))
			if c := st[a.chKey("chal", eps)]; c != "" {
				fmt.Printf("  challenge until block %s\n", c)
			}
		}
	}

	if len(problems) > 0 {
		fmt.Printf("\nCONFIG PROBLEMS (%d):\n", len(problems))
		for _, p := range problems {
			fmt.Printf("  ! %s\n", p)
		}
		return fmt.Errorf("config does not match chain state — fix before submitting")
	}
	fmt.Printf("\nconfig matches chain state\n")
	return nil
}

func orDash(v, dflt string) string {
	if v == "" {
		return dflt + " (unset)"
	}
	return v
}

// computed bundles everything cmdCompute/cmdPlan/cmdRun need.
type computed struct {
	Epoch  uint64
	Window hivesrc.Window
	Posts  int // content mode: posts scored
	// Providers is the LP-mode analogue of Posts: liquidity providers with a
	// non-zero position at BOTH epoch boundaries.
	Providers int
	Result    sharecore.Result
	Canon     string
	Pages     []sharecore.Page
}

func (a *app) compute(epochFlag string) (*computed, error) {
	ep, _, err := a.resolveEpoch(epochFlag)
	if err != nil {
		return nil, err
	}
	if problems := a.verifyChainConfig(); len(problems) > 0 {
		return nil, fmt.Errorf("refusing to compute: %s", strings.Join(problems, "; "))
	}
	if a.cfg.Kind() == SourceLP {
		return a.computeLP(ep)
	}

	win, err := hivesrc.EpochWindow(a.hive, a.cfg.Epoch.Genesis, a.cfg.Epoch.Len, ep)
	if err != nil {
		return nil, err
	}

	opt := hivesrc.Options{
		Tags:                 a.cfg.Source.Tags,
		Limit:                a.cfg.Source.Limit,
		Mode:                 hivesrc.WeightMode(a.cfg.Source.Weight),
		SnapshotHeight:       win.EndBlock,
		ExcludeAccounts:      a.cfg.Source.Exclude,
		ExcludedTags:         a.cfg.Source.ExcludedTags,
		PayoutWindow:         a.cfg.PayoutWindow(),
		IgnoreDeclinedPayout: a.cfg.Source.IgnoreDeclinedPayout,
		DisableDownvotes:     a.cfg.Source.DisableDownvotes,
	}
	if t := a.cfg.Shares.AppTax; t.Bps > 0 {
		opt.AppTax = &hivesrc.AppTaxPolicy{
			Bps: t.Bps, Apps: t.Apps, Beneficiary: t.Beneficiary,
		}
	}
	// Mana applies to token_stake only; hive_rshares already carries Hive's own.
	if opt.Mode == hivesrc.WeightTokenStake {
		opt.Mana = &hivesrc.ManaPolicy{
			RegenDays:           a.cfg.Source.VoteRegenDays,
			Consumption:         a.cfg.Source.VotePowerConsumption,
			DownvoteRegenDays:   a.cfg.Source.DownvoteRegenDays,
			DownvoteConsumption: a.cfg.Source.DownvotePowerConsumption,
		}
	}
	// The epoch is defined by PAYOUT time, but the feed can only be paged by creation
	// time — so walk the window shifted back one payout period and let
	// PayoutSince/Until decide actual membership. The margin absorbs posts whose
	// payout_at is not exactly created+7d.
	const margin = time.Hour
	payout := a.cfg.PayoutWindow()
	opt.PayoutSince, opt.PayoutUntil = win.StartTime, win.EndTime
	opt.Since = win.StartTime.Add(-payout - margin)
	opt.Until = win.EndTime.Add(-payout + margin)
	if opt.Mode == hivesrc.WeightTokenStake {
		opt.Stake = vscapi.NewStakeSource(a.vsc, a.cfg.Contracts.Stake)
	}

	posts, err := hivesrc.Collect(a.hive, opt)
	if err != nil {
		return nil, err
	}
	res := sharecore.ComputeShares(posts, a.cfg.ShareConfig())
	// Last gate before untrusted names become an irreversible payload. lpsrc checks
	// its providers at the source; this covers the Hive path, whose names come from
	// whatever API endpoint the operator configured.
	if err := sharecore.ValidateAccounts(res); err != nil {
		return nil, fmt.Errorf("refusing to build a report: %w", err)
	}
	canon := sharecore.Canonicalize(res)
	return &computed{
		Epoch:  ep,
		Window: win,
		Posts:  len(posts),
		Result: res,
		Canon:  canon,
		Pages:  sharecore.Paginate(canon, a.cfg.Page.MaxEntries, a.cfg.Page.MaxBytes),
	}, nil
}

// computeLP prices one epoch from the indexer's liquidity event log.
//
// The epoch's block range is derived arithmetically, NOT via hivesrc.EpochWindow:
// that resolves both boundaries to Hive timestamps, which LP has no use for, and
// would make two Hive round-trips that can fail for no reason. The interval is the
// same closed one the contracts use, [g+ep*el, g+(ep+1)*el-1].
func (a *app) computeLP(ep uint64) (*computed, error) {
	g, el := a.cfg.Epoch.Genesis, a.cfg.Epoch.Len
	start, end, ok := hivesrc.EpochBounds(g, el, ep)
	if !ok {
		return nil, fmt.Errorf("epoch %d is out of range for genesis %d and epochLen %d "+
			"(its block range overflows)", ep, g, el)
	}
	win := hivesrc.Window{Epoch: ep, StartBlock: start, EndBlock: end}
	res, err := lpsrc.LPShares(
		&lpsrc.HTTPTransport{Endpoint: a.cfg.Indexer.API, Secret: a.cfg.Indexer.Secret},
		lpsrc.Options{
			Pool:       a.cfg.Indexer.Pool,
			Start:      win.StartBlock,
			End:        win.EndBlock,
			PageSize:   a.cfg.Indexer.PageSize,
			AllowStale: a.cfg.Indexer.AllowStale,
		})
	if err != nil {
		return nil, err
	}
	if err := sharecore.ValidateAccounts(res); err != nil {
		return nil, fmt.Errorf("refusing to build a report: %w", err)
	}
	canon := sharecore.Canonicalize(res)
	return &computed{
		Epoch:     ep,
		Window:    win,
		Providers: len(res.Shares),
		Result:    res,
		Canon:     canon,
		Pages:     sharecore.Paginate(canon, a.cfg.Page.MaxEntries, a.cfg.Page.MaxBytes),
	}, nil
}

func (a *app) cmdCompute(epochFlag string, asJSON bool) error {
	c, err := a.compute(epochFlag)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"epoch": c.Epoch, "start_block": c.Window.StartBlock, "end_block": c.Window.EndBlock,
			"start_time": c.Window.StartTime, "end_time": c.Window.EndTime,
			"kind": a.cfg.Kind(), "posts": c.Posts, "providers": c.Providers,
			"accounts":     len(c.Result.Shares),
			"total_shares": c.Result.Total.String(),
			"canonical":    c.Canon, "pages": c.Pages,
		})
	}
	if a.cfg.Kind() == SourceLP {
		// No timestamps are resolved in LP mode, so printing them would show a zero
		// time and read as a bug.
		fmt.Printf("epoch %d  blocks %d..%d  (lp)\n", c.Epoch, c.Window.StartBlock, c.Window.EndBlock)
		fmt.Printf("providers %d  total shares %s  pages %d\n",
			c.Providers, c.Result.Total, len(c.Pages))
	} else {
		fmt.Printf("epoch %d  blocks %d..%d  (%s .. %s UTC)\n",
			c.Epoch, c.Window.StartBlock, c.Window.EndBlock,
			c.Window.StartTime.Format(hivesrc.HiveTimeLayout),
			c.Window.EndTime.Format(hivesrc.HiveTimeLayout))
		fmt.Printf("posts %d  accounts %d  total shares %s  pages %d\n",
			c.Posts, len(c.Result.Shares), c.Result.Total, len(c.Pages))
	}
	if len(c.Pages) == 0 {
		fmt.Println("\nnothing to report for this epoch")
		return nil
	}
	fmt.Println()
	for _, p := range c.Pages {
		fmt.Printf("page %d (%d entries, %d bytes)\n  %s\n", p.Index, p.Count, len(p.Entries), p.Entries)
	}
	return nil
}

func (a *app) buildPlan(epochFlag string) (*computed, submit.Plan, error) {
	c, err := a.compute(epochFlag)
	if err != nil {
		return nil, submit.Plan{}, err
	}
	eps := strconv.FormatUint(c.Epoch, 10)
	funder := ""
	if a.cfg.Submit.Keeper {
		funder = a.cfg.Contracts.Funder
	}
	// The commitment the claims verify against. Built from the SAME share set the
	// pages publish, so the leaves and the root cannot disagree.
	tree := sharecore.BuildTree(c.Result.Shares)
	pl := submit.BuildFullPlan(submit.PlanOpts{
		Epoch:         eps,
		Root:          tree.Root(),
		TotalShares:   c.Result.Total.String(),
		Accounts:      len(c.Result.Shares),
		DistributorID: a.cfg.Contracts.Distributor,
		Channel:       a.cfg.Contracts.Channel,
		FunderID:      funder,
		PullFunding:   a.cfg.Submit.PullFunding,
		Finalize:      a.cfg.Submit.Finalize,
		Pages:         c.Pages,
		RcLimit:       a.cfg.Submit.RcLimit,
	})
	return c, pl, nil
}

func (a *app) cmdPlan(epochFlag string, asJSON bool) error {
	c, pl, err := a.buildPlan(epochFlag)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(pl)
	}
	fmt.Printf("epoch %d — %d calls\n\n", c.Epoch, len(pl.Calls))
	for i, call := range pl.Calls {
		body, berr := broadcast.Body(a.cfg.VSC.NetID, call)
		if berr != nil {
			return berr
		}
		fmt.Printf("%2d. %-16s %s\n    %s\n    custom_json vsc.call: %s\n\n",
			i+1, call.Action, call.Note, call.ContractID, body)
	}
	return nil
}

func (a *app) cmdRun(epochFlag string, doBroadcast bool) error {
	c, pl, err := a.buildPlan(epochFlag)
	if err != nil {
		return err
	}
	if len(pl.Calls) == 0 {
		fmt.Println("nothing to do")
		return nil
	}

	prog, err := submit.LoadProgress(a.cfg.Submit.ProgressFile)
	if err != nil {
		return err
	}

	// CHAIN STATE, not the local file, decides what still needs doing.
	//
	// The local file cannot be authoritative: if a pullFunding tx was recorded but
	// never actually took effect (it was included before the C2 poke, so it claimed
	// 0), the epoch stays unfunded, finalizeEpoch keeps aborting, and a file-driven
	// skip would refuse to retry it forever. Every step here is observable on chain
	// and every one is safe to repeat, so the chain is both the safer and the more
	// accurate source. The progress file remains an audit trail and is reported when
	// it disagrees.
	applied, _, err := a.chainApplied(pl)
	if err != nil {
		return fmt.Errorf("could not read epoch state from chain: %w", err)
	}
	var remaining []submit.Call
	for _, call := range pl.Calls {
		key := call.Action + "/" + strconv.Itoa(submit.PageOf(call))
		locallyDone := prog.IsDone(pl.Epoch, call.Action, submit.PageOf(call))
		if applied[key] {
			continue
		}
		if locallyDone {
			fmt.Printf("note: %s was recorded locally but the chain does not show it applied — retrying\n",
				call.Action)
		}
		remaining = append(remaining, call)
	}

	fmt.Printf("epoch %d: %d calls, %d already applied on chain, %d remaining\n",
		c.Epoch, len(pl.Calls), len(pl.Calls)-len(remaining), len(remaining))
	if len(remaining) == 0 {
		fmt.Println("epoch already fully submitted")
		return nil
	}

	// An epoch with no earners must never reach the chain. distributeEpoch and
	// pullFunding would move C2's slice into a distributor that can then never
	// finalize (finalizeEpoch requires totalShares>0), leaving the funding
	// recoverable only by a guardian stale-cancel that pays the treasury rather than
	// any earner. Refuse before the first broadcast, not at the finalize step.
	if planHasFinalize(pl) && countSubmitShares(pl) == 0 {
		return fmt.Errorf("epoch %s has no share entries, so it can never be finalized "+
			"(finalizeEpoch requires totalShares>0). Refusing to fund it: pulling funding into an "+
			"epoch with no earners strands it until a guardian cancels the epoch after "+
			"epochEnd+staleBlocks, which pays the treasury and not the earners", pl.Epoch)
	}

	var b broadcast.Broadcaster
	if doBroadcast {
		hb, herr := broadcast.NewHiveBroadcaster(
			a.cfg.Hive.API, a.cfg.VSC.NetID, a.cfg.Submit.Account, a.cfg.Submit.WifEnv,
			a.cfg.Hive.ChainID)
		if herr != nil {
			return herr
		}
		b = hb
		fmt.Printf("BROADCASTING as %s on %s\n\n", hb.Account, a.cfg.VSC.NetID)
	} else {
		b = &broadcast.DryRun{NetID: a.cfg.VSC.NetID}
		fmt.Printf("DRY RUN (pass -broadcast to send)\n\n")
	}

	sentThisRun := map[string]bool{}
	for _, call := range remaining {
		// FINALIZE IS IRREVERSIBLE. A share page can be broadcast successfully and
		// still revert on L2 — the broadcaster returns the L1 txid and nothing about
		// execution — so reaching this point is NOT evidence the pages applied. If we
		// finalize anyway, the accounts on the surviving pages split the whole epoch
		// and the missing ones can never be added: submitShares aborts once status is
		// set. Re-read the chain and refuse unless every page is confirmed applied.
		if call.Action == "finalizeEpoch" {
			proceed, ferr := a.confirmBeforeFinalize(pl, sentThisRun, doBroadcast)
			if ferr != nil {
				return ferr
			}
			if !proceed {
				return nil
			}
		}
		fmt.Printf("-> %s %s\n", call.Action, call.Payload)
		txid, serr := b.Send(call)
		if serr != nil {
			// Stop at the first failure rather than pressing on: the calls are
			// ORDERED (fund before shares, finalize last), so continuing past a
			// failed step would finalize an epoch that is missing pages.
			return fmt.Errorf("%s failed (stopping; rerun to resume): %w", call.Action, serr)
		}
		if !doBroadcast {
			continue // never record progress for a call that was not sent
		}
		fmt.Printf("   tx %s\n", txid)
		sentThisRun[call.Action+"/"+strconv.Itoa(submit.PageOf(call))] = true
		if merr := prog.MarkDone(pl.Epoch, call.Action, submit.PageOf(call)); merr != nil {
			return fmt.Errorf("call %s landed as %s but progress could not be saved: %w",
				call.Action, txid, merr)
		}
	}
	if doBroadcast {
		// Deliberately "submitted", never "finalized": finalizeEpoch returns
		// {"finalized":false} below an Attest threshold, and the broadcaster cannot
		// see that. Claiming finalization here would be a fresh false all-clear.
		fmt.Printf("\nepoch %d submitted\n", c.Epoch)
	} else {
		fmt.Printf("\ndry run complete — nothing was sent and no progress was recorded\n")
	}
	return nil
}

// chainApplied reports which of a plan's calls the distributor has already taken,
// keyed as "<action>/<page>".
//
// The observable markers are exactly the ones the contracts write:
//
//	funded|<ep>        set by pullFunding (and only by it)
//	ssdone|<ep>|<page> set when a page is APPLIED (not merely attested)
//	status|<ep>        set to finalized/cancelled by finalizeEpoch/cancelEpoch
//
// distributeEpoch has no per-epoch marker on the distributor — it is a keeper poke
// on C2 — so it is treated as done once this epoch is funded, which is the only
// thing the poke is needed for.
// chKey builds a per-CHANNEL distributor state key.
//
// One distributor now serves several reward channels, so every per-epoch key it
// writes carries the channel: funded|<ch>|<ep>, status|<ch>|<ep>,
// ssdone|<ch>|<ep>|<page>, totalShares|<ch>|<ep>. A key built without it addresses
// nothing — getStateByKeys returns null, which reads as "not funded", "not
// finalized", "no pages applied". Every one of those is the SAFE-LOOKING answer,
// which is what made this survive: nothing errors, the reporter just quietly
// believes an epoch is untouched.
//
// Contract-wide config (cfg_genesis, cfg_epochLen, cfg_funder) is NOT channel-scoped
// and must not go through here.
func (a *app) chKey(prefix string, parts ...string) string {
	k := prefix + "|" + a.cfg.Contracts.Channel
	for _, p := range parts {
		k += "|" + p
	}
	return k
}

func (a *app) chainApplied(pl submit.Plan) (map[string]bool, map[string]string, error) {
	// cfg_rMode rides along: the finalize gate needs to know whether unapplied pages
	// are a fault or the normal state of an Attest quorum that has not converged yet.
	keys := []string{a.chKey("funded", pl.Epoch), a.chKey("status", pl.Epoch),
		"ch_rMode|" + a.cfg.Contracts.Channel}
	for _, c := range pl.Calls {
		if c.Action == "submitShares" {
			keys = append(keys, a.chKey("ssdone", pl.Epoch, strconv.Itoa(submit.PageOf(c))))
		}
	}

	state := map[string]string{}
	// getStateByKeys accepts at most 100 keys per call
	for i := 0; i < len(keys); i += 100 {
		end := min(i+100, len(keys))
		chunk, err := a.reader().StateGet(a.cfg.Contracts.Distributor, keys[i:end])
		if err != nil {
			return nil, nil, err
		}
		for k, v := range chunk {
			state[k] = v
		}
	}

	funded := state[a.chKey("funded", pl.Epoch)] != "" && state[a.chKey("funded", pl.Epoch)] != "0"
	finalized := state[a.chKey("status", pl.Epoch)] != ""

	out := map[string]bool{}
	for _, c := range pl.Calls {
		page := submit.PageOf(c)
		k := c.Action + "/" + strconv.Itoa(page)
		switch c.Action {
		case "distributeEpoch", "pullFunding":
			out[k] = funded
		case "submitShares":
			out[k] = state[a.chKey("ssdone", pl.Epoch, strconv.Itoa(page))] != ""
		case "finalizeEpoch":
			out[k] = finalized
		}
	}
	// An epoch that is already finalized or cancelled takes nothing further.
	if finalized {
		for k := range out {
			out[k] = true
		}
	}
	return out, state, nil
}

const (
	// The finalize gate's default patience. Six tries at 15s covers ~90s, which is
	// several VSC blocks — enough for a page broadcast moments earlier to land,
	// without stalling an unattended keeper for long when something is actually wrong.
	defaultConfirmTries    = 6
	defaultConfirmInterval = 15 * time.Second

	// auth.ModeAttest as it appears in contract state (cfg_rMode).
	authModeAttest = "2"

	// defaultLookback bounds the oldest-unfinalized scan to one batched state read.
	defaultLookback = 20
)

func planHasFinalize(pl submit.Plan) bool {
	for _, c := range pl.Calls {
		if c.Action == "finalizeEpoch" {
			return true
		}
	}
	return false
}

func countSubmitShares(pl submit.Plan) int {
	n := 0
	for _, c := range pl.Calls {
		if c.Action == "submitShares" {
			n++
		}
	}
	return n
}

// confirmBeforeFinalize decides whether it is safe to send finalizeEpoch.
//
// WHY THIS EXISTS. Broadcasting is not executing. `b.Send` returns the Hive L1 txid
// and there is no tx-receipt query on L2 anywhere in this codebase — vscapi exposes
// state reads only — so a share page can be accepted by Hive and still revert inside
// the contract (out of RC, a payload the contract rejects, an Attest threshold not
// yet met). finalizeEpoch does not care: it requires only status=="", funded>0 and
// totalShares>0. So a run in which four of five pages reverted finalizes happily, the
// accounts on the surviving page split 100% of the epoch, and the other four pages
// can never be added because submitShares aborts once status is set. The money is
// then recoverable only by a guardian cancelling inside the challenge window.
//
// `ssdone|<ep>|<page>` is the receipt we need: the contract writes it only on the
// committed path, so its presence is proof of application rather than of submission.
// chainApplied already reads exactly those keys — the entire bug was that it ran once
// before the send loop and never again.
//
// Returns (true, nil) to proceed, (false, nil) to stop cleanly without finalizing,
// or an error. A read failure is an ERROR, never treated as confirmation: this gate
// exists precisely for the case where we cannot see what happened.
func (a *app) confirmBeforeFinalize(pl submit.Plan, sentThisRun map[string]bool, doBroadcast bool) (bool, error) {
	if !doBroadcast {
		fmt.Println("   (dry run) finalize gate not evaluated — nothing was sent")
		return true, nil
	}
	tries := a.cfg.Submit.ConfirmTries
	if tries <= 0 {
		tries = defaultConfirmTries
	}
	interval := time.Duration(a.cfg.Submit.ConfirmIntervalSec) * time.Second
	if interval <= 0 {
		interval = defaultConfirmInterval
	}

	var missing []int
	lastMode := ""
	lastFunded := false
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			time.Sleep(interval)
		}
		applied, state, err := a.chainApplied(pl)
		if err != nil {
			return false, fmt.Errorf("cannot confirm share pages before finalizing epoch %s: %w "+
				"(refusing to finalize on an unverified report)", pl.Epoch, err)
		}
		lastMode = state["ch_rMode|"+a.cfg.Contracts.Channel]
		// Someone else finalized or cancelled it while we were working.
		if st := state[a.chKey("status", pl.Epoch)]; st != "" {
			fmt.Printf("   epoch %s is already %s on chain — not finalizing again\n", pl.Epoch, st)
			return false, nil
		}
		missing = missing[:0]
		for _, c := range pl.Calls {
			if c.Action != "submitShares" {
				continue
			}
			if !applied[c.Action+"/"+strconv.Itoa(submit.PageOf(c))] {
				missing = append(missing, submit.PageOf(c))
			}
		}
		fundedOK := state[a.chKey("funded", pl.Epoch)] != "" && state[a.chKey("funded", pl.Epoch)] != "0"
		lastFunded = fundedOK
		if len(missing) == 0 && fundedOK {
			return true, nil
		}
		if !fundedOK {
			fmt.Printf("   waiting: epoch %s is not funded yet\n", pl.Epoch)
			continue
		}
		fmt.Printf("   waiting: %d/%d share pages applied on chain (missing %v)\n",
			countSubmitShares(pl)-len(missing), countSubmitShares(pl), missing)
	}

	// Still unconfirmed. Do NOT finalize and do NOT record progress — this run simply
	// stops, and the next one resumes from chain state.
	//
	// Whether that is alarming depends on WHO is missing. A page this run broadcast
	// may just be slow, and in Attest mode a page can be legitimately unapplied
	// because the other attesters have not signed it yet — that is the normal state
	// for every attester but the last, so treating it as an error would make the
	// alarm fire on every healthy run and be ignored.
	// UNFUNDED is a different diagnosis from MISSING PAGES, and conflating them sends
	// the operator hunting for a page problem that does not exist. The common cause is
	// an exhausted emission pool: distributeEpoch then SUCCEEDS while distributing
	// nothing ({"distributed":"0","starved":true}), pullFunding aborts because C2 owes
	// this epoch nothing, and the pages still apply — so everything looks fine except
	// that no money ever arrived.
	if !lastFunded {
		return false, fmt.Errorf("refusing to finalize epoch %s: it is NOT FUNDED. The share pages "+
			"may be fine; what is missing is the money. Most often the emission pool is exhausted or "+
			"its allowance was revoked, in which case distributeEpoch succeeds while distributing "+
			"nothing and pullFunding has nothing to claim. Check the pool holder's balance and its "+
			"allowance to the funder contract, then re-run", pl.Epoch)
	}

	ours := false
	for _, p := range missing {
		if sentThisRun["submitShares/"+strconv.Itoa(p)] {
			ours = true
			break
		}
	}
	attesting := lastMode == authModeAttest
	switch {
	case ours:
		fmt.Printf("\nfinalize deferred to the next run: %d of %d pages are not applied on chain yet "+
			"(missing %v). They were broadcast in this run and may still be landing.\n",
			len(missing), countSubmitShares(pl), missing)
		return false, nil
	case attesting:
		fmt.Printf("\nfinalize deferred: %d of %d pages have not reached the attestation threshold "+
			"(missing %v). This is expected until enough reporters have signed them.\n",
			len(missing), countSubmitShares(pl), missing)
		return false, nil
	default:
		return false, fmt.Errorf("refusing to finalize epoch %s: %d of %d share pages are NOT applied "+
			"on chain (missing %v) and were not sent in this run, so they reverted rather than being "+
			"slow. Finalizing now would permanently pay the whole epoch to the accounts on the pages "+
			"that did land — submitShares aborts once the epoch is finalized. Investigate (RC limit, "+
			"page size) and re-run",
			pl.Epoch, len(missing), countSubmitShares(pl), missing)
	}
}

// cmdProof prints what an account needs in order to claim: its share and the sibling
// path proving that share is in the epoch's committed tree.
//
// THIS IS THE OTHER HALF OF THE MERKLE TRADE. The chain holds a 32-byte root and no
// per-account state, so "what am I owed?" is no longer a question the chain can
// answer — something has to recompute the epoch and hand the claimant a proof. That
// is what this does, and it is why determinism matters so much: anyone can run it,
// from public Hive data, and get a proof that verifies against a root they did not
// have to trust.
//
// A wallet would normally read the share book from the indexer, which holds the leaf
// logs. This path needs no indexer at all — it recomputes from Hive — which makes it
// the fallback when the indexer is unavailable and the check that the indexer's copy
// is honest.
func (a *app) cmdProof(epochFlag, account string, asJSON bool) error {
	if account == "" {
		return fmt.Errorf("-account is required for proof")
	}
	if !strings.Contains(account, ":") {
		account = "hive:" + account
	}
	c, err := a.compute(epochFlag)
	if err != nil {
		return err
	}
	tree := sharecore.BuildTree(c.Result.Shares)
	proof, ok := tree.Proof(account)
	if !ok {
		return fmt.Errorf("%s earned nothing in epoch %d — there is no proof to give",
			account, c.Epoch)
	}
	share := c.Result.Shares[account].String()
	// Check our own output before handing it over: a proof that does not verify is
	// worse than no proof, because the claimant burns RC discovering it.
	if !sharecore.VerifyProof(account, share, proof, tree.Root()) {
		return fmt.Errorf("internal: generated proof for %s does not verify against the root", account)
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"epoch": c.Epoch, "account": account, "share": share,
			"root": tree.Root(), "proof": proof,
			"claim_payload": fmt.Sprintf(`{"channel":"%s","epoch":"%d","share":"%s","proof":"%s"}`,
				a.cfg.Contracts.Channel, c.Epoch, share, strings.Join(proof, ",")),
		})
	}
	fmt.Printf("epoch %d  %s\n", c.Epoch, account)
	fmt.Printf("  share  %s\n", share)
	fmt.Printf("  root   %s\n", tree.Root())
	fmt.Printf("  proof  %d siblings\n", len(proof))
	fmt.Printf("\nclaim payload:\n%s\n", fmt.Sprintf(
		`{"channel":"%s","epoch":"%d","share":"%s","proof":"%s"}`,
		a.cfg.Contracts.Channel, c.Epoch, share, strings.Join(proof, ",")))
	return nil
}

// cmdRoot computes the merkle commitment for a share list supplied on the command
// line, and optionally a proof for one account.
//
// WHY THIS EXISTS. Not every share book comes out of the Hive pipeline. An operator
// pushing a list by hand — an LP channel fed from their own numbers, a one-off
// correction, a migration — still has to commit a root, and finalizeEpoch refuses an
// epoch without one. Before this there was no way to compute that root outside the
// reporter's own epoch machinery, which made direct submission impossible rather than
// merely manual.
//
// It takes the SAME "acct:share,acct:share" string submitShares takes, so the root
// provably covers what was published. Entries whose account carries no ledger domain
// are dropped exactly as the contract drops them — a root over a set the chain would
// not accept is a root no claim can use.
func (a *app) cmdRoot(entries, account string, asJSON bool) error {
	if entries == "" {
		return fmt.Errorf("-entries is required, in the form \"hive:a:100,hive:b:50\"")
	}
	// The contract's own rules, from sharecore — not a second copy. The copy that
	// used to live here had already drifted: it rejected the system: domain the
	// contract accepts, so such an entry was counted on chain and left out of the
	// root committed for it.
	shares, dropped := sharecore.ParseEntries(entries)
	skipped := []string{}
	for _, d := range dropped {
		skipped = append(skipped, d.Raw+" ("+d.Reason+")")
	}
	if len(shares) == 0 {
		return fmt.Errorf("no usable entries: %s", strings.Join(skipped, "; "))
	}
	tree := sharecore.BuildTree(shares)
	total := sharecore.TotalOf(shares)

	out := map[string]any{
		"root": tree.Root(), "total_shares": total.String(),
		"accounts": len(shares), "skipped": skipped,
	}
	if account != "" {
		if !strings.Contains(account, ":") {
			account = "hive:" + account
		}
		proof, ok := tree.Proof(account)
		if !ok {
			return fmt.Errorf("%s is not in this share list", account)
		}
		out["account"] = account
		out["share"] = shares[account].String()
		out["proof"] = proof
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("root          %s\n", tree.Root())
	fmt.Printf("total_shares  %s\n", total)
	fmt.Printf("accounts      %d\n", len(shares))
	for _, sk := range skipped {
		fmt.Printf("SKIPPED       %s\n", sk)
	}
	return nil
}
