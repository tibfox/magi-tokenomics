package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
)

// What it costs to stand this system up, measured against the free tier.
//
// RC is `ledger HBD + 10,000 free`. A devnet suite that makes no deposit has
// exactly that 10,000 to spend, and when it runs out the symptom is not an error:
// the transaction is simply never applied, and the test times out waiting for
// state that will never appear. magi_tokenomics failed that way — three inits
// landed, addChannel did not — and the cause took a devnet run and a local
// reproduction to find, because nothing reported "out of RC".
//
// This measures the sequence directly. It is a REGRESSION guard on contract size:
// every byte added to a contract raises its init cost, and the setup was already
// within 134 RC of the ceiling.
func TestRCBudget_TokenomicsSetupFitsTheFreeTier(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	own, pre := "hive:magi.test1", "magi.test"
	tk, c2, c3 := rcTok, rcC2, rcC3
	ct.RegisterContract(tk, own, read(tokenWasmPath))
	ct.RegisterContract(c2, own, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c3, own, read("../c3-distributor/artifacts/main.wasm"))

	total := int64(0)
	step := func(id, action, payload string) {
		r := call(t, &ct, id, action, payload, own, 0, true)
		total += int64(r.RcUsed)
		t.Logf("%-12s %7d RC   running total %7d / 10000 free tier", action, r.RcUsed, total)
	}
	step(tk, "init", `{"name":"MAGI","symbol":"MAGI","decimals":0,"maxSupply":"100000000"}`)
	step(c2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","epochLen":"5","baseAnnual":"1000000","blocksPerYear":"100",`+
			`"dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:magi.test1",`+
			`"guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:%s3","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:10000"}`, tk, pre, c3))
	step(c3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:%s4","guardianMode":"0",`+
			`"guardianAuth":"hive:%s3","guardianThreshold":"1"}`, tk, c2, pre, pre))
	step(c3, "addChannel", `{"channel":"author","bucket":"author","window":"1","reporterMode":"0",`+
		`"reporterAuth":"hive:magi.test1","reporterThreshold":"1","role":"content"}`)
	// The suite now deposits, so exceeding the free tier is no longer fatal to it —
	// but crossing this line means any UNFUNDED caller can no longer stand the
	// system up, which is a real property worth knowing about before it is
	// discovered as a silent timeout somewhere else.
	const freeTier = 10000
	if total > freeTier {
		t.Errorf("standing up token + C2 + C3 + one channel costs %d RC, over the %d free tier: "+
			"an account with no HBD can no longer deploy this system, and a devnet suite that "+
			"does not deposit will time out on the call that falls off the end", total, freeTier)
	}
	if margin := freeTier - total; margin < 1000 {
		t.Logf("WARNING: only %d RC of headroom under the free tier — devnet adds "+
			"per-transaction overhead this harness does not, so this is close enough "+
			"to fail there while passing here", margin)
	}
}

const (
	rcTok = "vsc1BqDYVAo1YkZF1gY4SyyH4sVKZpeKnWneu1"
	rcC2  = "vsc1BUFyykfjFfpsfFEfV24CADsBojoLbottH4"
	rcC3  = "vsc1BeuwS3ftGRBgMmW7RCjD6A9s2vTBrKTPJj"
)
