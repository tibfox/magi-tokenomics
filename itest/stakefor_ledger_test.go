package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// stakeFor must require a ledger-domain account, like every sibling path already does.
//
// It calls validateAddr but not isLedgerAddr. C1's own airdropBatch checks it
// (c1-staking:1372), C1's treasury checks it at init (:209), and C3's applyEntries
// checks it before counting a share (c3-distributor:343) — with a comment explaining
// exactly why: an entry like "alice:100" was counted and then unclaimable forever,
// silently diluting everyone else.
//
// stakeFor has that failure and a worse one. msg.caller is always domain-qualified
// (hive:… / contract:…), so a bare "alice" can never be the caller of unstake — that
// principal is locked permanently. And the amount still lands in total_staked, which
// is the YIELD DENOMINATOR, so every other staker is diluted for the life of the
// deployment by a share nobody can ever claim.
//
// The allowlist is NOT the guard here. The test below allowlists a real account, so
// the call reaches the entrypoint's own validation — otherwise it would pass whether
// or not the domain check exists, which is how this went unnoticed.

const sfC1 = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"

// sfSetup allowlists a HIVE account for stakeFor, so the domain check is what decides.
func sfSetup(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(sfC1, owner, read("../c1-staking/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, sfC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"5","epochLen":"1","allow":"hive:operator"}`,
		tokenID), owner, 0, true)
	call(t, &ct, tokenID, "mint", `{"amount":"5000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:operator","amount":"5000"}`, owner, 0, true)
	call(t, &ct, tokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"5000"}`, sfC1), "hive:operator", 0, true)
	return &ct
}

// THE FINDING: an allowlisted caller, everything else in order, only the domain wrong.
func TestStakeForLedger_BareNameIsRefused(t *testing.T) {
	ct := sfSetup(t)
	r := call(t, ct, sfC1, "stakeFor", `{"acct":"alice","amount":"100"}`, "hive:operator", 1, false)
	caFailedFor(t, r, "ledger address")

	st := call(t, ct, sfC1, "stakeOf", `{"account":"alice"}`, "hive:probe", 2, true)
	assert.NotContains(t, st.Ret, `"100"`, "a bare name must never hold stake")

	total := call(t, ct, sfC1, "totalStaked", ``, "hive:probe", 2, true)
	assert.Contains(t, total.Ret, `"0"`,
		"an unspendable principal must never enter total_staked — it is the yield "+
			"denominator, so it would dilute every other staker permanently")
}

// The same allowlisted caller with a proper domain must still work, or the fix would
// just be "refuse stakeFor".
func TestStakeForLedger_DomainQualifiedStillWorks(t *testing.T) {
	ct := sfSetup(t)
	call(t, ct, sfC1, "stakeFor", `{"acct":"hive:alice","amount":"100"}`, "hive:operator", 1, true)
	st := call(t, ct, sfC1, "stakeOf", `{"account":"hive:alice"}`, "hive:probe", 2, true)
	assert.Contains(t, st.Ret, `"100"`)
	total := call(t, ct, sfC1, "totalStaked", ``, "hive:probe", 2, true)
	assert.Contains(t, total.Ret, `"100"`)
}
