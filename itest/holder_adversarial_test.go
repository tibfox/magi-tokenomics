package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Adversarial scenarios for an ordinary TOKEN HOLDER — someone with no role, no
// ownership, only tokens and the public entrypoints. These are the "out of the
// box" cases: economic games rather than access-control breaks.

const (
	hdTok = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	hdC1  = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	hdC2  = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	hdC3  = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
)

func hdBal(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) *big.Int {
	r := call(t, ct, hdTok, "balanceOf", `{"account":"`+acct+`"}`, "hive:anyone", h, true)
	n := new(big.Int)
	n.SetString(pickJSON(r.Ret, "balance"), 10)
	return n
}

// hdBoot wires token + C2 + C3 + C7 + C1 with epochLen=10 (so hStart != hEnd).
func hdBoot(t *testing.T, ct *test_utils.ContractTest) {
	ct.RegisterContract(hdTok, owner, read(tokenWasmPath))
	ct.RegisterContract(hdC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(hdC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(hdC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	call(t, ct, hdTok, "init", `{"name":"HD","symbol":"HD","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	fundC2Pool(t, ct, hdTok, hdC2, "10000000", 0)
	call(t, ct, hdC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:hdguard","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:hdveto","vetoThreshold":"1","buckets":"author:contract:%s:5000,yield:contract:%s:5000"}`, hdTok, hdC3, hdC1), owner, 0, true)
	call(t, ct, hdC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, hdTok), owner, 0, true)
	call(t, ct, hdC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:hdtreasury","guardianMode":"0","guardianAuth":"hive:hdguard","guardianThreshold":"1"}`, hdTok, hdC2), owner, 0, true)
	call(t, ct, hdC3, "addChannel", `{"channel":"author","bucket":"author","window":"1","reporterMode":"0","reporterAuth":"hive:hdreporter","reporterThreshold":"1"}`, owner, 0, true)
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, ct, hdC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, hdC2), owner, 0, true)
}

func hdStake(t *testing.T, ct *test_utils.ContractTest, who, amt string, h uint64, ok bool) {
	call(t, ct, hdTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"%s"}`, hdC1, amt), who, h, true)
	call(t, ct, hdC1, "stake", fmt.Sprintf(`{"amount":"%s"}`, amt), who, h, ok)
}

// A holder donating tokens straight to a distributor must not inflate anybody's
// claim — payouts come from the on-chain `funded[epoch]` ledger, never from the
// contract's raw balance — and the donation must not become claimable.
func TestHolder_DonationCannotInflateClaims(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	hdBoot(t, &ct)

	call(t, &ct, hdTok, "mint", `{"amount":"50000"}`, owner, 0, true)
	call(t, &ct, hdTok, "transfer", `{"to":"hive:hddonor","amount":"50000"}`, owner, 0, true)
	call(t, &ct, hdTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hdC2), owner, 0, true)

	call(t, &ct, hdC2, "distributeEpoch", ``, "hive:hdkeeper", 10, true)
	call(t, &ct, hdC3, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:hdkeeper", 10, true)

	// the donor dumps tokens directly into C3 hoping to enlarge the pool
	call(t, &ct, hdTok, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"50000"}`, hdC3), "hive:hddonor", 10, true)

	call(t, &ct, hdC3, "submitShares", `{"channel":"author","epoch":"0","page":"0","entries":"hive:hda:1"}`, "hive:hdreporter", 10, true)
	call(t, &ct, hdC3, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:hdreporter", 10, true)
	r := call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hda", 11, true)

	// Emission is 100000/epoch and C3's bucket is 50%, so funded|0 == 50000.
	// The sole claimant must receive exactly that — NOT funded + the 50000 donation.
	assert.Equal(t, "50000", pickJSON(r.Ret, "claimed"), "donation must not inflate the payout")
	// the donation is still sitting in C3, unclaimed and unclaimable by the donor
	call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hddonor", 11, false)
	assert.Equal(t, "0", hdBal(t, &ct, "hive:hddonor", 11).String(), "donor cannot recover the donation")
	assert.Equal(t, "50000", hdBal(t, &ct, "contract:"+hdC3, 11).String(),
		"the donated 50000 remains stranded in C3 — accounting is ledger-based, not balance-based")
}

