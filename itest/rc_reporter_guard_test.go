package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"magi_token/reporter/submit"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// The reporter refuses a config whose rc_limit cannot fit a FULL page, because
// pagination emits a short page only at the very end: if a full page exceeds the
// limit then every page but the last reverts, every time, while the cheap calls
// (poke, pull, finalize) all succeed. That check is only as good as the two constants
// it is built from, and those describe the CONTRACT — so they go stale whenever the
// contract's writes or logs change.
//
// They have now gone stale twice. Channel-scoping added a component to every state
// key the call writes; event emission then added a log per page. Each time the
// constants were left behind, and each time the failure is the same shape: the check
// UNDER-estimates a page, a config loads cleanly, and every full page reverts on
// chain having burned its RC.
//
// The second drift was subtler than the first and is why this test exists. The
// constants were 95 RC/entry over a 500 base with a 20% headroom multiplier. After
// event emission a page really cost ~114 RC/entry — and 95 * 1.2 is exactly 114, so
// the check still PASSED while its margin quietly collapsed to a flat 174 RC
// regardless of page size: 0.7% at 200 entries, where 20% was intended. Nothing
// failed, nothing warned, and the guard had stopped guarding.
//
// So this measures the real metered cost and asserts the formula still covers it with
// headroom left over. A future change that makes pages more expensive fails HERE, in
// a second, rather than on a reporter that reverts every page it sends.
func TestRC_ReporterRcLimitFormulaCoversAFullPage(t *testing.T) {
	const (
		guardTok = "vsc1Bd6ZgTRHZQyMXCFYnCbcZaipvHNPd9YHSC"
		guardC2  = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		guardC3  = "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d"
	)
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(guardTok, owner, read(tokenWasmPath))
	ct.RegisterContract(guardC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(guardC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, guardTok, "init",
		`{"name":"G","symbol":"G","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, guardTok, guardC2, "500000000", 0)
	call(t, &ct, guardC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0",`+
			`"vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"content:contract:%s:10000"}`,
		guardTok, guardC3), owner, 0, true)
	call(t, &ct, guardC3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1"}`, guardTok, guardC2), owner, 0, true)
	call(t, &ct, guardC3, "addChannel",
		`{"channel":"content","bucket":"content","window":"1","reporterMode":"0",`+
			`"reporterAuth":"`+owner+`","reporterThreshold":"1"}`, owner, 0, true)
	call(t, &ct, guardC2, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, guardC3, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:keeper", 1, true)

	// Account names are sized like real ones: a page's cost depends on the bytes it
	// writes AND on the bytes it now logs, so short synthetic ids would flatter it.
	for _, n := range []int{1, 10, 30, 60} {
		parts := make([]string, n)
		for i := 0; i < n; i++ {
			parts[i] = fmt.Sprintf("hive:reporterguard%03d:%d", i, 1000+i)
		}
		res := call(t, &ct, guardC3, "submitShares", fmt.Sprintf(
			`{"channel":"content","epoch":"0","page":"%d","entries":"%s"}`, n, strings.Join(parts, ",")),
			owner, 1, true)

		predicted := submit.RCForPage(n)
		actual := int(res.RcUsed)
		t.Logf("page of %3d entries: measured %6d RC, reporter requires %6d (margin %+d, %.1f%%)",
			n, actual, predicted, predicted-actual, 100*float64(predicted-actual)/float64(actual))

		assert.Greaterf(t, predicted, actual,
			"submitShares(%d entries) really costs %d RC but the reporter only demands rc_limit >= %d. "+
				"A config would load and then revert EVERY full page. Re-measure with "+
				"TestRC_ProfileAllFunctions and raise RCPerEntry/RCBase in reporter/submit/rccost.go",
			n, actual, predicted)

		// Covering the cost is not enough — the multiplier has to still MEAN something,
		// or the next small increase silently pushes the estimate under the real cost.
		// This is exactly what the 95/500 constants failed to do while still passing the
		// assertion above.
		minMargin := actual / 10 // at least 10 of the intended 20 percentage points
		assert.GreaterOrEqualf(t, predicted-actual, minMargin,
			"the rc_limit formula covers a %d-entry page by only %d RC (%.1f%%). The headroom "+
				"multiplier exists to absorb metering variance and has been consumed by cost growth; "+
				"raise RCPerEntry/RCBase in reporter/submit/rccost.go to restore it",
			n, predicted-actual, 100*float64(predicted-actual)/float64(actual))
	}
}
