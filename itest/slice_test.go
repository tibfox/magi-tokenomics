package itest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// Vertical-slice integration test (plan §19.4): C0 token (unchanged) + C2 emission
// + C3 distributor. Proves C2-as-owner headroom-capped mint, msg.caller inter-
// contract auth (C2→token, C3→C2 claimBucket, C3→token), on-chain funding record,
// contract-computed totalShares, and claim conservation — via the real cross-
// contract engine (test_utils requires vsc1-format contract ids).

const (
	tokenID = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
	c2ID    = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	c3ID    = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
	owner   = "hive:tibfox"
)

const tokenWasmPath = "/mnt/HC_Volume_105012347/magi/testnet/magi_token-contract/test/artifacts/main.wasm"

func read(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}

func TestMain(m *testing.M) {
	_ = os.MkdirAll("data/config", 0755)
	_ = os.WriteFile("data/config/p2pConfig.json",
		[]byte(`{"Port":0,"ServerMode":false,"AllowPrivate":false,"Bootnodes":[],"AnnounceAddrs":[]}`), 0644)
	os.Exit(m.Run())
}

func call(t *testing.T, ct *test_utils.ContractTest, id, action, payload, caller string, height uint64, expectOK bool) test_utils.ContractTestCallResult {
	ct.BlockHeight = height // the env block.height comes from ct.BlockHeight, not tx.Self
	res := ct.Call(stateEngine.TxVscCallContract{
		Caller: caller,
		Self: stateEngine.TxSelf{
			TxId:                 action + "-tx",
			BlockId:              "block1",
			BlockHeight:          height,
			Timestamp:            "2025-09-03T00:00:00",
			RequiredAuths:        []string{caller},
			RequiredPostingAuths: []string{},
		},
		ContractId: id,
		Action:     action,
		Payload:    json.RawMessage(payload),
		RcLimit:    500000,
		Intents:    []contracts.Intent{},
	})
	fmt.Printf("[%s h=%d] ok=%v ret=%s err=%s\n", action, height, res.Success, res.Ret, res.ErrMsg)
	assert.Equal(t, expectOK, res.Success, action+": "+res.Ret+" "+res.ErrMsg)
	return res
}

func TestVerticalSlice(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c3ID, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"author:contract:%s:10000"}`, tokenID, c3ID)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)
	c3init := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID)
	call(t, &ct, c3ID, "init", c3init, owner, 0, true)

	// hand token ownership to C2 so it can mint
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	// keeper pokes epoch 0 at height 1 → emission = 1000000*1/10 = 100000, all to C3
	r := call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	assert.Contains(t, r.Ret, `"1"`) // distributed 1 epoch

	// C3 pulls funding, reporter submits shares + finalizes, alice/bob claim
	call(t, &ct, c3ID, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 1, true)
	call(t, &ct, c3ID, "submitShares", `{"epoch":"0","page":"0","entries":"hive:alice:60,hive:bob:40"}`, "hive:reporter", 1, true)
	call(t, &ct, c3ID, "finalizeEpoch", `{"epoch":"0"}`, "hive:reporter", 1, true)
	call(t, &ct, c3ID, "claim", `{"epoch":"0"}`, "hive:alice", 2, true)
	call(t, &ct, c3ID, "claim", `{"epoch":"0"}`, "hive:bob", 2, true)

	a := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:alice"}`, "hive:x", 2, true)
	b := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:bob"}`, "hive:x", 2, true)
	assert.Contains(t, a.Ret, `"60000"`, "alice should get 60% of 100000")
	assert.Contains(t, b.Ret, `"40000"`, "bob should get 40% of 100000")
}
