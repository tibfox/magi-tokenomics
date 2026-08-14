package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Full-system devnet scenario:
//   PHASE 1 — deploy + wire the whole framework (token, C1 staking, C2 emission,
//             C3 author-distributor, C7 staking-yield, C6 migration).
//   PHASE 2 — everyone behaves honestly; assert exact expected balances.
//   PHASE 3 — an attacker (`hive:advmallory`) attempts every attack vector the
//             security review identified. EVERY one must fail, and the attacker
//             must end with a zero balance.
//   PHASE 4 — assert system invariants survived the assault.

const (
	advTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	advC1  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	advC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	advC3  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	advC6  = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"

	advMal      = "hive:advmallory"
	advKeeper   = "hive:advkeeper"
	advReporter = "hive:advreporter"
	advGuardian = "hive:advguardian"
	advVeto     = "hive:advveto"
	advTreasury = "hive:advtreasury"
	advAlice    = "hive:advalice"
	advBob      = "hive:advbob"
)

func advBal(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) *big.Int {
	r := call(t, ct, advTok, "balanceOf", `{"account":"`+acct+`"}`, "hive:anyone", h, true)
	n := new(big.Int)
	n.SetString(pickJSON(r.Ret, "balance"), 10)
	return n
}

// pickJSON pulls a quoted field out of a flat JSON response.
func pickJSON(s, field string) string {
	needle := `"` + field + `":"`
	i := 0
	for ; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			j := i + len(needle)
			k := j
			for k < len(s) && s[k] != '"' {
				k++
			}
			return s[j:k]
		}
	}
	return "0"
}

