package itest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Event emission, checked the way magi-mongo-indexer actually reads it.
//
// The contracts emitted NOTHING until now — no sdk.Log on any path — so the
// indexer had no rows to build from and the only way to learn what a deployment
// had done was to read its raw state. These tests are the guard against that
// silently returning: they assert both that the events exist and that they are
// shaped the way the indexer's parser and data layer require.
//
// The parser does `json.Unmarshal` into map[string]interface{} and then walks the
// mapping's `$.attributes.<name>` path. Two properties follow, and both are
// asserted below rather than assumed:
//
//  1. every log is one JSON object with a STRING `type` and an OBJECT `attributes`
//  2. every value inside `attributes` is a JSON STRING
//
// (2) is the one that bites. encoding/json decodes a bare JSON number into
// float64, so an epoch or a big.Int amount emitted as a number would be silently
// rounded on the way in — 18446744073709551615 arrives as 18446744073709552000.
// magi_token-contract serialises its *big.Int amounts as strings for this reason
// and we follow it for every numeric value without exception.

// collectLogs runs a call and returns every log the transaction produced, across
// ALL contracts it touched — not just the one addressed.
//
// This matters for the pull path: C3.pullFunding calls C2.claimBucket, and C2's
// `bucket_claim` log is keyed under C2's id in the result while C3's `pull` is
// under C3's. The indexer sees them the same way, as two logs from two contracts
// in one transaction, so a test that reads only the addressed contract's slice
// would miss half of what a real deployment emits.
func collectLogs(t *testing.T, ct *test_utils.ContractTest, id, action, payload, caller string, h uint64) []string {
	t.Helper()
	res := call(t, ct, id, action, payload, caller, h, true)
	out := []string{}
	for _, lo := range res.Logs {
		out = append(out, lo.Logs...)
	}
	return out
}

// parseEvent asserts the envelope and returns (type, attributes).
func parseEvent(t *testing.T, raw string) (string, map[string]interface{}) {
	t.Helper()
	var top map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("log is not valid JSON: %v\n  %s", err, raw)
	}
	lt, ok := top["type"].(string)
	if !ok || lt == "" {
		t.Fatalf("log has no string `type` — the indexer keys its mapping on it:\n  %s", raw)
	}
	attrs, ok := top["attributes"].(map[string]interface{})
	if !ok {
		t.Fatalf("log %q has no `attributes` object — every mapped field is read as $.attributes.<name>:\n  %s", lt, raw)
	}
	// Every attribute must be a string. See the package note above: a bare number
	// would lose precision in the indexer's float64 decode.
	for k, v := range attrs {
		if _, isStr := v.(string); !isStr {
			t.Errorf("event %q attribute %q is %T, not a string — a JSON number loses precision "+
				"in the indexer's decode; emit it via Str/Big/U64/Int/Bool:\n  %s", lt, k, v, raw)
		}
	}
	return lt, attrs
}

