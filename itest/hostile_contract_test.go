package itest_test

import (
	"fmt"
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
	hosC7   = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
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
	ct.RegisterContract(hosC7, owner, read("../c7-yield/artifacts/main.wasm"))
	// the attacker deploys their own contract (owner is the attacker, not us)
	ct.RegisterContract(hosEvil, hosMal, read("../hostile/artifacts/main.wasm"))

	call(t, &ct, hosTok, "init", `{"name":"H","symbol":"H","decimals":0,"maxSupply":"100000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, hosTok, hosC2, "10000000", 0)
	call(t, &ct, hosC2, "init", fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"10","baseAnnual":"1000000","blocksPerYear":"100","dustBucket":"author","timelock":"5","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:hosveto","vetoThreshold":"1","buckets":"author:contract:%s:6000,yield:contract:%s:4000"}`, hosTok, hosC3, hosC7), owner, 0, true)
	call(t, &ct, hosC1, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"20","epochLen":"10","allow":""}`, hosTok), owner, 0, true)
	call(t, &ct, hosC3, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:hosreporter","reporterThreshold":"1","treasury":"hive:hostreasury","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1"}`, hosTok, hosC2), owner, 0, true)
	call(t, &ct, hosC7, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","genesis":"0","epochLen":"10","treasury":"hive:hostreasury","guardianMode":"0","guardianAuth":"hive:hosguardian","guardianThreshold":"1"}`, hosTok, hosC2, hosC1), owner, 0, true)
	call(t, &ct, hosTok, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, hosC2), owner, 0, true)

	// fund the system so there is something worth stealing
	call(t, &ct, hosC2, "distributeEpoch", ``, "hive:hoskeeper", 10, true)
	call(t, &ct, hosC3, "pullFunding", `{"epoch":"0"}`, "hive:hoskeeper", 10, true)
	call(t, &ct, hosC7, "pullFunding", `{"epoch":"0"}`, "hive:hoskeeper", 10, true)

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
	relay(hosC2, "claimBucket", `{"epoch":"0"}`, 12)                                                  // impersonate a bucket target
	relay(hosC3, "claim", `{"epoch":"0"}`, 12)                                                        // claim with no share
	relay(hosC7, "claim", `{"epoch":"0"}`, 12)                                                        // claim with no stake
	relay(hosC3, "submitShares", `{"epoch":"0","page":"0","entries":"hive:x:1"}`, 12)                 // not the reporter
	relay(hosC3, "finalizeEpoch", `{"epoch":"0"}`, 12)                                                // not the reporter
	relay(hosC3, "cancelEpoch", `{"epoch":"0"}`, 12)                                                  // not the guardian
	relay(hosC3, "sweepUnallocated", `{"nonce":"1"}`, 12)                                             // not the guardian
	relay(hosC7, "sweepResidual", `{"epoch":"0"}`, 12)                                                // not the guardian
	relay(hosC1, "stakeFor", `{"acct":"hive:hosmallory","amount":"100"}`, 12)                         // not allowlisted
	relay(hosC1, "unstake", `{"amount":"100"}`, 12)                                                   // no stake of its own
	relay(hosC2, "queueTokenOp", `{"op":"changeOwner","nonce":"1","newOwner":"hive:hosmallory"}`, 12) // not the guardian
	relay(hosTok, "mint", `{"amount":"1000000"}`, 12)                                                 // not the token owner
	relay(hosTok, "changeOwner", `{"newOwner":"hive:hosmallory"}`, 12)                                // not the token owner
	relay(hosC2, "init", `{"token":"x"}`, 12)                                                         // re-init
	relay(hosC3, "init", `{"token":"x"}`, 12)                                                         // re-init

	// reentrancy attempt: same privileged call twice inside one tx
	call(t, &ct, hosEvil, "reenter",
		fmt.Sprintf(`{"target":"%s","method":"claimBucket","payload":"{\"epoch\":\"0\"}"}`, hosC2), hosMal, 12, false)

	// --- nothing moved ---
	for _, acct := range []string{hosMal, "contract:" + hosEvil} {
		b := call(t, &ct, hosTok, "balanceOf", `{"account":"`+acct+`"}`, "hive:anyone", 13, true)
		assert.Contains(t, b.Ret, `"0"`, acct+" must hold nothing")
	}
	// distributor funding untouched
	si := call(t, &ct, hosC3, "shareOf", `{"epoch":"0","account":"hive:none"}`, "hive:anyone", 13, true)
	assert.Contains(t, si.Ret, `"funded":"60000"`, "C3 funding untouched")
	// token still owned by C2
	own := call(t, &ct, hosTok, "getOwner", ``, "hive:anyone", 13, true)
	assert.Contains(t, own.Ret, "contract:"+hosC2, "token owner unchanged")
}
