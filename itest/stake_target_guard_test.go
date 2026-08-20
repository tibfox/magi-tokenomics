package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
)

// C3's "stakeContract holds a different token" guard must actually fire.
//
// c3-distributor init does:
//
//	si := sdk.ContractCall(sc, "scheduleInfo", "", nil)
//	if tok := pickField(si, "token"); tok != "" && tok != f(payload, "token") { abort }
//
// but C1.scheduleInfo returns only {epochLen, cooldown, genesis} — no `token` — so
// pickField always yields "" and `tok != ""` is never true. The guard has never run.
//
// What it exists to prevent: cfg_stakeContract is pinned at init and immutable, and
// claim's staked half goes out as approve-then-stakeFor. C1's stakeFor pulls ITS OWN
// asset from the caller, so if the two contracts hold different tokens the pull
// finds nothing and every claim carrying a staked portion aborts — permanently, for
// the life of the distributor, with no way to repoint it.
//
// The fix is on C1's side: report the token, so the check that was written to
// compare it has something to compare.

const sgTok2 = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"

// A distributor must refuse a stake target holding a different token.
func TestStakeTargetGuard_MismatchedTokenIsRefused(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(sgTok2, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(spC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(spDist, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"A","symbol":"A","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, sgTok2, "init", `{"name":"B","symbol":"B","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)

	// C1 holds token B
	call(t, &ct, spC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"contract:%s"}`,
		sgTok2, spDist), owner, 0, true)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10","dustBucket":"content","timelock":"1",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"content:contract:%s:10000"}`, tokenID, spDist), owner, 0, true)

	// ...while the distributor pays token A, and names C1 as its stake target
	r := call(t, &ct, spDist, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",`+
			`"stakeContract":"%s","stakedBps":"5000"}`, tokenID, c2ID, spC1), owner, 0, false)
	caFailedFor(t, r, "different token")
}

// The matching case must still deploy — otherwise the guard would just block every
// staked-payout deployment.
func TestStakeTargetGuard_MatchingTokenDeploys(t *testing.T) {
	ct := spSetup(t, "5000")
	// spSetup wires C1 and the distributor on the SAME token and inits both; reaching
	// here at all means the guard did not fire on a correct configuration.
	b := spRunEpoch(t, ct, "hive:alice")
	if b == nil {
		t.Fatal("epoch setup failed")
	}
}