// TestEvents_FullSliceEmitsEveryLifecycleEvent walks token → C2 → C3 and asserts
// each step produced the event an indexer needs to rebuild that step's state.
func TestEvents_FullSliceEmitsEveryLifecycleEvent(t *testing.T) {
	const c3EvID = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c3EvID, owner, read("../c3-distributor/artifacts/main.wasm"))

	seen := map[string][]map[string]interface{}{}
	record := func(logs []string) {
		for _, raw := range logs {
			lt, attrs := parseEvent(t, raw)
			seen[lt] = append(seen[lt], attrs)
		}
	}

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
		`"blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0",`+
		`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
		`"vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, c3EvID)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)

	record(collectLogs(t, &ct, c2ID, "init", c2init, owner, 0))
	c3init := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
		`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID)
	record(collectLogs(t, &ct, c3EvID, "init", c3init, owner, 0))
	record(collectLogs(t, &ct, c3EvID, "addChannel",
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:reporter","reporterThreshold":"1","role":"content"}`, owner, 0))

	record(collectLogs(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1))

	// pullFunding calls C2.claimBucket underneath, so this one transaction produces
	// C3's `pull` AND C2's `bucket_claim`. Claiming the bucket separately first would
	// empty the owed record and make the pull abort.
	record(collectLogs(t, &ct, c3EvID, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:anyone", 1))
	record(collectLogs(t, &ct, c3EvID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`, "hive:reporter", 1))
	record(collectLogs(t, &ct, c3EvID, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:reporter", 1))
	record(collectLogs(t, &ct, c3EvID, "claim", `{"channel":"author","epoch":"0"}`, "hive:alice", 2))

	for _, want := range []string{
		"c2_init", "c3_init", "channel", "emit", "poke", "alloc",
		"bucket_claim", "pull", "shares", "epoch_status", "claim",
	} {
		assert.NotEmpty(t, seen[want], "no %q event was emitted — the indexer cannot rebuild that step", want)
	}

	// Spot-check the values an indexer would key on, not just that a row appeared.
	if len(seen["claim"]) > 0 {
		c := seen["claim"][0]
		assert.Equal(t, "author", c["channel"])
		assert.Equal(t, "0", c["epoch"])
		assert.Equal(t, "hive:alice", c["acct"])
		assert.Equal(t, "60000", c["payout"], "60/100 of a 100000 epoch")
		// funded*share/total must be re-derivable from the event alone.
		assert.Equal(t, "60", c["share"])
		assert.Equal(t, "100", c["total_shares"])
		assert.Equal(t, "100000", c["funded"])
	}
	if len(seen["poke"]) > 0 {
		p := seen["poke"][0]
		assert.Equal(t, "1", p["epochs"])
		assert.Equal(t, "100000", p["pulled"])
		assert.Equal(t, "false", p["starved"])
	}
	if len(seen["epoch_status"]) > 0 {
		assert.Equal(t, "finalized", seen["epoch_status"][0]["status"])
	}
}

// TestEvents_CatchUpEmitsPerEpochDetail covers the path the aggregation rewrote.
//
// distributeEpoch now makes ONE transferFrom for a whole catch-up instead of one per
// epoch, which is where the RC saving comes from — but it means the token's own
// transfer log no longer says which epochs the money was for. The per-epoch `emit`
// and per-(bucket,epoch) `alloc` events are what carry that attribution now, so they
// are load-bearing rather than decorative: without them a 10-epoch catch-up is a
// single opaque transfer.
func TestEvents_CatchUpEmitsPerEpochDetail(t *testing.T) {
	const c3EvID = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c3EvID, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// Two buckets so the per-(bucket, epoch) split is actually exercised.
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
		`"blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0",`+
		`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
		`"vetoThreshold":"1","buckets":"author:contract:%s:7000,ops:hive:ops:3000"}`, tokenID, c3EvID)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)

	// One poke at height 10 catches up epochs 0..9 in a single transaction.
	logs := collectLogs(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 10)

	emits := map[string]string{}  // epoch -> emission
	allocs := map[string]string{} // bucket|epoch -> amount
	var poke map[string]interface{}
	for _, raw := range logs {
		switch lt, a := parseEvent(t, raw); lt {
		case "emit":
			emits[a["epoch"].(string)] = a["emission"].(string)
		case "alloc":
			allocs[a["bucket"].(string)+"|"+a["epoch"].(string)] = a["amount"].(string)
		case "poke":
			poke = a
		}
	}

	assert.Len(t, emits, 10, "one emit event per epoch caught up")
	for ep := 0; ep < 10; ep++ {
		k := fmt.Sprintf("%d", ep)
		assert.Equal(t, "100000", emits[k], "epoch %s emission", k)
		// 7000/3000 bps of 100000, and the split must be attributed per epoch.
		assert.Equal(t, "70000", allocs["author|"+k], "author slice for epoch %s", k)
		assert.Equal(t, "30000", allocs["ops|"+k], "ops slice for epoch %s", k)
	}

	if assert.NotNil(t, poke, "a catch-up must emit exactly one poke summary") {
		assert.Equal(t, "0", poke["from_epoch"])
		assert.Equal(t, "9", poke["last_epoch"])
		assert.Equal(t, "10", poke["epochs"])
		// The single aggregated transferFrom — 10 epochs at 100000 in ONE pull.
		assert.Equal(t, "1000000", poke["pulled"])
		assert.Equal(t, "false", poke["starved"])
	}
}

// TestEvents_StarvedPokeIsVisible — a pool that cannot fund even one epoch must say
// so. Otherwise the indexer shows no emission and no explanation, and "the pool is
// empty" is indistinguishable from "the keeper died".
func TestEvents_StarvedPokeIsVisible(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// Pool holds one epoch (100000) and no more.
	fundC2Pool(t, &ct, tokenID, c2ID, "100000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"ops","timelock":"1","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
			`"vetoThreshold":"1","buckets":"ops:hive:ops:10000"}`, tokenID), owner, 0, true)

	// First poke drains the pool paying epoch 0.
	first := collectLogs(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1)
	var firstPoke map[string]interface{}
	for _, raw := range first {
		if lt, a := parseEvent(t, raw); lt == "poke" {
			firstPoke = a
		}
	}
	if assert.NotNil(t, firstPoke) {
		assert.Equal(t, "1", firstPoke["epochs"])
		assert.Equal(t, "false", firstPoke["starved"])
	}

	// Second poke: epochs are due but the pool is dry. No epoch can be funded, so
	// nothing is emitted about emission — but the starvation itself must be.
	second := collectLogs(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 5)
	var starvedPoke map[string]interface{}
	emitCount := 0
	for _, raw := range second {
		switch lt, a := parseEvent(t, raw); lt {
		case "poke":
			starvedPoke = a
		case "emit":
			emitCount++
		}
	}
	assert.Zero(t, emitCount, "nothing was funded, so no epoch may claim to have emitted")
	if assert.NotNil(t, starvedPoke, "a starved poke must be visible to the indexer") {
		assert.Equal(t, "0", starvedPoke["epochs"])
		assert.Equal(t, "0", starvedPoke["pulled"])
		assert.Equal(t, "true", starvedPoke["starved"])
		assert.Equal(t, "1", starvedPoke["from_epoch"], "the epoch it is stuck waiting to fund")
	}
}