// Splitting a holding across many accounts must never beat holding it in one —
// floor division means each extra claimant only loses more to truncation.
func TestHolder_SybilSplitNeverBeatsSingleAccount(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	hdBoot(t, &ct)
	call(t, &ct, hdTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hdC2), owner, 0, true)
	call(t, &ct, hdC2, "distributeEpoch", ``, "hive:hdkeeper", 10, true)
	call(t, &ct, hdC3, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:hdkeeper", 10, true)

	// honest whale holds 300 shares in ONE account; sybil holds 300 split 3 ways.
	// funded=25000, totalShares=600 → each share unit is worth 41.66…
	call(t, &ct, hdC3, "submitShares",
		`{"channel":"author","epoch":"0","page":"0","entries":"hive:hdwhale:300,hive:hdsyb1:100,hive:hdsyb2:100,hive:hdsyb3:100"}`,
		"hive:hdreporter", 10, true)
	call(t, &ct, hdC3, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, "hive:hdreporter", 10, true)

	whale := call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hdwhale", 11, true)
	s1 := call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hdsyb1", 11, true)
	s2 := call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hdsyb2", 11, true)
	s3 := call(t, &ct, hdC3, "claim", `{"channel":"author","epoch":"0"}`, "hive:hdsyb3", 11, true)

	w := new(big.Int)
	w.SetString(pickJSON(whale.Ret, "claimed"), 10)
	sy := new(big.Int)
	for _, r := range []string{pickJSON(s1.Ret, "claimed"), pickJSON(s2.Ret, "claimed"), pickJSON(s3.Ret, "claimed")} {
		n := new(big.Int)
		n.SetString(r, 10)
		sy.Add(sy, n)
	}
	t.Logf("whale(1 acct, 300 shares)=%s  sybil(3 accts, 300 shares total)=%s", w, sy)
	assert.LessOrEqual(t, sy.Cmp(w), 0, "splitting must never out-earn a single account")
}

// The known two-point-snapshot gap: a holder staked at BOTH epoch boundaries but
// absent in between. The cooldown makes the tokens un-reusable, so pulling this
// off needs a SECOND stake — i.e. 2x capital for the same reward. Prove it is
// never more profitable than simply staying staked.
func TestHolder_MidEpochExitRestakeIsNotProfitable(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	hdBoot(t, &ct)

	call(t, &ct, hdTok, "mint", `{"amount":"4000"}`, owner, 0, true)
	call(t, &ct, hdTok, "transfer", `{"to":"hive:hdgamer","amount":"2000"}`, owner, 0, true) // needs 2x
	call(t, &ct, hdTok, "transfer", `{"to":"hive:hdhonest","amount":"1000"}`, owner, 0, true)
	call(t, &ct, hdTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hdC2), owner, 0, true)

	// honest: 1000 staked for the whole epoch, capital committed = 1000
	hdStake(t, &ct, "hive:hdhonest", "1000", 0, true)
	// gamer: 1000 at hStart, exits mid-epoch, then stakes a SECOND 1000 before hEnd
	hdStake(t, &ct, "hive:hdgamer", "1000", 0, true)
	call(t, &ct, hdC1, "unstake", `{"amount":"1000"}`, "hive:hdgamer", 3, true)
	hdStake(t, &ct, "hive:hdgamer", "1000", 8, true)

	call(t, &ct, hdC2, "distributeEpoch", ``, "hive:hdkeeper", 10, true)
	call(t, &ct, hdC1, "pullFunding", `{"epoch":"0"}`, "hive:hdkeeper", 10, true)

	g := call(t, &ct, hdC1, "claimYield", `{"epoch":"0"}`, "hive:hdgamer", 11, true)
	h := call(t, &ct, hdC1, "claimYield", `{"epoch":"0"}`, "hive:hdhonest", 11, true)
	gv := new(big.Int)
	gv.SetString(pickJSON(g.Ret, "claimed"), 10)
	hv := new(big.Int)
	hv.SetString(pickJSON(h.Ret, "claimed"), 10)
	t.Logf("gamer(2000 capital, absent mid-epoch)=%s   honest(1000 capital, always staked)=%s", gv, hv)

	// The gamer may match the honest staker, but must never earn MORE PER UNIT OF
	// CAPITAL — they committed 2x to achieve at most the same credit.
	perHonest := new(big.Int).Div(hv, big.NewInt(1000))
	perGamer := new(big.Int).Div(gv, big.NewInt(2000))
	assert.LessOrEqual(t, perGamer.Cmp(perHonest), 0,
		"exit+restake must not beat staying staked, per unit of capital")
}

// One holder's token allowance to C1 must not be usable by anyone else.
func TestHolder_CannotSpendAnotherHoldersAllowance(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	hdBoot(t, &ct)
	call(t, &ct, hdTok, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, &ct, hdTok, "transfer", `{"to":"hive:hdvictim","amount":"1000"}`, owner, 0, true)
	call(t, &ct, hdTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hdC2), owner, 0, true)

	// victim approves C1 (a normal thing to do before staking)
	call(t, &ct, hdTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"1000"}`, hdC1), "hive:hdvictim", 0, true)

	// a thief tries to convert that allowance into stake credited to themselves
	call(t, &ct, hdC1, "stake", `{"amount":"1000"}`, "hive:hdthief", 1, false)                          // pulls from caller
	call(t, &ct, hdC1, "stakeFor", `{"acct":"hive:hdthief","amount":"1000"}`, "hive:hdthief", 1, false) // not allowlisted

	// victim's tokens untouched, thief has nothing staked
	assert.Equal(t, "1000", hdBal(t, &ct, "hive:hdvictim", 2).String(), "victim keeps their tokens")
	st := call(t, &ct, hdC1, "stakeOf", `{"account":"hive:hdthief"}`, "hive:anyone", 2, true)
	assert.Contains(t, st.Ret, `"0"`, "thief has no stake")
}
