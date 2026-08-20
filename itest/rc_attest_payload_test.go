package itest_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// What the attest-mode payload record ACTUALLY costs in RC, and therefore what a
// collision-resistant hash would actually buy.
//
// The premise being tested: `auth` pins the attested payload byte-for-byte (up to
// 4,096 bytes) because auth.Hash is a 128-bit double-FNV and is documented as not
// collision-resistant. A real digest would let the record be 32 bytes instead.
// README calls that "the real fix" on storage grounds. This measures whether it is
// also an RC argument.
//
// The host prices contract state BY THE BYTE — modules/common/params/params.go:
// WRITE_IO_GAS_RC_COST = 19, READ_IO_GAS_RC_COST = 1 — but only bytes that push the
// contract above its all-time high-water mark pay the write rate. ContractSession.
// IncSize: newWriteGas = max(0, CurrentSize-MaxSize). Bytes reusing space under the
// mark pay the read rate, 19x less. So the answer is not one number.
func TestRC_AttestPayloadCostScalesWithBytes(t *testing.T) {
	var obs [][2]int64 // payload bytes, RC to hold it
	for _, n := range []int{1, 10, 30, 60} {
		_ = os.RemoveAll("data/badger")
		ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			// the shape a live pool emits, same as TestRC_RealisticSharePageCost
			fmt.Fprintf(&b, "hive:u%03d:%d", i, 521226084116000+int64(i))
		}
		payload := fmt.Sprintf(
			`{"channel":"author","epoch":"0","page":"1","entries":"%s"}`, b.String())

		// FIRST attestation: nobody has voted, so this call is the one that writes
		// the payload record. Its RC is the cost of holding the page.
		first := caCall(t, ct, caC3ID, "submitShares", payload, []string{"hive:rep1"}, 1, true)

		// SECOND attestation of the SAME payload: reaches threshold, applies the
		// page, and releases the record. It pays for applying, not for holding.
		second := caCall(t, ct, caC3ID, "submitShares", payload, []string{"hive:rep2"}, 1, true)

		t.Logf("%2d entries (%4d payload bytes): hold %6d RC | apply+release %6d RC",
			n, len(payload), first.RcUsed, second.RcUsed)
		obs = append(obs, [2]int64{int64(len(payload)), first.RcUsed})
	}

	// The marginal cost must track WRITE_IO_GAS_RC_COST = 19 RC/byte. If the host
	// ever stops pricing state by the byte, the whole argument for a smaller record
	// evaporates and this test should say so rather than the doc going quietly stale.
	spanBytes := obs[len(obs)-1][0] - obs[0][0]
	spanRC := obs[len(obs)-1][1] - obs[0][1]
	perByte := float64(spanRC) / float64(spanBytes)
	t.Logf("marginal: %.1f RC per payload byte (host WRITE_IO_GAS_RC_COST is 19)", perByte)
	if perByte < 17 || perByte > 21 {
		t.Errorf("payload bytes cost %.1f RC each, not ~19: the state pricing model changed, "+
			"so docs/rc-costs.md and the hash-size argument need rechecking", perByte)
	}

	// A full page held at the high-water mark costs multiples of the free tier. This
	// is the operational fact attest mode carries and single/cosigned mode does not.
	if obs[len(obs)-1][1] < 10_000 {
		t.Errorf("a 60-entry attest hold now costs %d RC, under the 10,000 free tier — "+
			"if that is real it is good news, but docs/rc-costs.md claims otherwise",
			obs[len(obs)-1][1])
	}
}

// The part that decides whether a 32-byte digest is worth pulling into tinygo.
//
// If holding the payload were a PERMANENT cost, 4,096 -> 32 bytes would save
// 19 RC/byte forever. It is not permanent: the leak fix deletes the record on
// commit, so the space falls back under the high-water mark and the NEXT round
// rewrites it at the read rate. This measures the difference between the round that
// sets the mark and the rounds that reuse it.
func TestRC_AttestHighWaterMarkMakesLaterRoundsCheap(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := caSetupC3(t, "2", "hive:rep1,hive:rep2,hive:rep3", "2")

	entries := func(base int, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "hive:u%03d:%d", base+i, 521226084116000+int64(i))
		}
		return b.String()
	}

	var costs []int64
	for page := 1; page <= 4; page++ {
		payload := fmt.Sprintf(
			`{"channel":"author","epoch":"0","page":"%d","entries":"%s"}`,
			page, entries(page*100, 60))
		hold := caCall(t, ct, caC3ID, "submitShares", payload,
			[]string{"hive:rep1"}, uint64(page), true)
		caCall(t, ct, caC3ID, "submitShares", payload,
			[]string{"hive:rep2"}, uint64(page), true)
		costs = append(costs, hold.RcUsed)
		t.Logf("page %d: holding a 60-entry payload cost %6d RC", page, hold.RcUsed)
	}

	// Page 1 sets the high-water mark. Later pages write the same number of bytes
	// into space page 1's release freed. If they cost the same, the mark is NOT
	// being reused and a smaller record would save RC on every round.
	if costs[3] >= costs[0] {
		t.Errorf("later rounds cost as much as the first (%d vs %d): the release is not "+
			"returning the space, so every round pays the write rate and the record is a "+
			"recurring cost, not a one-off", costs[3], costs[0])
	} else {
		t.Logf("later rounds are %d RC cheaper than the first (%d vs %d): the payload "+
			"record is paid once at the high-water mark, not per round",
			costs[0]-costs[3], costs[0], costs[3])
	}

	// Rounds after the mark is set must be stable — if page 4 drifts above page 2 the
	// space is not being fully reclaimed and the cost creeps with every epoch.
	if costs[3] > costs[1]+200 {
		t.Errorf("round cost is creeping (page 2 %d -> page 4 %d): released space is not "+
			"being fully reused, so attest state grows unboundedly in RC terms",
			costs[1], costs[3])
	}
}
