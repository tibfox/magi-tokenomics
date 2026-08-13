package itest_test

import (
	"fmt"
	"strings"
	"testing"
)

// What a share page ACTUALLY costs at the shape a 500-user tribe produces.
//
// docs/rc-costs.md records ~8,369 RC for a 60-entry page, measured with short
// synthetic ids. A real epoch's entries are longer on the value side — an account
// that voted 40 posts carries an 18-digit share — and per-entry cost scales with
// BYTES, so the published figure understates a real page.
func TestRC_RealisticSharePageCost(t *testing.T) {
	ct := mdSetup(t)
	call(t, ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, ct, mdDist, "pullFunding", `{"channel":"content","epoch":"0"}`, "hive:anyone", 1, true)

	for _, n := range []int{10, 30, 60} {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			// exactly the shape the scale run emits
			fmt.Fprintf(&b, "hive:u%03d:%d", i, 521226084116000+int64(i))
		}
		r := call(t, ct, mdDist, "submitShares", fmt.Sprintf(
			`{"channel":"content","epoch":"0","page":"%d","entries":"%s"}`, n, b.String()),
			"hive:creporter", 1, true)
		t.Logf("%2d entries (%4d payload bytes): %6d RC", n, b.Len(), r.RcUsed)
	}
}
