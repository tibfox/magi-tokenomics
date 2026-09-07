package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A plan key the schema does not define must be REFUSED, not silently dropped.
//
// This is the defect this test exists for, found by using the tool for real: a plan
// carrying a top-level "genesis" was accepted with "plan is deployable: no problems
// found", and the key was discarded by encoding/json. The tool's entire promise is
// that it refuses a mistake before it costs anything, and a silently ignored key is
// the one mistake it cannot refuse — the operator reads their own file back as proof
// the setting is applied.
//
// The dangerous case is not "genesis" (C2 defaults it to the init block, which is
// usually what was wanted anyway) but a misspelt setting that the plan MEANT to turn
// on. `stake_bps` for `staked_bps` deploys with staked payouts silently off, and
// constraint 6's failure mode is that this surfaces at the point of PAYMENT, when the
// first earner claims — long after C1.init made `allow` immutable.
func TestLoad_RefusesUnknownPlanFields(t *testing.T) {
	// A plan valid enough to reach the decoder; only the stray key is at issue.
	const base = `{
	  "deployer":"hive:someone",
	  "token":{"name":"T","symbol":"T","decimals":0,"max_supply":"1","mint":"1"},
	  "c1":{"epoch_len":50,"treasury":"hive:t"},
	  "c2":{"epoch_len":50},
	  "c3":{"treasury":"hive:t"},
	  "channels":[]
	}`

	for _, c := range []struct{ name, mutate, want string }{
		{"top-level unknown key", `"genesis":123,`, "genesis"},
		{"misspelt staked_bps", `"xx":0,`, "xx"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "plan.json")
			body := strings.Replace(base, `{
	  "deployer"`, "{\n\t  "+c.mutate+`
	  "deployer"`, 1)
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := load(p)
			if err == nil {
				t.Fatal("an unknown plan key was accepted — a misspelt setting would " +
					"deploy silently as its zero value")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error must NAME the offending key so it can be fixed without "+
					"reading the source; got %q", err)
			}
		})
	}

	// ...and a plan with no stray keys must still load, or the guard is just a wall.
	p := filepath.Join(t.TempDir(), "ok.json")
	if err := os.WriteFile(p, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(p); err != nil {
		t.Fatalf("a clean plan must still load: %v", err)
	}
}
