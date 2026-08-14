package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"magi_token/reporter/vscapi"

	"vsc-node/lib/test_utils"
)

// The reporter's stake reader against the contract's own answer.
//
// vscapi.StakeSource reimplements C1's searchVal in Go, and says of itself that it
// "MUST stay bug-for-bug identical to the contract". Until now the only evidence
// for that was a synthetic history in its unit tests — which proves the Go code
// matches the Go author's belief about the contract, not the contract. Under
// `weight: token_stake` every vote is priced by this lookup, so a divergence
// silently changes payouts and nothing anywhere reports it.
//
// Here both read the SAME state: the reader is pointed at the live contract's
// state through ct.StateGet, and its answer is compared against what C1's own
// stakeAtHeight returns for the same account and height.

const (
	parityTok = "vsc1BqE7hMxLpVdNs3RkYcTfZaWuJ2gPnH64Kx"
	parityC1  = "vsc1BmT4nWsKdGxQvZ8FyRbLpUcAe5JhN27Vqs"
)

// ctStateReader adapts the contract test harness to the single capability the
// reporter's reader needs. This is the seam under test: in production the same
// interface is served by a VSC node's GraphQL state endpoint.
type ctStateReader struct {
	t  *testing.T
	ct *test_utils.ContractTest
}

func (r ctStateReader) StateGetOne(contractID, key string) (string, bool, error) {
	v := strings.Trim(r.ct.StateGet(contractID, key), `"`)
	if v == "" {
		return "", false, nil
	}
	return v, true, nil
}

func TestStakeReaderAgreesWithTheContractAtEveryHeight(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(parityTok, owner, read(tokenWasmPath))
	ct.RegisterContract(parityC1, owner, read("../c1-staking/artifacts/main.wasm"))

	call(t, &ct, parityTok, "init",
		`{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, parityC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":"",`+
			`"treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1"}`, parityTok), owner, 0, true)

	// A history with the shapes that break naive searches: repeated mutations in
	// ONE block (only the last is reachable), long gaps between heights, a stake
	// that goes to zero and comes back, and an account that never stakes at all.
	stakers := []string{"hive:alpha", "hive:bravo", "hive:charlie"}
	call(t, &ct, parityTok, "mint", `{"amount":"300000"}`, owner, 0, true)
	for _, s := range stakers {
		call(t, &ct, parityTok, "transfer",
			fmt.Sprintf(`{"to":"%s","amount":"100000"}`, s), owner, 0, true)
	}
	type mut struct {
		acct   string
		height uint64
		action string
		amount string
	}
	muts := []mut{
		{"hive:alpha", 5, "stake", "100"},
		{"hive:alpha", 5, "stake", "50"},   // same block again — only the last is reachable
		{"hive:bravo", 5, "stake", "700"},  // same block, different account
		{"hive:alpha", 40, "stake", "25"},  // a long gap
		{"hive:bravo", 41, "unstake", "700"}, // drops to zero
		{"hive:alpha", 42, "unstake", "175"}, // alpha to zero too
		{"hive:alpha", 90, "stake", "9"},   // and back up after zero
		{"hive:bravo", 91, "stake", "1"},
	}
	for _, m := range muts {
		if m.action == "stake" {
			call(t, &ct, parityTok, "approve", fmt.Sprintf(
				`{"spender":"contract:%s","amount":"%s"}`, parityC1, m.amount), m.acct, m.height, true)
		}
		call(t, &ct, parityC1, m.action, fmt.Sprintf(`{"amount":"%s"}`, m.amount), m.acct, m.height, true)
	}

	reader := vscapi.NewStakeSource(ctStateReader{t, &ct}, parityC1)

	// Probe every height across and beyond the history, including before the first
	// mutation and after the last. Off-by-one at a boundary is the whole failure
	// mode: searchVal takes the RIGHTMOST entry with height <= target.
	probes := []uint64{0, 1, 4, 5, 6, 39, 40, 41, 42, 43, 89, 90, 91, 92, 500}
	compared := 0
	for _, acct := range append(stakers, "hive:never-staked") {
		for _, h := range probes {
			want := contractStakeAtHeight(t, &ct, parityC1, acct, h)
			got, err := reader.StakeAtHeight(acct, h)
			if err != nil {
				t.Fatalf("reader failed for %s at height %d: %v", acct, h, err)
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("DIVERGENCE at %s height %d: the contract says %s, the reporter's "+
					"reader says %s.\n  Under weight: token_stake this prices every vote, so "+
					"the reporter would compute shares the chain would never agree with.",
					acct, h, want, got)
			}
			compared++
		}
	}
	t.Logf("reader and contract agree on %d (account, height) probes", compared)
}

// contractStakeAtHeight asks C1 itself, so the expectation comes from the wasm
// rather than from a second Go implementation of the same search.
func contractStakeAtHeight(t *testing.T, ct *test_utils.ContractTest,
	c1, acct string, height uint64) *big.Int {
	t.Helper()
	res := call(t, ct, c1, "stakeAtHeight",
		fmt.Sprintf(`{"account":"%s","height":"%d"}`, acct, height), "hive:anyone", 600, true)
	raw := res.Ret
	// {"stake":"123"} — pull the number out without pulling in a JSON dependency
	i := strings.Index(raw, `:"`)
	j := strings.LastIndex(raw, `"`)
	if i < 0 || j <= i+2 {
		t.Fatalf("stakeAtHeight returned %q, which this test cannot read", raw)
	}
	v, ok := new(big.Int).SetString(raw[i+2:j], 10)
	if !ok {
		t.Fatalf("stakeAtHeight returned a non-numeric stake: %q", raw)
	}
	return v
}
