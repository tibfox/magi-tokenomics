// magi-setup turns a deployment description into a checked, ordered plan.
//
//	magi-setup template > deploy.json   a filled-in starting point
//	magi-setup check -plan deploy.json  refuse every mistake, before any fee is paid
//	magi-setup steps -plan deploy.json  the ordered sequence, with WHY each step is there
//
// `check` is the reason this exists. The seven ordering constraints in the README
// are all real and all documented, and the first real deployment still broke two of
// them — both immutable-at-init choices whose consequence surfaces several steps
// later, long after the transaction that decided them. A plan is free to change
// until the first deploy; after that some mistakes cost a redeployment and some
// cannot be fixed at all. So everything checkable is checked while it is still free.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"magi_token/reporter/broadcast"
	"magi_token/setup"
)

// Hive caps custom_json operations at 5 per account per block; a deployment is
// more calls than that. Stay one under the cap and wait out a block, for the same
// reason the reporter does: the alternative is a consensus assert partway through
// that says nothing about rate limits.
const (
	customOpsPerBlock = 4
	hiveBlockInterval = 3500 * time.Millisecond
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	planPath := fs.String("plan", "", "path to the deployment plan JSON")
	idsPath := fs.String("ids", "", "JSON map of symbolic name -> deployed contract id")
	netID := fs.String("net-id", "vsc-testnet", "VSC network id")
	api := fs.String("hive-api", "https://api.hive.blog", "Hive JSON-RPC endpoint")
	chainID := fs.String("chain-id", "", "Hive chain id (empty = mainnet)")
	wifEnv := fs.String("wif-env", "MAGI_SETUP_WIF", "env var holding the deployer active WIF")
	doBroadcast := fs.Bool("broadcast", false, "actually send (default is a dry run)")
	_ = fs.Parse(os.Args[2:])

	switch cmd {
	case "template":
		b, _ := json.MarshalIndent(setup.Template(), "", "  ")
		fmt.Println(string(b))
	case "run":
		p, err := load(*planPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		// Never execute a plan that would not pass check: the ordering only protects
		// you if the values it orders are sane.
		if probs := p.Check(); len(probs) > 0 {
			fmt.Fprintf(os.Stderr, "refusing to run a plan with %d problem(s) — run `check`\n", len(probs))
			os.Exit(1)
		}
		ids, err := loadIDs(*idsPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		calls, err := p.Calls(ids)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if err := execute(p, calls, *doBroadcast, *api, *netID, *chainID, *wifEnv); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "check", "steps":
		p, err := load(*planPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		probs := p.Check()
		if cmd == "steps" {
			if len(probs) > 0 {
				fmt.Fprintf(os.Stderr, "refusing to print a sequence for a plan with %d problem(s) — run `check`\n", len(probs))
				os.Exit(1)
			}
			for _, s := range p.Steps() {
				fmt.Println(s)
				fmt.Printf("     why: %s\n", s.Why)
			}
			return
		}
		if len(probs) == 0 {
			fmt.Println("plan is deployable: no problems found")
			fmt.Println("next: magi-setup steps -plan", *planPath)
			return
		}
		fmt.Printf("%d problem(s) — NOTHING has been deployed, fix these first:\n\n", len(probs))
		for _, pr := range probs {
			fmt.Println(" ✗", pr)
		}
		fmt.Println("\nThe README's \"Order matters\" section explains each numbered constraint.")
		os.Exit(1)
	default:
		usage()
		os.Exit(2)
	}
}

func load(path string) (*setup.Plan, error) {
	if path == "" {
		return nil, fmt.Errorf("-plan is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p setup.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// execute runs the ordered calls, pausing where the deployer is not the signer.
//
// It stops at the first failure rather than pressing on. These calls are ordered
// and several are irreversible: continuing past a failed C1.init would initialise
// C2 against a contract that is not configured, and C2.init sets the clock.
func execute(p *setup.Plan, calls []setup.Step2Call, doBroadcast bool,
	api, netID, chainID, wifEnv string) error {

	var b broadcast.Broadcaster = &broadcast.DryRun{NetID: netID}
	if doBroadcast {
		hb, err := broadcast.NewHiveBroadcaster([]string{api}, netID, strip(p.Deployer), wifEnv, chainID)
		if err != nil {
			return err
		}
		b = hb
	} else {
		fmt.Println("DRY RUN (pass -broadcast to send)")
	}

	sentInBlock := 0
	for i, c := range calls {
		if norm(c.Signer) != norm(p.Deployer) {
			fmt.Printf("\n%2d. PAUSE — %s must sign this one, not %s:\n", i+1, c.Signer, p.Deployer)
			fmt.Printf("    %s.%s %s\n", c.Call.ContractID, c.Call.Action, c.Call.Payload)
			fmt.Printf("    why: %s\n", c.Why)
			fmt.Println("    Run it with that account's key, then re-run to continue.")
			return fmt.Errorf("stopping: step %d needs a signer this tool does not hold", i+1)
		}
		fmt.Printf("\n%2d. %s.%s\n    why: %s\n", i+1, c.Call.ContractID, c.Call.Action, c.Why)
		if doBroadcast && sentInBlock >= customOpsPerBlock {
			time.Sleep(hiveBlockInterval)
			sentInBlock = 0
		}
		txid, err := b.Send(c.Call)
		if err != nil {
			return fmt.Errorf("step %d (%s) failed, nothing after it was sent: %w",
				i+1, c.Call.Action, err)
		}
		if doBroadcast {
			fmt.Printf("    tx %s\n", txid)
			sentInBlock++
		}
	}
	fmt.Println("\nall steps sent. Verify on chain before treating the deployment as live.")
	return nil
}

func loadIDs(path string) (setup.Contracts, error) {
	if path == "" {
		return nil, fmt.Errorf("-ids is required: a JSON map like " +
			`{"token":"vsc1...","c1":"vsc1...","c2":"vsc1...","c3":"vsc1..."}`)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c setup.Contracts
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func strip(acct string) string {
	if i := len(acct) - len(trimPrefix(acct, "hive:")); i > 0 {
		return acct[i:]
	}
	return acct
}

func trimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func norm(s string) string { return s }

func usage() {
	fmt.Fprintln(os.Stderr, `magi-setup — check a deployment before it costs anything

  magi-setup template                 print a starting plan
  magi-setup check -plan deploy.json  refuse every mistake it can find
  magi-setup steps -plan deploy.json  the ordered sequence, with reasons
  magi-setup run   -plan deploy.json -ids ids.json [-broadcast]
                                      execute it, in order, dry by default`)
}
