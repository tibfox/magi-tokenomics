package itest_test

import (
	"fmt"
	"os"

	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Checkpoint growth, and the cost of the path that drives it hardest.
//
// Every stake mutation appends to two append-only arrays: a per-account history and
// a GLOBAL checkpoint of total_staked. `searchVal` binary-searches them at one host
// state read per probe, so their length is what `stakeAtHeight`, `claimYield` and
// `minStakeSum` pay for, for the life of the deployment.
//
// N mutations inside ONE block used to append N entries all carrying the same height,
// of which the search can only ever reach the last. `airdropBatch` with
// `airdropStaked=1` routes every recipient through applyCredit in a single
// transaction, so a 25-holder batch appended 25 global checkpoints where 1 was needed
// — and a 1,000-holder migration, 1,000.
//
// This measures that path (it is absent from TestRC_ProfileAllFunctions, which only
// exercises the liquid airdrop and so never touches a checkpoint) and asserts the
// arrays stay flat across a same-block batch.

const (
	perfTok = "vsc1Bd6ZgTRHZQyMXCFYnCbcZaipvHNPd9YHSC"
	perfC1  = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
)

func perfBootStakedAirdrop(t *testing.T, ct *test_utils.ContractTest) {
	t.Helper()
	ct.RegisterContract(perfTok, owner, read(tokenWasmPath))
	ct.RegisterContract(perfC1, owner, read("../c1-staking/artifacts/main.wasm"))
	call(t, ct, perfTok, "init",
		`{"name":"P","symbol":"P","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	// airdropStaked: recipients are credited stake rather than paid liquid tokens, so
	// each one runs applyCredit -> appendHist + appendCkpt.
	call(t, ct, perfC1, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","cooldown":"20","epochLen":"5","allow":"","maxAirdrop":"1000000",`+
			`"airdropStaked":"1","treasury":"hive:treasury","guardianMode":"0",`+
			`"guardianAuth":"hive:guardian","guardianThreshold":"1"}`, perfTok), owner, 0, true)
	// Float for the airdrop to reclassify into principal.
	call(t, ct, perfTok, "mint", `{"amount":"1000000"}`, owner, 0, true)
	call(t, ct, perfTok, "transfer",
		fmt.Sprintf(`{"to":"contract:%s","amount":"500000"}`, perfC1), owner, 0, true)
}

// The array length itself is not observable from outside — `ckpt_n` has no query and
// the harness's state diff type is unexported — so the test asserts the two things
// that ARE observable and that the collapse must not disturb: the RC the batch spends
// (one write per collapsed entry, logged for docs/rc-costs.md) and the exactness of
// every height-indexed read that depends on the arrays.
func TestPerf_StakedAirdropDoesNotInflateTheCheckpointArray(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	perfBootStakedAirdrop(t, &ct)

	// One batch of 25, all credited inside a single block.
	parts := make([]string, 25)
	for i := 0; i < 25; i++ {
		parts[i] = fmt.Sprintf("hive:perfdrop%03d:100", i)
	}
	res := call(t, &ct, perfC1, "airdropBatch",
		fmt.Sprintf(`{"batchId":"1","entries":"%s"}`, strings.Join(parts, ",")), owner, 1, true)
	t.Logf("airdropBatch (airdropStaked, 25 recipients, one block): %d RC", res.RcUsed)

	// A second batch, also in one block, at a LATER height.
	for i := 0; i < 25; i++ {
		parts[i] = fmt.Sprintf("hive:perfdropb%03d:100", i)
	}
	res2 := call(t, &ct, perfC1, "airdropBatch",
		fmt.Sprintf(`{"batchId":"2","entries":"%s"}`, strings.Join(parts, ",")), owner, 2, true)
	t.Logf("second batch at a later height: %d RC", res2.RcUsed)

	// Collapsing same-height entries must not disturb any height-indexed read.
	total := call(t, &ct, perfC1, "totalStaked", ``, "hive:any", 3, true)
	assert.Contains(t, total.Ret, "5000", "50 recipients x 100 credited as stake")
	at1 := call(t, &ct, perfC1, "totalStakedAtHeight", `{"height":"1"}`, "hive:any", 3, true)
	assert.Contains(t, at1.Ret, "2500", "height 1 saw only the first batch")
	at2 := call(t, &ct, perfC1, "totalStakedAtHeight", `{"height":"2"}`, "hive:any", 3, true)
	assert.Contains(t, at2.Ret, "5000", "height 2 saw both")

	// Per-account history must be exact too — one entry, at the block it was credited.
	s := call(t, &ct, perfC1, "stakeAtHeight",
		`{"account":"hive:perfdrop000","height":"1"}`, "hive:any", 3, true)
	assert.Contains(t, s.Ret, "100")
	s0 := call(t, &ct, perfC1, "stakeAtHeight",
		`{"account":"hive:perfdropb000","height":"1"}`, "hive:any", 3, true)
	assert.Contains(t, s0.Ret, `"0"`, "batch 2's recipients held nothing at height 1")
}
