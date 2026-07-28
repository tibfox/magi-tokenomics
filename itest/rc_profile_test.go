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
		pC5  = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
		pC6  = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
		pC7  = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"

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
	ct.RegisterContract(pC5, owner, read("../c5-lp/artifacts/main.wasm"))
	ct.RegisterContract(pC6, owner, read("../c6-migration/artifacts/main.wasm"))
	ct.RegisterContract(pC7, owner, read("../c7-yield/artifacts/main.wasm"))

	// ---- setup: the standard deployment sequence -------------------------
	record("C0 token", "init", "", call(t, &ct, pTok, "init",
		`{"name":"RC","symbol":"RC","decimals":0,"maxSupply":"100000000000"}`, owner, 0, true))
	record("C0 token", "mint", "credits the owner", call(t, &ct, pTok, "mint",
		`{"amount":"1000000"}`, owner, 0, true))
	record("C0 token", "transfer", "", call(t, &ct, pTok, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"100000"}`, pC6), owner, 0, true))
	record("C0 token", "transfer", "to a plain account", call(t, &ct, pTok, "transfer",
		fmt.Sprintf(`{"to":"%s","amount":"5000"}`, alice), owner, 0, true))

	record("C6 migration", "init", "", call(t, &ct, pC6, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"100000"}`, pTok), owner, 0, true))
	for _, n := range []int{1, 10, 25, 50} {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			parts[i] = fmt.Sprintf("hive:rcdrop%03d:10", i)
		}
		record("C6 migration", "airdropBatch", fmt.Sprintf("%d recipients", n),
			call(t, &ct, pC6, "airdropBatch",
				fmt.Sprintf(`{"batchId":"%d","entries":"%s"}`, n, strings.Join(parts, ",")), owner, 0, true))
	}

	record("C1 staking", "init", "", call(t, &ct, pC1, "init",
		fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"5","allow":"%s"}`, pTok, owner), owner, 0, true))
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
	record("C1 staking", "claimUnstaked", "after cooldown", call(t, &ct, pC1, "claimUnstaked",
		``, alice, 40, true))
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
		pTok, guard, veto, pC3, pC5, pC7), owner, 40, true))

	distCfg := func(id string) string {
		return fmt.Sprintf(
			`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
				`"reporterAuth":"%s","reporterThreshold":"1","treasury":"%s",`+
				`"guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1"}`,
			pTok, pC2, owner, treas, guard)
	}
	record("C3 content", "init", "", call(t, &ct, pC3, "init", distCfg(pC3), owner, 41, true))
	record("C5 LP", "init", "", call(t, &ct, pC5, "init", distCfg(pC5), owner, 41, true))
	record("C7 yield", "init", "", call(t, &ct, pC7, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","treasury":"%s",`+
			`"guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1"}`,
		pTok, pC2, pC1, treas, guard), owner, 41, true))

	// ---- the recurring per-epoch path ------------------------------------
	record("C2 emission", "distributeEpoch", "1 epoch, 3 buckets", call(t, &ct, pC2, "distributeEpoch",
		``, "hive:keeper", 46, true))
	record("C2 emission", "scheduleInfo", "query", call(t, &ct, pC2, "scheduleInfo", ``, "hive:any", 46, true))
	record("C2 emission", "owedOf", "query", call(t, &ct, pC2, "owedOf",
		fmt.Sprintf(`{"target":"contract:%s","epoch":"0"}`, pC3), "hive:any", 46, true))

	record("C3 content", "pullFunding", "cross-contract claimBucket", call(t, &ct, pC3, "pullFunding",
		`{"epoch":"0"}`, "hive:keeper", 46, true))
	record("C5 LP", "pullFunding", "", call(t, &ct, pC5, "pullFunding",
		`{"epoch":"0"}`, "hive:keeper", 46, true))
	record("C7 yield", "pullFunding", "", call(t, &ct, pC7, "pullFunding",
		`{"epoch":"0"}`, "hive:keeper", 46, true))

	// submitShares is the one that scales with input — measure the curve.
	for _, n := range []int{1, 10, 30, 60} {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			parts[i] = fmt.Sprintf("hive:rcshare%03d:%d", i, 100+i)
		}
		record("C3 content", "submitShares", fmt.Sprintf("%d entries (1 page)", n),
			call(t, &ct, pC3, "submitShares", fmt.Sprintf(
				`{"epoch":"0","page":"%d","entries":"%s"}`, n, strings.Join(parts, ",")), owner, 46, true))
	}
	record("C3 content", "shareOf", "query", call(t, &ct, pC3, "shareOf",
		`{"epoch":"0","account":"hive:rcshare000"}`, "hive:any", 46, true))
	record("C3 content", "finalizeEpoch", "", call(t, &ct, pC3, "finalizeEpoch",
		`{"epoch":"0"}`, owner, 46, true))
	record("C3 content", "claim", "transfers the payout", call(t, &ct, pC3, "claim",
		`{"epoch":"0"}`, "hive:rcshare000", 60, true))

	record("C5 LP", "submitShares", "2 entries", call(t, &ct, pC5, "submitShares",
		fmt.Sprintf(`{"epoch":"0","page":"0","entries":"%s:70,%s:30"}`, alice, bob), owner, 46, true))
	record("C5 LP", "finalizeEpoch", "", call(t, &ct, pC5, "finalizeEpoch", `{"epoch":"0"}`, owner, 46, true))
	record("C5 LP", "claim", "", call(t, &ct, pC5, "claim", `{"epoch":"0"}`, alice, 60, true))

	record("C7 yield", "claim", "reads C1 stake history", call(t, &ct, pC7, "claim",
		`{"epoch":"0"}`, alice, 60, true))
	record("C7 yield", "fundedOf", "query", call(t, &ct, pC7, "fundedOf", `{"epoch":"0"}`, "hive:any", 60, true))

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
