package itest_test

import (
	"fmt"
	"os"
	"testing"
)

// What an ALREADY-DONE keeper poke costs.
//
// A keeper on a cron pokes every few minutes; almost every one of those pokes finds
// the schedule already caught up. If the idle case cost the same as a working one, a
// keeper's RC bill would be set by its polling interval rather than by the epoch
// cadence, and the "keeper needs no funded ledger" guidance in docs/rc-costs.md would
// be wrong.
//
// distributeEpoch returns before touching the token when next >= current, so the two
// cross-contract calls (allowance, balanceOf) are skipped. This measures what that is
// actually worth rather than trusting the comment that says it.
func TestRC_IdlePokeIsCheap(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := mdSetup(t)

	// first poke of epoch 0: the working case
	working := call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)

	// every subsequent poke at the same height has nothing to do
	var idle int64
	for i := 0; i < 3; i++ {
		r := call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
		if !contains(r.Ret, `"distributed":"0"`) {
			t.Fatalf("expected an idle poke to report nothing distributed, got %s", r.Ret)
		}
		idle = r.RcUsed
	}

	fmt.Printf("\n=== keeper poke ===\nworking (1 epoch): %d RC\nidle (already done): %d RC\n",
		working.RcUsed, idle)

	if idle >= working.RcUsed {
		t.Errorf("an idle poke costs %d RC against %d for a working one — the early return "+
			"before the token calls is not saving anything, so a keeper's bill would scale "+
			"with its POLLING interval rather than the epoch cadence", idle, working.RcUsed)
	}
	// The floor is 100 RC; an idle poke should be near it, not a multiple of it.
	if idle > 1000 {
		t.Errorf("an idle poke costs %d RC — above the 1,000 this guidance assumes, so "+
			"docs/rc-costs.md's \"the keeper needs no funded ledger\" needs rechecking", idle)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
