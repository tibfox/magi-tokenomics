package itest_test

import (
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// C5 LP-reward slice: identical distribution flow as C3 but via the C5 contract
// (LP reporter pushes per-provider shares). token + C2 + C5.
func TestC5LPSlice(t *testing.T) {
	const c5ID = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c2ID, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(c5ID, owner, read("../c5-lp/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	c2init := fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"lp","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1","buckets":"lp:contract:%s:10000"}`, tokenID, c5ID)
	call(t, &ct, c2ID, "init", c2init, owner, 0, true)
	c5init := fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0","reporterAuth":"hive:lpreporter","reporterThreshold":"1","treasury":"hive:treasury","guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`, tokenID, c2ID)
	call(t, &ct, c5ID, "init", c5init, owner, 0, true)
	call(t, &ct, tokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, c2ID), owner, 0, true)

	call(t, &ct, c2ID, "distributeEpoch", ``, "hive:keeper", 1, true)
	call(t, &ct, c5ID, "pullFunding", `{"epoch":"0"}`, "hive:anyone", 1, true)
	call(t, &ct, c5ID, "submitShares", `{"epoch":"0","page":"0","entries":"hive:lp1:70,hive:lp2:30"}`, "hive:lpreporter", 1, true)
	call(t, &ct, c5ID, "finalizeEpoch", `{"epoch":"0"}`, "hive:lpreporter", 1, true)
	call(t, &ct, c5ID, "claim", `{"epoch":"0"}`, "hive:lp1", 2, true)
	call(t, &ct, c5ID, "claim", `{"epoch":"0"}`, "hive:lp2", 2, true)
	a := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:lp1"}`, "hive:x", 2, true)
	b := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:lp2"}`, "hive:x", 2, true)
	assert.Contains(t, a.Ret, `"70000"`)
	assert.Contains(t, b.Ret, `"30000"`)
}

// C6 migration: fund C6, airdrop a snapshot, verify balances + per-batch idempotency.
func TestC6MigrationSlice(t *testing.T) {
	const c6ID = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(tokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c6ID, owner, read("../c6-migration/artifacts/main.wasm"))

	call(t, &ct, tokenID, "init", `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	call(t, &ct, c6ID, "init", fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"1000000"}`, tokenID), owner, 0, true)
	// fund C6 with a bootstrap balance
	call(t, &ct, tokenID, "mint", `{"amount":"1000"}`, owner, 0, true)
	call(t, &ct, tokenID, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"1000"}`, c6ID), owner, 0, true)
	// airdrop the snapshot
	call(t, &ct, c6ID, "airdropBatch", `{"batchId":"batch1","entries":"hive:alice:600,hive:bob:400"}`, owner, 0, true)
	a := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:alice"}`, "hive:x", 0, true)
	b := call(t, &ct, tokenID, "balanceOf", `{"account":"hive:bob"}`, "hive:x", 0, true)
	assert.Contains(t, a.Ret, `"600"`)
	assert.Contains(t, b.Ret, `"400"`)
	// idempotency: re-running the same batch must fail
	call(t, &ct, c6ID, "airdropBatch", `{"batchId":"batch1","entries":"hive:alice:600,hive:bob:400"}`, owner, 0, false)
	// non-owner cannot airdrop
	call(t, &ct, c6ID, "airdropBatch", `{"batchId":"batch2","entries":"hive:eve:1"}`, "hive:eve", 0, false)
}
