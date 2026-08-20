package itest_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
)

// TestRC_ProfileAllFunctions measures the REAL metered RC cost of every callable
// function across all seven contracts, using the same metering the chain applies
// (RcUsed = ceil(gas / CYCLE_GAS_PER_RC), with a floor of 100).
//
// Why this matters: RC = the account's VSC-ledger HBD balance + a 10,000 free
// allowance. An operator sizing a deployment needs to know which calls fit in the
// free tier, which need a funded account, and — for the variable-cost calls — how
// the cost scales with input size. Guessing here is how a keeper poke or a share
// page silently starts failing in production.
//
// Output is a table; `go test -run TestRC_Profile -v` prints it. The committed
// results live in docs/rc-costs.md.
type rcRow struct {
	contract string
	action   string
	note     string
	rc       int64
	gas      uint
}

func TestRC_ProfileAllFunctions(t *testing.T) {
	const (
		pTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		pC1  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
		pC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
		pC3  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"

		alice = "hive:rcalice"
		bob   = "hive:rcbob"
		guard = "hive:rcguardian"
		veto  = "hive:rcveto"
		treas = "hive:rctreasury"
	)

	var rows []rcRow
	record := func(contract, action, note string, r test_utils.ContractTestCallResult) {
		rows = append(rows, rcRow{contract, action, note, r.RcUsed, r.GasUsed})
	}

	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(pTok, owner, read(tokenWasmPath))
	ct.RegisterContract(pC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(pC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(pC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	// ---- setup: the standard deployment sequence -------------------------
	record("C0 token", "init", "", call(t, &ct, pTok, "init",
		`{"name":"RC","symbol":"RC","decimals":0,"maxSupply":"100000000000"}`, owner, 0, true))
	record("C0 token", "mint", "credits the owner", call(t, &ct, pTok, "mint",
		`{"amount":"1000000"}`, owner, 0, true))
	record("C0 token", "transfer", "", call(t, &ct, pTok, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"100000"}`, pC1), owner, 0, true))
	record("C0 token", "transfer", "to a plain account", call(t, &ct, pTok, "transfer",
		fmt.Sprintf(`{"to":"%s","amount":"5000"}`, alice), owner, 0, true))

	record("C1 staking", "init", "staking + yield + airdrop", call(t, &ct, pC1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"5","allow":"%s",`+
			`"maxAirdrop":"100000","treasury":"%s","guardianMode":"0","guardianAuth":"%s",`+
			`"guardianThreshold":"1"}`, pTok, owner, treas, guard), owner, 0, true))
	// float for the airdrop batches: they may only spend the UNOBLIGATED balance.
	call(t, &ct, pTok, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"100000"}`, pC1), owner, 0, true)
	for _, n := range []int{1, 10, 25, 50} {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			parts[i] = fmt.Sprintf("hive:rcdrop%03d:10", i)
		}
		record("C1 staking", "airdropBatch", fmt.Sprintf("%d recipients", n),
			call(t, &ct, pC1, "airdropBatch",
				fmt.Sprintf(`{"batchId":"%d","entries":"%s"}`, n, strings.Join(parts, ",")), owner, 0, true))
	}

	record("C0 token", "approve", "", call(t, &ct, pTok, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"5000"}`, pC1), alice, 1, true))
	record("C1 staking", "stake", "first stake for an account", call(t, &ct, pC1, "stake",
		`{"amount":"1000"}`, alice, 1, true))
	record("C1 staking", "stake", "second stake (history append)", call(t, &ct, pC1, "stake",
		`{"amount":"500"}`, alice, 2, true))
	record("C0 token", "approve", "owner->C1 for stakeFor", call(t, &ct, pTok, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"2000"}`, pC1), owner, 2, true))
	record("C1 staking", "stakeFor", "allowlisted", call(t, &ct, pC1, "stakeFor",
		fmt.Sprintf(`{"acct":"%s","amount":"500"}`, bob), owner, 2, true))
	record("C1 staking", "unstake", "queues a cooldown entry", call(t, &ct, pC1, "unstake",
		`{"amount":"200"}`, alice, 3, true))
	record("C1 staking", "claimUnstaked", "after cooldown, 1 entry", call(t, &ct, pC1, "claimUnstaked",
		``, alice, 40, true))
	// A SECOND claimUnstaked row, with several matured entries, because the
	// single-entry figure above cannot show what the batching is worth: every
	// matured entry used to cost its own token transfer, and they all pay the same
	// recipient. bob queues five, then claims them in one call.
	record("C0 token", "approve", "owner->C1 for bob's stake",
		call(t, &ct, pTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"5000"}`, pC1), owner, 40, true))
	call(t, &ct, pC1, "stakeFor", fmt.Sprintf(`{"acct":"%s","amount":"500"}`, bob), owner, 40, true)
	for i := 0; i < 5; i++ {
		call(t, &ct, pC1, "unstake", `{"amount":"50"}`, bob, uint64(41+i), true)
	}
	record("C1 staking", "claimUnstaked", "after cooldown, 5 entries", call(t, &ct, pC1, "claimUnstaked",
		``, bob, 80, true))
	record("C1 staking", "stakeOf", "query", call(t, &ct, pC1, "stakeOf",
		fmt.Sprintf(`{"account":"%s"}`, alice), alice, 40, true))
	record("C1 staking", "stakeAtHeight", "query, historical", call(t, &ct, pC1, "stakeAtHeight",
		fmt.Sprintf(`{"account":"%s","height":"2"}`, alice), alice, 40, true))

	// Fund the pool BEFORE handing the token over — only the owner may mint.
	fundC2Pool(t, &ct, pTok, pC2, "500000000", 40)

	record("C0 token", "changeOwner", "hand token to C2", call(t, &ct, pTok, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, pC2), owner, 40, true))

	record("C2 emission", "init", "3 buckets", call(t, &ct, pC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"40","epochLen":"5","baseAnnual":"1000000",`+
			`"blocksPerYear":"1000","dustBucket":"content","maxCatch":"50","timelock":"5",`+
			`"guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"%s","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:5000,lp:contract:%s:3000,yield:contract:%s:2000"}`,
		pTok, guard, veto, pC3, pC3, pC1), owner, 40, true))

	distCfg := fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"%s",`+
			`"guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1"}`,
		pTok, pC2, treas, guard)
	record("distributor", "init", "", call(t, &ct, pC3, "init", distCfg, owner, 41, true))
	// Both reward channels live on ONE contract now, so the per-channel policy is
	// registered rather than deployed a second time.
	chCfg := func(ch, reporter string) string {
		return fmt.Sprintf(`{"channel":"%s","bucket":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"%s","reporterThreshold":"1"}`, ch, ch, reporter)
	}
	record("distributor", "addChannel", "content", call(t, &ct, pC3, "addChannel", chCfg("content", owner), owner, 41, true))
	record("distributor", "addChannel", "lp", call(t, &ct, pC3, "addChannel", chCfg("lp", owner), owner, 41, true))
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, pC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, pC2), owner, 41, true)

	// ---- the recurring per-epoch path ------------------------------------
	record("C2 emission", "distributeEpoch", "1 epoch, 3 buckets", call(t, &ct, pC2, "distributeEpoch",
		``, "hive:keeper", 46, true))
	record("C2 emission", "scheduleInfo", "query", call(t, &ct, pC2, "scheduleInfo", ``, "hive:any", 46, true))
	record("C2 emission", "owedOf", "query", call(t, &ct, pC2, "owedOf",
		fmt.Sprintf(`{"target":"contract:%s","epoch":"0"}`, pC3), "hive:any", 46, true))

	record("distributor", "pullFunding", "cross-contract claimBucket", call(t, &ct, pC3, "pullFunding",
		`{"channel":"content","epoch":"0"}`, "hive:keeper", 46, true))
	record("distributor", "pullFunding", "lp channel", call(t, &ct, pC3, "pullFunding",
		`{"channel":"lp","epoch":"0"}`, "hive:keeper", 46, true))
	record("C1 staking", "pullFunding", "yield", call(t, &ct, pC1, "pullFunding",
		`{"epoch":"0"}`, "hive:keeper", 46, true))

	// submitShares is the one that scales with input — measure the curve.
	//
	// Account names are the MAXIMUM a Hive account can be (`hive:` + 16 characters).
	// The cost scales with the bytes a page writes and logs, not just its entry count,
	// so the shorter synthetic ids this fixture used before understated a real page by
	// about 5% at 30 entries — and these figures are what the reporter's rc_limit check
	// is sized from, where understating is the direction that breaks things.
	// Accumulated as the pages go out, because finalizeEpoch now requires the declared
	// totalShares to equal what the pages actually published — an over-declared
	// denominator used to strand the difference where no call could reach it. The same
	// account appears on several pages here, and duplicate entries SUM, exactly as
	// sharecore treats them.
	published := map[string]int64{}
	for _, n := range []int{1, 10, 30, 60} {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			acct := fmt.Sprintf("hive:rcsharemaxlen%03d", i)
			parts[i] = fmt.Sprintf("%s:%d", acct, 100+i)
			published[acct] += int64(100 + i)
		}
		record("distributor", "submitShares", fmt.Sprintf("%d entries (1 page)", n),
			call(t, &ct, pC3, "submitShares", fmt.Sprintf(
				`{"channel":"content","epoch":"0","page":"%d","entries":"%s"}`, n, strings.Join(parts, ",")), owner, 46, true))
	}
	publishedTotal := int64(0)
	for _, v := range published {
		publishedTotal += v
	}
	record("distributor", "shareOf", "query", call(t, &ct, pC3, "shareOf",
		`{"channel":"content","epoch":"0","account":"hive:rcsharemaxlen000"}`, "hive:any", 46, true))
	// The commitment: one call for the whole epoch, whatever the earner count.
	pBook := shareBook(published)
	record("distributor", "submitRoot", "commitment for the epoch",
		call(t, &ct, pC3, "submitRoot", fmt.Sprintf(
			`{"channel":"content","epoch":"0","root":"%s","totalShares":"%d","accounts":"%d"}`,
			pBook.tree.Root(), publishedTotal, len(published)), owner, 46, true))
	record("distributor", "finalizeEpoch", "", call(t, &ct, pC3, "finalizeEpoch",
		`{"channel":"content","epoch":"0"}`, owner, 46, true))
	record("distributor", "claim", "with a merkle proof", call(t, &ct, pC3, "claim",
		pBook.claimFor(t, "content", "0", "hive:rcsharemaxlen000"), "hive:rcsharemaxlen000", 60, true))

	record("distributor", "submitShares", "lp channel, 2 entries", call(t, &ct, pC3, "submitShares",
		fmt.Sprintf(`{"channel":"lp","epoch":"0","page":"0","entries":"%s:70,%s:30"}`, alice, bob), owner, 46, true))
	lpBook := shareBook(map[string]int64{alice: 70, bob: 30})
	lpBook.submitRoot(t, &ct, pC3, "lp", "0", owner, 46)
	record("distributor", "finalizeEpoch", "lp channel", call(t, &ct, pC3, "finalizeEpoch", `{"channel":"lp","epoch":"0"}`, owner, 46, true))
	record("distributor", "claim", "lp channel", call(t, &ct, pC3, "claim",
		lpBook.claimFor(t, "lp", "0", alice), alice, 60, true))

	record("C1 staking", "claimYield", "local stake history", call(t, &ct, pC1, "claimYield",
		`{"epoch":"0"}`, alice, 60, true))
	record("C1 staking", "fundedOf", "query", call(t, &ct, pC1, "fundedOf", `{"epoch":"0"}`, "hive:any", 60, true))

	// ---- guardian / governance paths --------------------------------------
	record("C2 emission", "queueTokenOp", "timelocked", call(t, &ct, pC2, "queueTokenOp",
		`{"op":"pause","nonce":"1"}`, guard, 60, true))
	record("C2 emission", "cancelTokenOp", "veto", call(t, &ct, pC2, "cancelTokenOp",
		`{"op":"pause","nonce":"1"}`, veto, 61, true))
	record("C2 emission", "queueTokenOp", "re-queue after cancel", call(t, &ct, pC2, "queueTokenOp",
		`{"op":"pause","nonce":"2"}`, guard, 62, true))
	record("C2 emission", "executeTokenOp", "after timelock; calls token.pause", call(t, &ct, pC2, "executeTokenOp",
		`{"op":"pause","nonce":"2"}`, guard, 70, true))

	// ---- report -----------------------------------------------------------
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].contract != rows[j].contract {
			return rows[i].contract < rows[j].contract
		}
		return rows[i].rc > rows[j].rc
	})
	fmt.Println()
	fmt.Println("=== metered RC cost per function (free tier = 10,000 RC) ===")
	fmt.Printf("%-14s %-16s %-34s %9s %12s %s\n", "CONTRACT", "ACTION", "NOTE", "RC", "GAS", "FREE TIER")
	var maxRC int64
	for _, r := range rows {
		fit := "ok"
		if r.rc > 10000 {
			fit = "EXCEEDS"
		}
		if r.rc > maxRC {
			maxRC = r.rc
		}
		fmt.Printf("%-14s %-16s %-34s %9d %12d %s\n", r.contract, r.action, r.note, r.rc, r.gas, fit)
	}
	fmt.Printf("\nrows=%d  max=%d RC\n\n", len(rows), maxRC)

	if len(rows) < 35 {
		t.Fatalf("only %d functions profiled — the sweep is not covering the system", len(rows))
	}
}
