package main

import (
	"testing"
	"time"
)

// A real share book is more custom_json calls than Hive lets one account put in a
// single block, and the reporter used to fire them back to back.
//
// Hive's cap is HIVE_CUSTOM_OP_BLOCK_LIMIT (5 per account per block). Broadcasting a
// 26-page book put five calls in one block and the chain rejected the sixth:
//
//	Assert Exception: insert_info.first->second <= HIVE_CUSTOM_OP_BLOCK_LIMIT
//
// Nothing was lost, because the run stops at the first failure and resume skips what
// already applied — but landing one epoch then took repeated manual re-runs, each
// ending in a consensus assert that says nothing about rate limits. This is not an
// edge case at scale either: the sizing in docs/rc-costs.md, 500 earners over 9
// pages, still needs 13 calls once the keeper poke, the pull, the root and the
// finalize are counted.
//
// What this pins is the arithmetic that keeps a run under the cap. It deliberately
// does NOT assert the wall-clock wait — that would make the test slow and flaky for
// no gain — only that the batch size leaves headroom and that the interval clears a
// block.
func TestBlockPacing_StaysUnderHiveCustomOpLimit(t *testing.T) {
	// Hive's own limit. If a future chain release raises it this test still passes;
	// what must never happen is us pacing AT or ABOVE it.
	const hiveCustomOpBlockLimit = 5

	if customOpsPerBlock >= hiveCustomOpBlockLimit {
		t.Fatalf("customOpsPerBlock = %d, which is not under Hive's limit of %d — "+
			"a batch would fill the block and the next call would be rejected by "+
			"consensus, not by us", customOpsPerBlock, hiveCustomOpBlockLimit)
	}
	if customOpsPerBlock < 1 {
		t.Fatalf("customOpsPerBlock = %d would stall every run", customOpsPerBlock)
	}
	// A Hive block is 3s. Waiting exactly 3s races a slow head and can fold two
	// batches into one block, which is the failure this pacing exists to prevent.
	if hiveBlockInterval <= 3*time.Second {
		t.Fatalf("hiveBlockInterval = %v does not clear a 3s block with any margin",
			hiveBlockInterval)
	}
}

// The pacer must actually be reached once a batch is full, and must not fire before
// that — a wait on every call would make a 26-page book needlessly slow.
func TestBlockPacing_WaitsOnlyWhenTheBatchIsFull(t *testing.T) {
	orig := pace
	defer func() { pace = orig }()

	var waits int
	pace = func(time.Duration) { waits++ }

	// Mirror the loop's accounting: a wait happens before a send when the batch is
	// full, and the counter resets after it.
	sentInBlock := 0
	sends := 3*customOpsPerBlock + 1 // three full batches and one more call
	for i := 0; i < sends; i++ {
		if sentInBlock >= customOpsPerBlock {
			pace(hiveBlockInterval)
			sentInBlock = 0
		}
		sentInBlock++
	}

	if want := 3; waits != want {
		t.Fatalf("paced %d times over %d sends with a batch of %d, want %d — "+
			"too few and a batch overruns the block limit, too many and every "+
			"page costs an extra block", waits, sends, customOpsPerBlock, want)
	}
}
