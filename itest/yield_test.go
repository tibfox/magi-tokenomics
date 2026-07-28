package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Staking-yield slice (plan Profile-1 trustless core): token + C1 staking + C2
// emission + C7 yield. Proves allowance staking, height-checkpointed stake reads,
// and pro-rata trustless yield (no reporter). alice stakes 600, bob 400; one epoch
// mints 100000 all to C7; claims split 60000/40000 by stake.
func TestStakingYieldSlice(t *testing.T) {
	const (
		c1ID = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
		c7ID = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	)
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c1ID, owner, read("../c1-staking/artifacts/main.wasm"))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c7ID, owner, read("../c7-yield/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c1ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","cooldown":"2","epochLen":"1","allow":""}`, tokenID), owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"yield","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"yield:contract:%s:10000"}`, tokenID, c7ID)
	fundC2Pool(t, &ct, tokenID, c2ID, "500000000", 0)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)
	call(t, &ct, c7ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","stakeSource":"%s","genesis":"0","epochLen":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID, c1ID), owner, 0, true)

	// mint 1000 to owner, hand out to alice/bob (all before ownership handover)
	call(t, &ct, tokenID, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:alice","amount":"600"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", `{"to":"hive:bob","amount":"400"}`, owner, 0, true)

	// alice/bob approve C1 and stake (at height 0 → snapshot h_e=0 for epoch 0)
	c1spender := "contract:" + c1ID
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"%s","amount":"600"}`, c1spender), "hive:alice", 0, true)
	call(t, &ct, c1ID, "stake", `{"amount":"600"}`, "hive:alice", 0, true)
	call(t, &ct, tokenID, "approve", fmt.Sprintf(`{"spender":"%s","amount":"400"}`, c1spender), "hive:bob", 0, true)
	call(t, &ct, c1ID, "stake", `{"amount":"400"}`, "hive:bob", 0, true)

	// hand token ownership to C2
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	// keeper pokes epoch 0 (h=1) → 100000 minted, all to C7; C7 pulls it
	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, c7ID, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 1, true)

	// pro-rata yield claims (h_e for epoch 0 = 0; stakes recorded at height 0)
	ca := call(t, &ct, c7ID, "claim", `{"epoch":"0"}`, "hive:alice", 2, true)
	cb := call(t, &ct, c7ID, "claim", `{"epoch":"0"}`, "hive:bob", 2, true)
	assert.Contains(t, ca.Ret, `"60000"`)
	assert.Contains(t, cb.Ret, `"40000"`)

	// verify staking read + balances
	sa := call(t, &ct, c1ID, "stakeAtHeight", `{"account":"hive:alice","height":"0"}`, "hive:x", 1, true)
	assert.Contains(t, sa.Ret, `"600"`)
	a := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:alice"}`, "hive:x", 2, true)
	b := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:bob"}`, "hive:x", 2, true)
	assert.Contains(t, a.Ret, `"60000"`, "alice: 0 liquid after staking + 60000 yield")
	assert.Contains(t, b.Ret, `"40000"`)
}
