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
  policy-digest -config F  digest of the settings that decide the share book —
                           pass to addChannel/setPolicy, and compare across the
                           reporters of an Attest quorum (differing = refused)

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
		// Accepted and unused. This subcommand is pure arithmetic over the list
		// given, so a config has nothing to contribute — but every other
		// subcommand takes one, and scripts and test harnesses pass it
		// uniformly. Rejecting it here made a caller fail on a flag that could
		// not have changed the answer.
		fsRoot.String("config", "", "accepted for uniformity; unused — the entries are supplied directly")
		if err := fsRoot.Parse(rest); err != nil {
			return err
		}
		return (&app{cfg: &Config{}}).cmdRoot(*entries, *acct, *asJSONRoot)
	case "epoch", "compute", "plan", "run", "proof", "policy-digest":
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

	// Before newApp: this is the command an operator runs to get the value for
	// addChannel/setPolicy, and to diff two reporters against each other. Needing a
	// reachable chain to compute a hash of a local file would be a nuisance exactly
	// when the chain is what you are trying to configure.
	if cmd == "policy-digest" {
		return cmdPolicyDigest(cfg, *asJSON)
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
	// reader(), not a.vsc: every other chain read in this file goes through it, and
	// bypassing it here made the epoch selector the one path a test could not stub.
	state, err := a.reader().StateGet(a.cfg.Contracts.Distributor, keys)
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

// nextUnfinalizedAfter returns the first unfinalized epoch strictly after `ep`,
// still inside the lookback window.
//
// This is what lets a run step OVER an epoch it cannot work. An epoch with no
// earners computes to an empty book, and cmdRun refuses it — correctly, since
// finalizeEpoch requires totalShares>0 and funding an epoch that can never finalize
// strands it. But nothing sets `status` for such an epoch, so oldestUnfinalized kept
// returning the same one every run and every LATER epoch — with real earners and
// real funding — went unlooked-at until the empty one fell below the window floor.
// One quiet week at launch froze the channel for up to `lookback` epochs.
//
// An empty epoch cannot be finalized by anyone; only a guardian cancelEpoch resolves
// it. So the reporter's job is to step past it and keep working, having said so.
func (a *app) nextUnfinalizedAfter(ep, latest uint64) (uint64, error) {
	if ep >= latest {
		return 0, fmt.Errorf("no epoch after %d is closed yet (head epoch is %d): "+
			"nothing further to work in this window", ep, latest)
	}
	first := ep + 1
	keys := make([]string, 0, latest-first+1)
	for e := first; e <= latest; e++ {
		keys = append(keys, a.chKey("status", strconv.FormatUint(e, 10)))
	}
	state, err := a.reader().StateGet(a.cfg.Contracts.Distributor, keys)
	if err != nil {
		return 0, err
	}
	for e := first; e <= latest; e++ {
		if state[a.chKey("status", strconv.FormatUint(e, 10))] == "" {
			return e, nil
		}
	}
	return 0, fmt.Errorf("every epoch after %d up to %d is already finalized or "+
		"cancelled — nothing left to work", ep, latest)
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
	problems = append(problems, a.verifyPolicy()...)
	return problems
}

// verifyPolicy compares this reporter's book-affecting settings against the digest
// the channel declared on chain.
//
// The three checks above cover genesis, epochLen and role. Everything else that
// changes the numbers — tags, exclusions, the dust cutoff, the reward curves, the
// vote-mana budget, the app tax, pagination — was compared against nothing, so two
// HONEST reporters differing in one setting produced different books forever and
// neither could tell. In Attest mode that is a deadlock, not a wrong payout: each
// authority has one vote per action and they burn it in different buckets.
//
// This is the check that makes that impossible rather than merely detectable. It
// runs before anything is computed, so a divergent reporter spends no RC and
// submits nothing; the contract's own check at submitRoot is the backstop for a
// reporter that skipped this one.
func (a *app) verifyPolicy() []string {
	mine, err := a.cfg.PolicyDigest()
	if err != nil {
		return []string{fmt.Sprintf("could not compute the policy digest of this config: %v", err)}
	}
	got, err := a.reader().StateGet(a.cfg.Contracts.Distributor,
		[]string{"ch_policy|" + a.cfg.Contracts.Channel})
	if err != nil {
		return []string{fmt.Sprintf("could not read the channel policy: %v", err)}
	}
	want := strings.TrimSpace(got["ch_policy|"+a.cfg.Contracts.Channel])
	if want == "" {
		// No declared policy. Legitimate for a single reporter — there is nobody to
		// disagree with — and impossible for Attest, which addChannel refuses without
		// one. Silence here rather than a warning on every run.
		return nil
	}
	if want != mine {
		return []string{fmt.Sprintf(
			"POLICY MISMATCH: channel %q declares policy %s but this config hashes to %s.\n"+
				"    Some setting that decides the share book differs from what the channel\n"+
				"    was configured with — tags, excluded tags, excluded or muted accounts,\n"+
				"    min_share_bps, the author/curation curves, author_reward_bps, cashout_days,\n"+
				"    downvote or declined-payout handling, the vote-mana settings, the app tax,\n"+
				"    or page limits. Refusing to report: submitting now would score this epoch\n"+
				"    by different rules than the other reporters, which in Attest mode deadlocks\n"+
				"    the epoch rather than paying anyone wrongly.\n"+
				"    Run `reporter policy-digest` on each reporter and diff the configs.",
			a.cfg.Contracts.Channel, want, mine)}
	}
	return nil
}

// verifyEpochPolicy checks this config against the digest pinned to ONE epoch.
//
// pullFunding snapshots the channel policy into policy|<ch>|<ep> the first time an
// epoch is funded, so an epoch is permanently scored against the rules in force
// when it was funded rather than whatever governance has changed to since. An
// unreported backlog epoch therefore needs the config it was funded under, not the
// current one — and this is where an operator finds that out, rather than at
// submitRoot after the whole book has been computed.
func (a *app) verifyEpochPolicy(ep uint64) error {
	key := "policy|" + a.cfg.Contracts.Channel + "|" + strconv.FormatUint(ep, 10)
	got, err := a.reader().StateGet(a.cfg.Contracts.Distributor, []string{key})
	if err != nil {
		return fmt.Errorf("could not read the pinned policy for epoch %d: %w", ep, err)
	}
	want := strings.TrimSpace(got[key])
	if want == "" {
		return nil // not funded yet, or a channel that declared no policy
	}
	mine, err := a.cfg.PolicyDigest()
	if err != nil {
		return fmt.Errorf("could not compute the policy digest of this config: %w", err)
	}
	if want != mine {
		return fmt.Errorf(
			"refusing to compute: epoch %d was funded under policy %s but this config hashes "+
				"to %s.\n    An epoch is scored against the policy pinned when it was funded, so "+
				"that it stays\n    reproducible after governance changes anything. If policy "+
				"changed while this epoch\n    was outstanding, report it with the configuration "+
				"it was funded under.", ep, want, mine)
	}
	return nil
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

// computeForProof is compute WITHOUT the submit-path policy gates.
//
// Those gates exist to stop a divergent reporter SUBMITTING, and they are right for
// that. Applied to a read-only proof they lock earners out: after a setPolicy from
// digest A to B, an epoch funded under A satisfies NEITHER config — the old one fails
// verifyPolicy (A != current B), the new one fails verifyEpochPolicy (B != pin A). So
// `reporter proof` refused every pre-change epoch, and both docs/how-it-works.md and
// reporter/README.md promise it as the path that "needs no indexer at all… so you are
// never locked out by someone else's server being down". Combined with proofsvc
// being down, that left no route at all to a proof.
//
// What replaces the gates is a STRICTER check, in cmdProof: the computed book must
// reproduce the epoch's COMMITTED ROOT. A policy digest is a proxy for "did this
// reporter score the epoch the agreed way"; the root is the thing itself. If the root
// matches, the proof will verify on chain whatever the digests say. If it does not,
// no digest could have made the proof good.
func (a *app) computeForProof(epochFlag string) (*computed, error) {
	return a.computeInner(epochFlag, false)
}

func (a *app) compute(epochFlag string) (*computed, error) {
	return a.computeInner(epochFlag, true)
}

func (a *app) computeInner(epochFlag string, enforcePolicy bool) (*computed, error) {
	ep, _, err := a.resolveEpoch(epochFlag)
	if err != nil {
		return nil, err
	}
	if problems := a.verifyChainConfig(); enforcePolicy && len(problems) > 0 {
		return nil, fmt.Errorf("refusing to compute: %s", strings.Join(problems, "; "))
	}
	// The epoch may have been funded under an OLDER policy than the channel now
	// declares — governance is allowed to change it, and pullFunding pins whatever
	// was in force to the epoch. That pin is what this epoch must be scored against
	// for the rest of its life, so it outranks the channel-level check above.
	if enforcePolicy {
		if err := a.verifyEpochPolicy(ep); err != nil {
			return nil, err
		}
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
	// Declared with the root so the contract can refuse a root scored under
	// different rules. compute() has already checked this against the epoch's pin,
	// so a mismatch here means the chain changed underneath the run.
	pol, err := a.cfg.PolicyDigest()
	if err != nil {
		return nil, submit.Plan{}, fmt.Errorf("could not compute the policy digest: %w", err)
	}
	opts := submit.PlanOpts{
		Epoch:         eps,
		Root:          tree.Root(),
		Policy:        pol,
		TotalShares:   c.Result.Total.String(),
		Accounts:      len(c.Result.Shares),
		DistributorID: a.cfg.Contracts.Distributor,
		Channel:       a.cfg.Contracts.Channel,
		FunderID:      funder,
		PullFunding:   a.cfg.Submit.PullFunding,
		Finalize:      a.cfg.Submit.Finalize,
		Pages:         c.Pages,
		RcLimit:       a.cfg.Submit.RcLimit,
	}
	// Refuse a plan that cannot succeed before spending a single RC on it. A run
	// that publishes pages and then dies at finalize has already paid for the
	// pages and pulled the funding.
	if err := opts.Validate(); err != nil {
		return nil, submit.Plan{}, err
	}
	pl := submit.BuildFullPlan(opts)
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

// selectWorkableEpoch builds a plan, stepping OVER any auto-selected epoch that has
// no earners.
//
// An epoch with no qualifying posts computes to an empty book, and it must never
// reach the chain: distributeEpoch and pullFunding would move C2's slice into a
// distributor that can then never finalize (finalizeEpoch requires totalShares>0),
// leaving the funding recoverable only by a guardian stale-cancel that pays the
// treasury rather than any earner.
//
// Refusing was right; STOPPING there was not. Nothing sets `status` for such an
// epoch, so oldestUnfinalized returned the same one on every subsequent run and every
// later epoch — with real earners and real funding — was never looked at, because a
// run handles exactly one epoch. It only cleared when the empty epoch fell below the
// window floor: up to `lookback` (default 20) epochs of frozen payouts from a single
// quiet week, which is guaranteed at launch and routine for a small tribe.
//
// An empty epoch cannot be finalized by anyone, so the reporter cannot resolve it —
// only a guardian cancelEpoch can. What it CAN do is say so and get on with the
// epochs it can work.
//
// An EXPLICIT -epoch is never stepped over: the operator named that epoch, and
// silently working a different one would be worse than refusing.
func (a *app) selectWorkableEpoch(epochFlag string) (*computed, submit.Plan, error) {
	c, pl, err := a.buildPlan(epochFlag)
	if err != nil {
		return nil, submit.Plan{}, err
	}
	if epochFlag != "" || !planHasFinalize(pl) || countSubmitShares(pl) > 0 {
		return c, pl, nil
	}

	_, head, err := a.resolveEpoch("")
	if err != nil {
		return nil, submit.Plan{}, err
	}
	latest, err := hivesrc.LatestClosedEpoch(a.cfg.Epoch.Genesis, a.cfg.Epoch.Len, head)
	if err != nil {
		return nil, submit.Plan{}, err
	}

	// Bounded by the window: nextUnfinalizedAfter never returns an epoch past
	// `latest`, so this cannot spin.
	for {
		ep, perr := strconv.ParseUint(pl.Epoch, 10, 64)
		if perr != nil {
			return nil, submit.Plan{}, perr
		}
		fmt.Printf("epoch %d has no share entries, so it can never be finalized "+
			"(finalizeEpoch requires totalShares>0). NOT funding it — pulling funding into an "+
			"epoch with no earners strands it until a guardian cancels it after "+
			"epochEnd+staleBlocks, which pays the treasury and not the earners.\n"+
			"  A guardian should cancelEpoch %d; skipping to the next open epoch.\n", ep, ep)

		next, nerr := a.nextUnfinalizedAfter(ep, latest)
		if nerr != nil {
			return nil, submit.Plan{}, fmt.Errorf("epoch %d has no earners and %w", ep, nerr)
		}
		c, pl, err = a.buildPlan(strconv.FormatUint(next, 10))
		if err != nil {
			return nil, submit.Plan{}, err
		}
		if !planHasFinalize(pl) || countSubmitShares(pl) > 0 {
			return c, pl, nil
		}
	}
}

// Hive's per-account custom_op limit is 5 per block. Stay one under it so a
// retried or duplicated broadcast cannot push a batch over the edge, and wait a
// little more than a block so a slow head does not fold two batches into one.
const (
	customOpsPerBlock = 4
	hiveBlockInterval = 3500 * time.Millisecond
)

// pace is a variable so a test can observe the waiting without spending it.
var pace = time.Sleep

func (a *app) cmdRun(epochFlag string, doBroadcast bool) error {
	c, pl, err := a.selectWorkableEpoch(epochFlag)
	if err != nil {
		return err
	}
	if len(pl.Calls) == 0 {
		fmt.Println("nothing to do")
		return nil
	}

	// Before anything is broadcast: if this epoch already carries a committed root,
	// this run's book must reproduce it. Otherwise a resume splices two books and the
	// epoch can never satisfy the on-chain pagesum guard.
	if err := a.assertBookMatchesChain(pl); err != nil {
		return err
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
	// Hive caps custom_json operations per ACCOUNT per BLOCK, and a real share book
	// is far more calls than that cap: 1,504 accounts came to 26 pages, and even the
	// sizing in docs/rc-costs.md (500 earners -> 9 pages) needs 13 calls with the
	// keeper poke, the pull, the root and the finalize. Broadcasting them back to
	// back put five in one block and the sixth was rejected by the chain itself:
	//
	//   Assert Exception: insert_info.first->second <= HIVE_CUSTOM_OP_BLOCK_LIMIT
	//
	// Nothing was lost — the run stops at the first failure and resume picks it up —
	// but an operator had to notice an opaque consensus assert and re-run by hand,
	// repeatedly, to land one epoch. Pace instead: stay a call under the cap, then
	// wait out a block before the next batch.
	sentInBlock := 0
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
		if doBroadcast && sentInBlock >= customOpsPerBlock {
			pace(hiveBlockInterval)
			sentInBlock = 0
		}
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
		sentInBlock++
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
//	root|<ep>          set when the share-book root is COMMITTED (same rule)
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

// assertBookMatchesChain refuses a run whose book is not the one the epoch already
// committed.
//
// buildPlan recomputes and repaginates the whole book every run, while chainApplied
// decides a page is done purely from ssdone|<ch>|<ep>|<pageIndex>. Nothing compared
// the two, so if the book changed between a partial run and its resume — a
// repagination, or a load-balanced api.hive.blog backend returning one more post —
// the indices already marked done were treated as satisfying the NEW pagination and a
// contiguous middle slice of the canonical list was never published.
//
// On chain that is terminal: applyEntries accumulates pagesum from what actually
// landed while submitRoot recorded the full declared totalShares, so finalizeEpoch
// fails its pagesum guard forever and submitRoot is immutable, so the declared total
// can never be corrected. Recovery is a guardian stale-cancel, which pays the
// treasury rather than the earners.
//
// And it was invisible: broadcast.Send returns an L1 txid and no L2 receipt is read
// anywhere, so the run recorded progress and printed "epoch N submitted" every time.
//
// The committed root IS the epoch's identity — it is the commitment every claim
// verifies against. If one is on chain and this run's book does not reproduce it,
// they are different books and there is nothing safe to resume.
func (a *app) assertBookMatchesChain(pl submit.Plan) error {
	mine := ""
	for _, c := range pl.Calls {
		if c.Action == "submitRoot" {
			var q struct {
				Root string `json:"root"`
			}
			if err := json.Unmarshal([]byte(c.Payload), &q); err != nil {
				return fmt.Errorf("the plan's submitRoot payload does not parse: %w", err)
			}
			mine = q.Root
			break
		}
	}
	if mine == "" {
		return nil // no commitment in this plan; nothing to compare
	}
	key := a.chKey("root", pl.Epoch)
	st, err := a.reader().StateGet(a.cfg.Contracts.Distributor, []string{key})
	if err != nil {
		return err
	}
	onChain := strings.TrimSpace(st[key])
	if onChain == "" {
		return nil // first run for this epoch
	}
	// Case-insensitive: the contract stores lowercase, and refusing an identical book
	// over a rendering difference would be a false alarm on the one check that exists
	// to prevent a false negative.
	if !strings.EqualFold(onChain, mine) {
		return fmt.Errorf("epoch %s already committed root %s but this run computes %s — "+
			"the book has changed since the last run (a repagination, or a different set of "+
			"posts from the Hive API). Resuming would publish THIS book's pages against THAT "+
			"book's root: the pages already applied stay, the missing slice is never "+
			"published, and finalizeEpoch can then never satisfy its pagesum guard. The root "+
			"is immutable, so this is not recoverable by re-running. Reproduce the original "+
			"book (same config, same source data) or have a guardian cancel the epoch",
			pl.Epoch, onChain, mine)
	}
	return nil
}

func (a *app) chainApplied(pl submit.Plan) (map[string]bool, map[string]string, error) {
	// cfg_rMode rides along: the finalize gate needs to know whether unapplied pages
	// are a fault or the normal state of an Attest quorum that has not converged yet.
	keys := []string{a.chKey("funded", pl.Epoch), a.chKey("status", pl.Epoch),
		a.chKey("root", pl.Epoch), "ch_rMode|" + a.cfg.Contracts.Channel}
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
		case "submitRoot":
			// The root is immutable once set, so its presence is the whole story: the
			// contract aborts a second submitRoot outright. Safe as a bare presence
			// check ONLY because assertBookMatchesChain already ran on this path and
			// refused the run if the committed root were not this plan's — see the
			// note there. Written by set(rk, root) inside submitRoot's `if committed`
			// block, the same place a page's ssdone| is written, so under Attest this
			// appears when the quorum converges and not when this reporter voted.
			out[k] = state[a.chKey("root", pl.Epoch)] != ""
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

// assertRootMatchesChain refuses to hand over a proof from a book the chain did not
// commit. An empty on-chain root means the epoch has no commitment yet, so there is
// nothing to claim against and nothing to compare.
func (a *app) assertRootMatchesChain(ep uint64, mine string) error {
	key := a.chKey("root", strconv.FormatUint(ep, 10))
	st, err := a.reader().StateGet(a.cfg.Contracts.Distributor, []string{key})
	if err != nil {
		return fmt.Errorf("could not read the committed root for epoch %d: %w", ep, err)
	}
	onChain := strings.TrimSpace(st[key])
	if onChain == "" {
		return fmt.Errorf("epoch %d has no committed root yet, so there is nothing to prove "+
			"against — a claim would be refused", ep)
	}
	if !strings.EqualFold(onChain, mine) {
		return fmt.Errorf("epoch %d committed root %s but this config computes %s.\n"+
			"    A proof from this book would not verify on chain. The epoch was scored with "+
			"different\n    settings than this config carries — if policy changed since, use "+
			"the configuration\n    the epoch was funded under.", ep, onChain, mine)
	}
	return nil
}

func (a *app) cmdProof(epochFlag, account string, asJSON bool) error {
	if account == "" {
		return fmt.Errorf("-account is required for proof")
	}
	if !strings.Contains(account, ":") {
		account = "hive:" + account
	}
	c, err := a.computeForProof(epochFlag)
	if err != nil {
		return err
	}
	tree := sharecore.BuildTree(c.Result.Shares)
	// The book must reproduce what the chain committed. This is what makes skipping
	// the policy gates above safe — and it is a check the proof path never had, so a
	// divergent book used to yield a proof that verified locally and then failed at
	// the point of payment, which is the worst place to find out.
	if err := a.assertRootMatchesChain(c.Epoch, tree.Root()); err != nil {
		return err
	}
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
