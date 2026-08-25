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

	"magi_token/setup"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	planPath := fs.String("plan", "", "path to the deployment plan JSON")
	_ = fs.Parse(os.Args[2:])

	switch cmd {
	case "template":
		b, _ := json.MarshalIndent(setup.Template(), "", "  ")
		fmt.Println(string(b))
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

func usage() {
	fmt.Fprintln(os.Stderr, `magi-setup — check a deployment before it costs anything

  magi-setup template                 print a starting plan
  magi-setup check -plan deploy.json  refuse every mistake it can find
  magi-setup steps -plan deploy.json  the ordered sequence, with reasons`)
}
