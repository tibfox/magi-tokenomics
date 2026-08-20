package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// An ATTACKER deploys their own contract and drives it against the framework.
// Threat model: the deployer/owner is trusted; the adversary is an outsider or a
// non-owner token holder — who may of course deploy contracts of their own.
//
// The point: calling from a contract context makes msg.caller "contract:<id>",
// a namespace disjoint from every hive: account and every configured role. A
// contract caller must therefore have NO power an ordinary account lacks.

const (
	hosTok  = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	hosC1   = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	hosC2   = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	hosC3   = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	hosEvil = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3" // attacker-deployed
	hosMal  = "hive:hosmallory"
)

func TestHostile_AttackerContractHasNoPrivilege(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(hosTok, owner, read(tokenWasmPath))
	ct.RegisterContract(hosC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(hosC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(hosC3, owner, read("../c3-distributor/artifacts/main.wasm"))
	// the attacker deploys their own contract (owner is the attacker, not us)
	ct.RegisterContract(hosEvil, hosMal, read("../hostile/artifacts/main.wasm"))

	call(t, &ct, hosTok, "init", `{"name":"H","symbol":"H","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, hosTok, hosC2, "10000000", 0)
	call(t, &ct, hosC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:hosveto","vetoThreshold":"1","buckets":"author:contract:%s:6000,yield:contract:%s:4000"}`, hosTok, hosC3, hosC1), owner, 0, true)
	call(t, &ct, hosC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, hosTok), owner, 0, true)
	call(t, &ct, hosC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","treasury":"hive:hostreasury","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1"}`, hosTok, hosC2), owner, 0, true)
	call(t, &ct, hosC3, "addChannel", `{"channel":"author","bucket":"author","window":"1","reporterMode":"0","reporterAuth":"hive:hosreporter","reporterThreshold":"1"}`, owner, 0, true)
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, hosC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, hosC2), owner, 0, true)
	call(t, &ct, hosTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hosC2), owner, 0, true)

	// fund the system so there is something worth stealing
	call(t, &ct, hosC2, "distributeEpoch", ``, "hive:hoskeeper", 10, true)
	call(t, &ct, hosC3, "pullFunding", `{"channel":"author","epoch":"0"}`, "hive:hoskeeper", 10, true)
	call(t, &ct, hosC1, "pullFunding", `{"epoch":"0"}`, "hive:hoskeeper", 10, true)

	// relay(target, method, payload) — inner JSON is escaped so the hostile
	// contract can forward it verbatim.
	relay := func(target, method, inner string, h uint64) {
		esc := ""
		for _, r := range inner {
			if r == '"' {
				esc += `\"`
			} else {
				esc += string(r)
			}
		}
		p := fmt.Sprintf(`{"target":"%s","method":"%s","payload":"%s"}`, target, method, esc)
		call(t, &ct, hosEvil, "relay", p, hosMal, h, false) // callee abort ⇒ whole tx reverts
	}

	// --- the attacker's contract tries every privileged path ---
	relay(hosC2, "claimBucket", `{"epoch":"0"}`, 12)                                                     // impersonate a bucket target
	relay(hosC3, "claim", `{"channel":"author","epoch":"0"}`, 12)                                        // claim with no share
	relay(hosC1, "claimYield", `{"epoch":"0"}`, 12)                                                      // claim with no stake
	relay(hosC3, "submitShares", `{"channel":"author","epoch":"0","page":"0","entries":"hive:x:1"}`, 12) // not the reporter
	relay(hosC3, "finalizeEpoch", `{"channel":"author","epoch":"0"}`, 12)                                // not the reporter
	relay(hosC3, "cancelEpoch", `{"channel":"author","epoch":"0"}`, 12)                                  // not the guardian
	relay(hosC3, "sweepUnallocated", `{"channel":"author","nonce":"1","amount":"1"}`, 12)                             // not the guardian
	relay(hosC1, "sweepResidual", `{"epoch":"0"}`, 12)                                                   // not the guardian
	relay(hosC1, "stakeFor", `{"acct":"hive:hosmallory","amount":"100"}`, 12)                            // not allowlisted
	relay(hosC1, "unstake", `{"amount":"100"}`, 12)                                                      // no stake of its own
	relay(hosC2, "queueTokenOp", `{"op":"changeOwner","nonce":"1","newOwner":"hive:hosmallory"}`, 12)    // not the guardian
	relay(hosTok, "mint", `{"amount":"1000000"}`, 12)                                                    // not the token owner
	relay(hosTok, "changeOwner", `{"newOwner":"hive:hosmallory"}`, 12)                                   // not the token owner
	relay(hosC2, "init", `{"token":"x"}`, 12)                                                            // re-init
	relay(hosC3, "init", `{"token":"x"}`, 12)                                                            // re-init

	// reentrancy attempt: same privileged call twice inside one tx
	call(t, &ct, hosEvil, "reenter",
		fmt.Sprintf(`{"target":"%s","method":"claimBucket","payload":"{\"epoch\":\"0\"}"}`, hosC2), hosMal, 12, false)

	// --- nothing moved ---
	for _, acct := range []string{hosMal, "contract:" + hosEvil} {
		b := call(t, &ct, hosTok, "balanceOf", `{"account":"`+acct+`"}`, "hive:anyone", 13, true)
		assert.Contains(t, b.Ret, `"0"`, acct+" must hold nothing")
	}
	// distributor funding untouched
	si := call(t, &ct, hosC3, "shareOf", `{"channel":"author","epoch":"0","account":"hive:none"}`, "hive:anyone", 13, true)
	assert.Contains(t, si.Ret, `"funded":"60000"`, "C3 funding untouched")
	// token still owned by C2
	own := call(t, &ct, hosTok, "getOwner", ``, "hive:anyone", 13, true)
	assert.Contains(t, own.Ret, "contract:"+hosC2, "token owner unchanged")
}

