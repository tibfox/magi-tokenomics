package main

import (
	"strconv"
	"testing"
)

// A quiet epoch must not freeze the channel.
//
// resolveEpoch defaults to oldestUnfinalized, which returns the first epoch in the
// lookback window whose `status|<ch>|<ep>` is unset. An epoch with no qualifying
// posts computes to an empty book, so the plan carries no submitShares and cmdRun
// refuses it — correctly, because finalizeEpoch requires totalShares>0 and pulling
// funding into an epoch that can never finalize strands it.
//
// But nothing ever sets `status` for that epoch, so the SAME epoch is selected on
// the next run, and the next. Every later epoch — including ones with real earners
// and real funding — is never looked at, because a run handles exactly one epoch.
// It only clears when the epoch falls below the window floor: up to `lookback`
// (default 20) epochs of frozen payouts, from one quiet week at launch.
//
// An empty epoch cannot be finalized by ANYONE — the contract refuses it by design
// — so the reporter's job is not to fix it but to step over it and keep working,
// while saying loudly enough that a guardian cancels it.

func epochApp(t *testing.T, status map[uint64]string) (*app, *fakeState) {
	t.Helper()
	kv := map[string]string{}
	for ep, st := range status {
		kv["status|content|"+strconv.FormatUint(ep, 10)] = st
	}
	return gateApp(t, kv, nil)
}

// nextUnfinalizedAfter is what the selector needs in order to step over an epoch it
// cannot work: the next candidate strictly after `ep`, still inside the window.
func TestEmptyEpoch_SelectorCanStepPastAnUnworkableEpoch(t *testing.T) {
	// 41 and 42 are open; 43 finalized; 44 open.
	a, _ := epochApp(t, map[uint64]string{43: "finalized"})

	first, err := a.oldestUnfinalized(45)
	if err != nil {
		t.Fatalf("oldestUnfinalized: %v", err)
	}
	if first != 26 { // windowFloor(45) with default lookback 20
		t.Logf("window floor selection returned %d", first)
	}

	// Stepping past it must yield a LATER open epoch, never the same one again.
	next, err := a.nextUnfinalizedAfter(first, 45)
	if err != nil {
		t.Fatalf("nextUnfinalizedAfter: %v", err)
	}
	if next <= first {
		t.Fatalf("nextUnfinalizedAfter(%d) returned %d — the selector cannot advance, so a "+
			"single unworkable epoch blocks every later one for the whole lookback window",
			first, next)
	}
	if next == 43 {
		t.Errorf("stepped onto epoch 43, which is already finalized")
	}
}

// Stepping must stay inside the window and report exhaustion rather than looping.
func TestEmptyEpoch_SelectorReportsWhenNothingIsLeft(t *testing.T) {
	// everything from the floor to the head is finalized except the very last
	st := map[uint64]string{}
	for ep := uint64(26); ep <= 44; ep++ {
		st[ep] = "finalized"
	}
	a, _ := epochApp(t, st)

	next, err := a.nextUnfinalizedAfter(44, 45)
	if err != nil {
		t.Fatalf("nextUnfinalizedAfter: %v", err)
	}
	if next != 45 {
		t.Fatalf("expected the only remaining open epoch 45, got %d", next)
	}

	// past the head there is nothing to advance to, and that must be an error rather
	// than a wrapped-around or repeated epoch
	if _, err := a.nextUnfinalizedAfter(45, 45); err == nil {
		t.Fatal("advancing past the head must report exhaustion, not return an epoch")
	}
}