// TestEvents_SkippedShareEntryIsRecorded covers the one failure that is otherwise
// invisible: submitShares drops a malformed entry, the page still applies, and the
// earner is never paid. The transaction SUCCEEDS, which is exactly the case a log
// can capture (the indexer keeps logs from committed transactions only).
func TestEvents_SkippedShareEntryIsRecorded(t *testing.T) {
	const c3EvID = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c3EvID, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
		`"blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0",`+
		`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
		`"vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, c3EvID)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)
	call(t, &ct, c3EvID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID), owner, 0, true)
	call(t, &ct, c3EvID, "addChannel",
		`{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:reporter","reporterThreshold":"1"}`, owner, 0, true)
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, c3EvID, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:anyone", 1, true)

	// `alice` has no ledger domain — silently dropped before this change.
	logs := collectLogs(t, &ct, c3EvID, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"alice:60,hive:bob:40,hive:carol:0"}`,
		"hive:reporter", 1)

	skips := []map[string]interface{}{}
	var shares map[string]interface{}
	for _, raw := range logs {
		lt, attrs := parseEvent(t, raw)
		switch lt {
		case "skip":
			skips = append(skips, attrs)
		case "shares":
			shares = attrs
		}
	}

	assert.Len(t, skips, 2, "both the domainless account and the zero-share entry must be reported")
	reasons := map[string]string{}
	for _, s := range skips {
		assert.Equal(t, "shares", s["scope"])
		assert.Equal(t, "author", s["channel"])
		assert.Equal(t, "0", s["epoch"])
		assert.Equal(t, "0", s["page"])
		reasons[s["raw_entry"].(string)] = s["reason"].(string)
	}
	assert.Equal(t, "not a ledger address", reasons["alice:60"])
	assert.Equal(t, "shares not positive", reasons["hive:carol:0"])

	// The page still applied, with only the good entry counted. `submitted` carries
	// what the reporter sent verbatim, so applied = submitted minus the skip logs —
	// which is what makes the per-account share book reconstructible from one log
	// per page instead of one per entry.
	if assert.NotNil(t, shares, "a page summary must be emitted even when entries are skipped") {
		assert.Equal(t, "1", shares["entries"])
		assert.Equal(t, "40", shares["page_total"])
		assert.Equal(t, "40", shares["new_total"])
		assert.Equal(t, "alice:60,hive:bob:40,hive:carol:0", shares["submitted"])
	}
}

// TestEvents_StakeLifecycleEmitsDrawdown covers C1, whose drawdown accumulator is
// the one value an observer cannot recompute from the outside: it telescopes over
// every mutation in an epoch, so claimYield's denominator is only auditable if the
// contract says what it did.
func TestEvents_StakeLifecycleEmitsDrawdown(t *testing.T) {
	const c1EvID = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c1EvID, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))

	// epochLen 4, not 1. With a one-block epoch EVERY height is an epoch start, and
	// noteDrawdown returns early there by design (a mutation on the first block
	// DEFINES aᵢ rather than moving away from it), so no drawdown would ever be
	// recorded and the assertion below would be vacuous. cooldown must exceed
	// epochLen (R15), hence 5.
	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c1EvID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"4","allow":"","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID), owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"4","baseAnnual":"1000000",`+
		`"blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0",`+
		`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto",`+
		`"vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, tokenID, c1EvID)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)
	call(t, &ct, c1EvID, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, c2ID), owner, 0, true)

	call(t, &ct, tokenID, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, c1EvID), "hive:alice", 0, true)

	stakeLogs := collectLogs(t, &ct, c1EvID, "stake", `{"amount":"600"}`, "hive:alice", 0)
	var stakeEv map[string]interface{}
	for _, raw := range stakeLogs {
		if lt, attrs := parseEvent(t, raw); lt == "stake" {
			stakeEv = attrs
		}
	}
	if assert.NotNil(t, stakeEv, "stake must emit an event") {
		assert.Equal(t, "hive:alice", stakeEv["acct"])
		assert.Equal(t, "stake", stakeEv["via"], "via distinguishes a purchase from a migration credit")
		assert.Equal(t, "600", stakeEv["amount"])
		assert.Equal(t, "600", stakeEv["new_stake"])
		assert.Equal(t, "600", stakeEv["new_total"])
	}

	// Unstaking mid-epoch (height 2 is inside epoch 0, which starts at 0) moves the
	// drawdown accumulator off zero.
	unLogs := collectLogs(t, &ct, c1EvID, "unstake", `{"amount":"100"}`, "hive:alice", 2)
	var unstakeEv, ddEv map[string]interface{}
	for _, raw := range unLogs {
		switch lt, attrs := parseEvent(t, raw); lt {
		case "unstake":
			unstakeEv = attrs
		case "drawdown":
			ddEv = attrs
		}
	}
	if assert.NotNil(t, unstakeEv, "unstake must emit an event") {
		assert.Equal(t, "100", unstakeEv["amount"])
		assert.Equal(t, "500", unstakeEv["new_stake"])
		assert.Equal(t, "7", unstakeEv["ready_height"], "height 2 + cooldown 5")
	}
	if assert.NotNil(t, ddEv, "a stake drop inside an epoch must emit its drawdown delta") {
		assert.Equal(t, "hive:alice", ddEv["acct"])
		assert.Equal(t, "0", ddEv["epoch"], "height 2, epochLen 4, genesis 0")
		assert.Equal(t, "100", ddEv["delta"])
		assert.Equal(t, "100", ddEv["new_dd"])
	}
}