// CROSS-CONTRACT COMPOSITION IN ONE TRANSACTION.
//
// The adversary previously had `relay` (one contract) and `reenter` (the same
// contract twice), so nothing could compose across two DIFFERENT framework contracts
// atomically — which is precisely where this system's time-based invariants meet. C7
// credits min(stakeAt(hStart), stakeAt(hEnd)) by reading C1 live, and C1 checkpoints
// at blockHeight(). Both are reachable in one tx, and within a tx the height cannot
// advance, so any sequence that works only because two contracts disagree about "now"
// would surface here.
//
// No exploit is expected. The point is that the tooling to look for one now exists.
func TestHostile_CrossContractCompositionInOneTx(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(hosTok, owner, read(tokenWasmPath))
	ct.RegisterContract(hosC1, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(hosC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(hosEvil, hosMal, read("../hostile/artifacts/main.wasm"))

	call(t, &ct, hosTok, "init", `{"name":"H","symbol":"H","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, hosTok, hosC2, "10000000", 0)
	call(t, &ct, hosC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"yield","timelock":"5","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:hosveto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, hosTok, hosC1), owner, 0, true)
	call(t, &ct, hosC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, hosTok), owner, 0, true)
	// C7 requires its stakeSource to have adopted the emission schedule:
	// without it C1 records no drawdowns and the yield denominator over-counts.
	call(t, &ct, hosC1, "adoptSchedule", fmt.Sprintf(`{"funder":"%s","bucket":"yield"}`, hosC2), owner, 0, true)

	// give the attacker's CONTRACT real tokens and a real approval, so the composed
	// legs are individually legitimate — the question is whether the SEQUENCE is.
	evil := "contract:" + hosEvil
	call(t, &ct, hosTok, "mint", `{"amount":"5000"}`, owner, 0, true)
	call(t, &ct, hosTok, "transfer", fmt.Sprintf(`{"to":"%s","amount":"5000"}`, evil), owner, 0, true)
	call(t, &ct, hosTok, "approve", fmt.Sprintf(`{"spender":"contract:%s","amount":"5000"}`, hosC1), evil, 0, true)

	// fund epoch 0's yield bucket
	call(t, &ct, hosC2, "distributeEpoch", ``, "hive:hoskeeper", 10, true)
	call(t, &ct, hosC1, "pullFunding", `{"epoch":"0"}`, "hive:hoskeeper", 10, true)

	esc := func(s string) string {
		out := ""
		for _, r := range s {
			if r == '"' {
				out += `\"`
			} else {
				out += string(r)
			}
		}
		return out
	}
	compose := func(h uint64, expectOK bool, legs ...[3]string) {
		p := "{"
		for i, l := range legs {
			if i > 0 {
				p += ","
			}
			p += fmt.Sprintf(`"t%d":"%s","m%d":"%s","p%d":"%s"`, i+1, l[0], i+1, l[1], i+1, esc(l[2]))
		}
		p += "}"
		call(t, &ct, hosEvil, "compose", p, hosMal, h, expectOK)
	}

	// 1. stake and claim the SAME epoch's yield in one tx. Both legs see the same
	//    height, so if C7 credited the end boundary alone this would mint yield from
	//    stake that existed for zero blocks. min(start,end) must make it worthless.
	// The attacker starts holding the 5000 it was given; a reverted composition must
	// leave exactly that, and a successful exploit would leave MORE.
	before := covBalanceOf(t, &ct, hosTok, evil, 11)
	assert.Equal(t, "5000", before.String(), "fixture: the attacker contract holds its float")
	compose(12, false,
		[3]string{hosC1, "stake", `{"amount":"5000"}`},
		[3]string{hosC1, "claimYield", `{"epoch":"0"}`},
	)
	assert.Equal(t, "5000", covBalanceOf(t, &ct, hosTok, evil, 12).String(),
		"a stake and a claim in one tx must not pay the attacker, and the revert must "+
			"leave the float untouched")

	// 2. stake in one tx (legitimately), then in a LATER single tx claim and unstake
	//    together — an attempt to be paid for an epoch and exit within it.
	call(t, &ct, hosC1, "stake", `{"amount":"5000"}`, evil, 11, true)
	compose(13, false,
		[3]string{hosC1, "claimYield", `{"epoch":"0"}`},
		[3]string{hosC1, "unstake", `{"amount":"5000"}`},
	)

	// 3. the reverse order — exit first, then claim, in one atomic tx.
	compose(14, false,
		[3]string{hosC1, "unstake", `{"amount":"5000"}`},
		[3]string{hosC1, "claimYield", `{"epoch":"0"}`},
	)

	// The stake moved the float into C1, so the liquid balance is now zero and must
	// STAY zero: any yield paid by a composition would show up right here.
	assert.Equal(t, "0", covBalanceOf(t, &ct, hosTok, evil, 15).String(),
		"no composition may pay the attacker's contract any yield")
	r := call(t, &ct, hosC1, "stakeOf", fmt.Sprintf(`{"account":"%s"}`, evil), "hive:reader", 15, true)
	assert.Contains(t, r.Ret, `"stake":"5000"`, "a reverted composition must not have moved the stake")
}

// covBalanceOf reads a token balance for an arbitrary account against a given token.
func covBalanceOf(t *testing.T, ct *test_utils.ContractTest, token, acct string, h uint64) *big.Int {
	r := call(t, ct, token, "balanceOf", `{"account":"`+acct+`"}`, "hive:reader", h, true)
	return cvBig(cvField(r.Ret, "balance"))
}