func TestDevnet_HonestThenAdversarial(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(advTok, owner, read(tokenWasmPath))
	ct.RegisterContract(advC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(advC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(advC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	ct.RegisterContract(advC6, owner, read("../c1-staking/artifacts/main.wasm"))

	// ---------------- PHASE 1: deploy + wire ----------------
	// epochLen=10 so C7's hStart(0) != hEnd(9); emission = 1000000*10/100 = 100000/epoch
	call(t, &ct, advTok, "init", `{"name":"ADV","symbol":"ADV","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, advTok, advC2, "10000000", 0)
	call(t, &ct, advC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1","vetoMode":"0","vetoAuth":"%s","vetoThreshold":"1","buckets":"author:contract:%s:6000,yield:contract:%s:4000"}`,
		advTok, advGuardian, advVeto, advC3, advC1), owner, 0, true)
	call(t, &ct, advC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, advTok), owner, 0, true)
	call(t, &ct, advC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","treasury":"%s","guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1"}`,
		advTok, advC2, advTreasury, advGuardian), owner, 0, true)
	call(t, &ct, advC3, "addChannel", `{"channel":"author","bucket":"author","window":"1","reporterMode":"0","reporterAuth":"`+advReporter+`","reporterThreshold":"1"}`, owner, 0, true)
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, advC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, advC2), owner, 0, true)
	call(t, &ct, advC6, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":"","maxAirdrop":"1000"}`,
		advTok), owner, 0, true)

	// bootstrap supply, then hand the token to C2 (the ONLY minter from here on)
	call(t, &ct, advTok, "mint", `{"amount":"3000"}`, owner, 0, true)
	call(t, &ct, advTok, "transfer", fmt.Sprintf(`{"to":"%s","amount":"600"}`, advAlice), owner, 0, true)
	call(t, &ct, advTok, "transfer", fmt.Sprintf(`{"to":"%s","amount":"400"}`, advBob), owner, 0, true)
	call(t, &ct, advTok, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, advC6), owner, 0, true)

	// users stake for the WHOLE of epoch 0 (heights 0..9)
	call(t, &ct, advTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"600"}`, advC1), advAlice, 0, true)
	call(t, &ct, advC1, "stake", `{"amount":"600"}`, advAlice, 0, true)
	call(t, &ct, advTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"400"}`, advC1), advBob, 0, true)
	call(t, &ct, advC1, "stake", `{"amount":"400"}`, advBob, 0, true)

	call(t, &ct, advTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, advC2), owner, 0, true)

	// ---------------- PHASE 2: honest operation ----------------
	call(t, &ct, advC6, "airdropBatch", fmt.Sprintf(`{"batchId":"genesis","entries":"%s:700,%s:300"}`, advAlice, advBob), owner, 1, true)

	call(t, &ct, advC2, "distributeEpoch", ``, advKeeper, 10, true)
	call(t, &ct, advC3, "pullFunding", `{"channel":"author","epoch":"0"}`, advKeeper, 10, true) // 60000
	call(t, &ct, advC1, "pullFunding", `{"epoch":"0"}`, advKeeper, 10, true)                    // 40000
	advBook := publishEntries(t, &ct, advC3, "author", "0",
		fmt.Sprintf("%s:75,%s:25", advAlice, advBob), advReporter, 10)
	call(t, &ct, advC3, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, advReporter, 10, true)

	call(t, &ct, advC3, "claim", advBook.claimFor(t, "author", "0", advAlice), advAlice, 11, true) // 45000
	call(t, &ct, advC3, "claim", advBook.claimFor(t, "author", "0", advBob), advBob, 11, true)     // 15000
	call(t, &ct, advC1, "claimYield", `{"epoch":"0"}`, advAlice, 11, true)               // 24000
	call(t, &ct, advC1, "claimYield", `{"epoch":"0"}`, advBob, 11, true)                 // 16000

	// airdrop 700 + author 45000 + yield 24000 = 69700 (600 is staked in C1)
	assert.Equal(t, "69700", advBal(t, &ct, advAlice, 11).String(), "alice honest total")
	assert.Equal(t, "31300", advBal(t, &ct, advBob, 11).String(), "bob honest total")

	// ---------------- PHASE 3: the attacker tries everything ----------------
	F := func(id, action, payload, caller string, h uint64) {
		call(t, &ct, id, action, payload, caller, h, false)
	}
	// 1. mint directly (not the owner)
	F(advTok, "mint", `{"amount":"1000000"}`, advMal, 12)
	// 2. seize token ownership
	F(advTok, "changeOwner", fmt.Sprintf(`{"newOwner":"%s"}`, advMal), advMal, 12)
	// 3. impersonate a bucket target to pull emission
	F(advC2, "claimBucket", `{"epoch":"0"}`, advMal, 12)
	// 4. re-init contracts to seize config
	F(advC2, "init", `{"token":"x"}`, advMal, 12)
	F(advC1, "init", `{"token":"x"}`, advMal, 12)
	F(advC3, "init", `{"token":"x"}`, advMal, 12)
	// 5. push fraudulent reward shares
	F(advC3, "submitShares", fmt.Sprintf(`{"channel":"author","epoch":"1","page":"0","entries":"%s:999999"}`, advMal), advMal, 12)
	// 6. finalize / veto without the role
	F(advC3, "finalizeEpoch", `{"channel":"author","epoch":"1"}`, advMal, 12)
	F(advC3, "cancelEpoch", `{"channel":"author","epoch":"0"}`, advMal, 12)
	// 7. sweep the distributor to himself
	F(advC3, "sweepUnallocated", fmt.Sprintf(`{"channel":"author","nonce":"1","to":"%s"}`, advMal), advMal, 12)
	F(advC1, "sweepResidual", `{"epoch":"0"}`, advMal, 12)
	// 8. claim rewards he has no share of / double-claim
	F(advC3, "claim", `{"channel":"author","epoch":"0"}`, advMal, 12)
	F(advC3, "claim", `{"channel":"author","epoch":"0"}`, advAlice, 12) // alice already claimed
	F(advC1, "claimYield", `{"epoch":"0"}`, advMal, 12)
	F(advC1, "claimYield", `{"epoch":"0"}`, advBob, 12) // bob already claimed
	// 9. non-canonical epoch aliasing (strand/divert funding)
	F(advC3, "pullFunding", `{"channel":"author","epoch":"00"}`, advMal, 12)
	F(advC3, "pullFunding", `{"channel":"author","epoch":"18446744073709551616"}`, advMal, 12)
	// 10. steal another user's stake
	F(advC1, "unstake", `{"amount":"600"}`, advMal, 12)
	F(advC1, "stakeFor", fmt.Sprintf(`{"acct":"%s","amount":"500"}`, advMal), advMal, 12)
	// 11. hijack the token via the guardian passthrough
	F(advC2, "queueTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"%s"}`, advMal), advMal, 12)
	// ...and against a REAL op the guardian legitimately queued: the attacker may
	// neither veto it (veto-only) nor execute it (guardian/veto-only). The veto
	// then cancels it, proving the escape hatch works and the op dies.
	legitOp := `{"op":"pause","nonce":"7"}`
	call(t, &ct, advC2, "queueTokenOp", legitOp, advGuardian, 12, true)
	F(advC2, "cancelTokenOp", legitOp, advMal, 13)  // not the veto
	F(advC2, "executeTokenOp", legitOp, advMal, 18) // not guardian/veto
	call(t, &ct, advC2, "cancelTokenOp", legitOp, advVeto, 13, true)
	F(advC2, "executeTokenOp", legitOp, advGuardian, 18) // cancelled → gone
	// 12. drain the migration contract
	F(advC6, "airdropBatch", fmt.Sprintf(`{"batchId":"steal","entries":"%s:300"}`, advMal), advMal, 12)
	// 13. posting-key privilege escalation (runtime derives msg.caller from posting auths)
	pvCallPosting(t, &ct, advC3, "submitShares",
		fmt.Sprintf(`{"epoch":"1","page":"0","entries":"%s:1"}`, advMal), advReporter, 12, false)
	pvCallPosting(t, &ct, advC6, "airdropBatch",
		fmt.Sprintf(`{"batchId":"p","entries":"%s:100"}`, advMal), owner, 12, false)
	pvCallPosting(t, &ct, advC2, "queueTokenOp",
		fmt.Sprintf(`{"op":"changeOwner","nonce":"9","newOwner":"%s"}`, advMal), advGuardian, 12, false)

	// 14. flash-stake to capture a full epoch of yield he wasn't committed to
	call(t, &ct, advTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"69700"}`, advC1), advAlice, 12, true)
	call(t, &ct, advC1, "stake", `{"amount":"5000"}`, advAlice, 19, true) // joins at the END of epoch 1
	call(t, &ct, advC2, "distributeEpoch", ``, advKeeper, 20, true)
	call(t, &ct, advC1, "pullFunding", `{"epoch":"1"}`, advKeeper, 20, true)
	// alice's epoch-1 credit is min(stake@h10, stake@h19) = 600 — the 5000 late top-up
	// must NOT count, and must not dilute bob either.
	rA := call(t, &ct, advC1, "claimYield", `{"epoch":"1"}`, advAlice, 21, true)
	rB := call(t, &ct, advC1, "claimYield", `{"epoch":"1"}`, advBob, 21, true)
	assert.Equal(t, "24000", pickJSON(rA.Ret, "claimed"), "flash-stake must not inflate alice")
	assert.Equal(t, "16000", pickJSON(rB.Ret, "claimed"), "bob must not be diluted")

	// 15. execute a token op nobody legitimately queued
	F(advC2, "executeTokenOp", fmt.Sprintf(`{"op":"changeOwner","nonce":"1","newOwner":"%s"}`, advMal), advMal, 30)

	// ---------------- PHASE 4: invariants survived ----------------
	assert.Equal(t, "0", advBal(t, &ct, advMal, 31).String(), "ATTACKER MUST HOLD NOTHING")

	// token ownership never moved
	own := call(t, &ct, advTok, "getOwner", ``, "hive:anyone", 31, true)
	assert.Contains(t, own.Ret, "contract:"+advC2, "token owner must still be C2")

	// C1 custody intact: alice 600+5000, bob 400
	st := call(t, &ct, advC1, "totalStaked", ``, "hive:anyone", 31, true)
	assert.Equal(t, "6000", pickJSON(st.Ret, "total"), "total staked")
	c1bal := advBal(t, &ct, "contract:"+advC1, 31)
	assert.Equal(t, "6000", c1bal.String(), "C1 token custody == totalStaked")

	// supply is exactly bootstrap(3000) + 2 epochs of emission(200000)
	sup := call(t, &ct, advTok, "totalSupply", ``, "hive:anyone", 31, true)
	assert.Equal(t, "10003000", pickJSON(sup.Ret, "totalSupply"), "no unscheduled minting occurred (3000 bootstrap + the 10000000 pool; epoch emission now comes OUT of that pool rather than adding to supply)")

	// C3 never paid out more than it was funded
	si := call(t, &ct, advC3, "shareOf", fmt.Sprintf(`{"channel":"author","epoch":"0","account":"%s"}`, advAlice), "hive:anyone", 31, true)
	assert.Equal(t, "60000", pickJSON(si.Ret, "funded"), "C3 epoch-0 funding untouched by the attack")
}
