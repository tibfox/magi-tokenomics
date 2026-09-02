package main

import (
	"strconv"
	"testing"
)

// Raising lookback to reach a real backlog broke the backlog scan.
//
// oldestUnfinalized builds one state key per epoch across the whole lookback window
// and reads them in ONE call. The VSC API refuses more than 100 keys, so any window
// wider than that fails outright:
//
//	default target unknown (vsc api: getStateByKeys accepts at most 100 keys, got 4907)
//
// That is the second thing wrong with the same recovery path. The reporter tells an
// operator who has been down to "Raise epoch.lookback and re-run to work them off";
// raising it used to change the policy digest (fixed separately), and raising it far
// enough to matter still exceeded the key cap. A backlog longer than 100 epochs —
// three months of daily epochs — was unreachable by the documented procedure.
//
// The default window of 20 hides it, which is why 178 offline tests passed: nothing
// exercised a window wide enough, and the test double accepted any number of keys.
func TestBacklogScan_WindowWiderThanTheKeyCap(t *testing.T) {
	// A long backlog: everything below 500 finalized, 500 onward outstanding.
	status := map[uint64]string{}
	for ep := uint64(0); ep < 500; ep++ {
		status[ep] = "finalized"
	}
	a, fs := epochApp(t, status)
	// Wide enough that the window actually reaches epoch 500: windowFloor(4500) is
	// latest-lookback+1, so 4000 would floor at 501 and step over the backlog head.
	a.cfg.Epoch.Lookback = 4500

	got, err := a.oldestUnfinalized(4500)
	if err != nil {
		t.Fatalf("a lookback of %d must not break the scan: %v", a.cfg.Epoch.Lookback, err)
	}
	if got != 500 {
		t.Fatalf("oldest unfinalized = %d, want 500 — the scan must find the real "+
			"backlog head, not the window floor", got)
	}
	// It has to have been split: one call could not have carried 4001 keys.
	if fs.hits < 2 {
		t.Fatalf("the window was read in %d call(s); a window this wide must be "+
			"chunked under the 100-key cap", fs.hits)
	}
	t.Logf("%d-epoch window scanned in %d calls, found epoch %d",
		a.cfg.Epoch.Lookback, fs.hits, got)
}

// The default window must keep working, and must not pay for chunking it does not
// need: 21 keys is one call.
func TestBacklogScan_DefaultWindowIsStillOneCall(t *testing.T) {
	a, fs := epochApp(t, map[uint64]string{43: "finalized"})
	if _, err := a.oldestUnfinalized(45); err != nil {
		t.Fatalf("oldestUnfinalized: %v", err)
	}
	if fs.hits != 1 {
		t.Fatalf("the default 20-epoch window took %d calls, want 1", fs.hits)
	}
}

// Chunking must not change which epoch is chosen. Convergence is the whole point of
// picking the oldest: two reporters running minutes apart must target the same epoch,
// and a scan that returned a different answer depending on how it was batched would
// break an Attest quorum in a way no single reporter could see.
func TestBacklogScan_ChunkingDoesNotChangeTheAnswer(t *testing.T) {
	status := map[uint64]string{}
	for ep := uint64(0); ep < 250; ep++ {
		if ep != 137 { // 137 is the one gap
			status[ep] = "finalized"
		}
	}
	a, _ := epochApp(t, status)
	for _, lookback := range []uint64{200, 300, 1000} {
		a.cfg.Epoch.Lookback = lookback
		got, err := a.oldestUnfinalized(249)
		if err != nil {
			t.Fatalf("lookback %d: %v", lookback, err)
		}
		if got != 137 {
			t.Fatalf("lookback %d selected epoch %d, want 137 — the batching changed "+
				"the answer, so two reporters with different windows would target "+
				"different epochs "+strconv.Itoa(int(got)), lookback, got)
		}
	}
}
