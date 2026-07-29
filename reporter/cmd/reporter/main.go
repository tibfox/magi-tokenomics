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
	case "epoch", "compute", "plan", "run":
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
	}
	return nil
}

// ---- app -----------------------------------------------------------------

type app struct {
	cfg  *Config
	hive hivesrc.Transport
	vsc  *vscapi.Client
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
		end := a.cfg.Epoch.Genesis + (ep+1)*a.cfg.Epoch.Len - 1
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
	const lookback = 20 // one batched state read; see Epoch.Lookback note in config

	first := uint64(0)
	if latest >= lookback {
		first = latest - lookback + 1
	}
	keys := make([]string, 0, latest-first+1)
	for ep := first; ep <= latest; ep++ {
		keys = append(keys, "status|"+strconv.FormatUint(ep, 10))
	}
	state, err := a.vsc.StateGet(a.cfg.Contracts.Distributor, keys)
	if err != nil {
		return 0, err
	}
	for ep := first; ep <= latest; ep++ {
		if state["status|"+strconv.FormatUint(ep, 10)] == "" {
			return ep, nil
		}
	}
	// Everything in the window is finalized/cancelled. Return the latest closed
	// epoch so the caller reports "already fully submitted" rather than an error.
	return latest, nil
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
	got, err := a.vsc.StateGet(a.cfg.Contracts.Distributor, []string{"cfg_genesis", "cfg_epochLen", "cfg_funder"})
	if err != nil {
		return []string{fmt.Sprintf("could not read distributor state: %v", err)}
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
	}

	// Show where the target epoch actually stands on chain, so an operator can see
	// whether it still needs funding, shares or finalizing.
	if targetErr == nil {
		eps := strconv.FormatUint(target, 10)
		st, err := a.vsc.StateGet(a.cfg.Contracts.Distributor,
			[]string{"status|" + eps, "funded|" + eps, "totalShares|" + eps, "chal|" + eps})
		if err == nil {
			fmt.Printf("\nepoch %s on chain:\n", eps)
			fmt.Printf("  status       %s\n", orDash(st["status|"+eps], "open"))
			fmt.Printf("  funded       %s\n", orDash(st["funded|"+eps], "0"))
			fmt.Printf("  totalShares  %s\n", orDash(st["totalShares|"+eps], "0"))
			if c := st["chal|"+eps]; c != "" {
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
		Tag:             a.cfg.Source.Tag,
		Limit:           a.cfg.Source.Limit,
		Mode:            hivesrc.WeightMode(a.cfg.Source.Weight),
		SnapshotHeight:  win.EndBlock,
		ExcludeAccounts: a.cfg.Source.Exclude,
		Attribution:     hivesrc.Attribution(a.cfg.Source.Attribution),
		Since:           win.StartTime,
		Until:           win.EndTime,
	}
	if opt.Attribution == hivesrc.AttributeCashout {
		// The epoch is defined by PAYOUT time, but the feed can only be paged by
		// creation time — so walk the window shifted back one payout period and let
		// PayoutSince/Until decide actual membership. The margin absorbs posts whose
		// payout_at is not exactly created+7d.
		const margin = time.Hour
		opt.PayoutSince, opt.PayoutUntil = win.StartTime, win.EndTime
		opt.Since = win.StartTime.Add(-hivesrc.PayoutPeriod - margin)
		opt.Until = win.EndTime.Add(-hivesrc.PayoutPeriod + margin)
	}
	if opt.Mode == hivesrc.WeightTokenStake {
		opt.Stake = vscapi.NewStakeSource(a.vsc, a.cfg.Contracts.Stake)
	}

	posts, err := hivesrc.Collect(a.hive, opt)
	if err != nil {
		return nil, err
	}
	res := sharecore.ComputeShares(posts, a.cfg.ShareConfig())
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
	win := hivesrc.Window{
		Epoch:      ep,
		StartBlock: g + ep*el,
		EndBlock:   g + (ep+1)*el - 1,
	}
	res, err := lpsrc.LPShares(
		&lpsrc.HTTPTransport{Endpoint: a.cfg.Indexer.API, Secret: a.cfg.Indexer.Secret},
		lpsrc.Options{
			Pool:     a.cfg.Indexer.Pool,
			Start:    win.StartBlock,
			End:      win.EndBlock,
			PageSize: a.cfg.Indexer.PageSize,
		})
	if err != nil {
		return nil, err
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
	pl := submit.BuildFullPlan(submit.PlanOpts{
		Epoch:         eps,
		DistributorID: a.cfg.Contracts.Distributor,
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
	applied, err := a.chainApplied(pl)
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

	var b broadcast.Broadcaster
	if doBroadcast {
		hb, herr := broadcast.NewHiveBroadcaster(
			a.cfg.Hive.API, a.cfg.VSC.NetID, a.cfg.Submit.Account, a.cfg.Submit.WifEnv)
		if herr != nil {
			return herr
		}
		b = hb
		fmt.Printf("BROADCASTING as %s on %s\n\n", hb.Account, a.cfg.VSC.NetID)
	} else {
		b = &broadcast.DryRun{NetID: a.cfg.VSC.NetID}
		fmt.Printf("DRY RUN (pass -broadcast to send)\n\n")
	}

	for _, call := range remaining {
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
		if merr := prog.MarkDone(pl.Epoch, call.Action, submit.PageOf(call)); merr != nil {
			return fmt.Errorf("call %s landed as %s but progress could not be saved: %w",
				call.Action, txid, merr)
		}
	}
	if doBroadcast {
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
func (a *app) chainApplied(pl submit.Plan) (map[string]bool, error) {
	keys := []string{"funded|" + pl.Epoch, "status|" + pl.Epoch}
	for _, c := range pl.Calls {
		if c.Action == "submitShares" {
			keys = append(keys, fmt.Sprintf("ssdone|%s|%d", pl.Epoch, submit.PageOf(c)))
		}
	}

	state := map[string]string{}
	// getStateByKeys accepts at most 100 keys per call
	for i := 0; i < len(keys); i += 100 {
		end := min(i+100, len(keys))
		chunk, err := a.vsc.StateGet(a.cfg.Contracts.Distributor, keys[i:end])
		if err != nil {
			return nil, err
		}
		for k, v := range chunk {
			state[k] = v
		}
	}

	funded := state["funded|"+pl.Epoch] != "" && state["funded|"+pl.Epoch] != "0"
	finalized := state["status|"+pl.Epoch] != ""

	out := map[string]bool{}
	for _, c := range pl.Calls {
		page := submit.PageOf(c)
		k := c.Action + "/" + strconv.Itoa(page)
		switch c.Action {
		case "distributeEpoch", "pullFunding":
			out[k] = funded
		case "submitShares":
			out[k] = state[fmt.Sprintf("ssdone|%s|%d", pl.Epoch, page)] != ""
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
	return out, nil
}
