package itest_test

import (
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
)

// Coverage tests for C2 (emission controller) — the economic core. The pre-existing
// suite only ever exercised ONE epoch through ONE 100%-weight bucket, so the
// flat emission, maxSupply exhaustion, keeper catch-up, multi-bucket splitting
// (incl. dust/remainder), claim authorization, the guardian passthrough round-trip
// and the init validation gauntlet were all untested. These fill that gap.
//
// Naming: every test is TestCovEmit_*, every helper/const is cv*-prefixed so this
// file never collides with slice_test.go / security_regression_test.go.

const (
	cvToken = "vsc1BYFMiw2RCvC7NnjNVm2naPKhJfkU7t55SN"
	cvC2    = "vsc1BeTJGp9QJLzFkp5jsa6LJnksrH2kZ1Y5NE"

	// spare C2 instances — the init-validation gauntlet needs a virgin contract
	// per rejected config so a rolled-back abort can never mask the next case.
	cvBad0  = "vsc1BoisPRSZjp92RbvNxnAdcSmBZHhyfzRDP2"
	cvBad1  = "vsc1BriTkBcRUmzVXkB7t9gqgneR8rA45yNn49"
	cvBad2  = "vsc1BVXLQTVFhvCUhP9cVTpd1seLTtJ32D2rFH"
	cvBad3  = "vsc1BqxdHrnt94QHYKVjrQiUFCnqvZVnXLbN8W"
	cvBad4  = "vsc1BfnJXgDtEvBqMoDkAPp61zKXUNJrAiuhZh"
	cvBad5  = "vsc1Bmvm1T6pbLZPRg35gvVDWj4CQmVKMCNVFJ"
	cvBad6  = "vsc1BZR12Cj3Ats3qVhPArm81V33bZoLX6Hvx2"
	cvBad7  = "vsc1BkNsjgWwLvZzReAEofrq5hrThoADRdbKzz"
	cvBad8  = "vsc1BiyuQUCMaTpMvpyngP9MbGVvQpqeDGMx5A"
	cvBad9  = "vsc1BXCNrcXSqKHUHZPC9jH3m8r6E9FucBBxEJ"
	cvBad10 = "vsc1BqLpwwWVAWCyNTobG6NyypC75Zm7aMAZLh"
	cvBad11 = "vsc1BerVs984Uxfd3XyFYbVzTJXyL3JPrtsQX5"

	cvC2Wasm = "../c2-emission/artifacts/main.wasm"

	// bucket targets are plain hive accounts so claimBucket can be driven directly
	cvCore = "hive:cvcore"
	cvOps  = "hive:cvops"
	cvDust = "hive:cvdust"
	cvSolo = "hive:cvsolo"

	// sentinel: an override with this value DROPS the key from the init payload
	cvOmit = "\x00omit"
)

// cvField pulls a flat field out of a contract's JSON return. Values may be
// quoted (big-int strings) or bare (numbers/bools), so handle both.
func cvField(ret, name string) string {
	needle := `"` + name + `"`
	i := strings.Index(ret, needle)
	if i < 0 {
		return ""
	}
	j := i + len(needle)
	for j < len(ret) && (ret[j] == ' ' || ret[j] == ':') {
		j++
	}
	if j < len(ret) && ret[j] == '"' {
		j++
		k := j
		for k < len(ret) && ret[k] != '"' {
			k++
		}
		return ret[j:k]
	}
	k := j
	for k < len(ret) && ret[k] != ',' && ret[k] != '}' {
		k++
	}
	return strings.TrimSpace(ret[j:k])
}

func cvBig(s string) *big.Int {
	n := new(big.Int)
	if s != "" {
		n.SetString(s, 10)
	}
	return n
}

// cvCfg builds a C2 init payload from a sane baseline plus per-test overrides.
// Baseline: genesis 0, 1-block epochs, 100000 emitted per epoch
// blocks, one 100% bucket paying cvSolo (which is also the dust bucket).
func cvCfg(over map[string]string) string {
	d := map[string]string{
		"token":             cvToken,
		"kind":              "0",
		"tokenId":           "",
		"genesis":           "0",
		"epochLen":          "1",
		"baseAnnual":        "1000000",
		"blocksPerYear":     "10",
		"dustBucket":        "solo",
		"timelock":          "1",
		"guardianMode":      "0",
		"guardianAuth":      "hive:cvguardian",
		"guardianThreshold": "1",
		"vetoMode":          "0",
		"vetoAuth":          "hive:cvveto",
		"vetoThreshold":     "1",
		"buckets":           "solo:" + cvSolo + ":10000",
	}
	for k, v := range over {
		if v == cvOmit {
			delete(d, k)
			continue
		}
		d[k] = v
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, `"`+k+`":"`+d[k]+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// cvBoot registers + inits the token and C2, and hands token ownership to C2 so
// it can mint. maxSupply is a knob (the exhaustion test needs a tiny one).
func cvBoot(t *testing.T, ct *test_utils.ContractTest, maxSupply string, over map[string]string) {
	ct.RegisterContract(cvToken, owner, read(tokenWasmPath))
	ct.RegisterContract(cvC2, owner, read(cvC2Wasm))
	call(t, ct, cvToken, "init",
		fmt.Sprintf(`{"name":"CV","symbol":"CV","decimals":0,"maxSupply":"%s"}`, maxSupply), owner, 0, true)
	// C2 no longer mints — it PULLS each epoch's emission from an account that has
	// approved it. So the pool must exist and be approved before any poke. Minting
	// the full maxSupply as the pool keeps the old semantics exactly: the pool IS
	// the supply cap, so "emission stops at maxSupply" still holds.
	call(t, ct, cvToken, "mint", fmt.Sprintf(`{"amount":"%s"}`, maxSupply), owner, 0, true)
	call(t, ct, cvToken, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"%s"}`, cvC2, maxSupply), owner, 0, true)
	call(t, ct, cvC2, "init", cvCfg(over), owner, 0, true)
	call(t, ct, cvToken, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, cvC2), owner, 0, true)
}

func cvEmission(t *testing.T, ct *test_utils.ContractTest, epoch string, h uint64) *big.Int {
	r := call(t, ct, cvC2, "emissionForEpochQ", `{"epoch":"`+epoch+`"}`, "hive:cvreader", h, true)
	return cvBig(cvField(r.Ret, "emission"))
}

func cvOwed(t *testing.T, ct *test_utils.ContractTest, target, epoch string, h uint64) *big.Int {
	r := call(t, ct, cvC2, "owedOf", `{"target":"`+target+`","epoch":"`+epoch+`"}`, "hive:cvreader", h, true)
	return cvBig(cvField(r.Ret, "owed"))
}

// cvSupply reports HOW MUCH HAS BEEN EMITTED so far.
//
// It used to read token.totalSupply, because C2 minted each epoch and supply grew
// with emission. C2 now PULLS from a pre-approved pool instead, so total supply is
// constant from the moment the pool is minted and no longer says anything about
// emission progress. What tracks emission is how much C2 has drawn out of the pool,
// i.e. C2's own balance (these tests never let buckets claim, so nothing leaves).
//
// This is a real consequence of the allowance model, not a test detail: an external
// observer can no longer read emission progress off totalSupply either.
func cvSupply(t *testing.T, ct *test_utils.ContractTest, h uint64) *big.Int {
	return cvBalance(t, ct, "contract:"+cvC2, h)
}

func cvBalance(t *testing.T, ct *test_utils.ContractTest, acct string, h uint64) *big.Int {
	r := call(t, ct, cvToken, "balanceOf", `{"account":"`+acct+`"}`, "hive:cvreader", h, true)
	return cvBig(cvField(r.Ret, "balance"))
}

func cvPoke(t *testing.T, ct *test_utils.ContractTest, h uint64) string {
	return call(t, ct, cvC2, "distributeEpoch", ``, "hive:cvkeeper", h, true).Ret
}

// cvDrain pokes repeatedly until the backlog is empty. distributeEpoch is bounded
// to maxCatch epochs per tx so one poke always fits the RC free tier (measured
// ~840 RC/epoch with 1 bucket, ~1280 with 3); a keeper therefore pokes until
// caught up. Returns the LAST response.
func cvDrain(t *testing.T, ct *test_utils.ContractTest, h uint64) string {
	last := ""
	for i := 0; i < 40; i++ {
		last = call(t, ct, cvC2, "distributeEpoch", ``, "hive:cvkeeper", h, true).Ret
		if strings.Contains(last, `"distributed":"0"`) ||
			strings.Contains(last, `"distributed":0`) ||
			strings.Contains(last, `"terminal":true`) {
			return last
		}
	}
	return last
}

// ---- emission schedule ---------------------------------------------------

// Emission is FLAT. The decaying/halving schedule (halvingPeriod, ratioNum,
// ratioDen, maxEras) was removed as out of scope, so every epoch — however far in
// the future — must emit exactly baseAnnual*epochLen/blocksPerYear.
func TestCovEmit_EmissionIsFlatForever(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{})

	base := cvEmission(t, &ct, "0", 1)
	assert.Equal(t, "100000", base.String(), "emission = baseAnnual*epochLen/blocksPerYear")
	for _, ep := range []string{"1", "2", "3", "10", "100", "1000", "1000000"} {
		assert.Equal(t, base.String(), cvEmission(t, &ct, ep, 1).String(),
			"emission must never decay (epoch "+ep+")")
	}
}

// With halving gone, maxSupply headroom is the ONLY thing that ends emission: the
// final epoch mints just the remainder and `terminal` latches so later pokes are
// permanent no-ops.
func TestCovEmit_SupplyHeadroomIsTheOnlyTerminator(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	// room for 2 full epochs (100000 each) plus a 30000 remainder
	cvBoot(t, &ct, "230000", map[string]string{})

	ret := cvPoke(t, &ct, 100)
	assert.Equal(t, "3", cvField(ret, "distributed"), "2 full epochs + the partial one")

	total := cvSupply(t, &ct, 100)
	assert.Equal(t, "230000", total.String(), "must mint exactly up to maxSupply, never past it")

	r2 := cvPoke(t, &ct, 500)
	assert.Contains(t, r2, `"terminal":true`, "terminal must latch once headroom is gone")
	assert.Equal(t, total.String(), cvSupply(t, &ct, 500).String(), "no mint after terminal")
}

// ---- epoch accounting ----------------------------------------------------

// Epoch 0 must be emitted (the absent-lastEpoch sentinel), and re-poking the same
// height must be a strict no-op — not a second mint of the same epoch.
func TestCovEmit_EpochZeroEmittedOnceOnly(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", nil)

	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"), "epoch 0 must be emitted")
	after := cvSupply(t, &ct, 1)
	assert.Equal(t, "100000", after.String())
	assert.Equal(t, "100000", cvOwed(t, &ct, cvSolo, "0", 1).String())

	// re-poke, same height, different caller — nothing more may be minted
	assert.Equal(t, "0", cvField(cvPoke(t, &ct, 1), "distributed"))
	call(t, &ct, cvC2, "distributeEpoch", ``, "hive:cvgriefer", 1, true)
	assert.Equal(t, after.String(), cvSupply(t, &ct, 1).String(), "re-poke must mint nothing")
	assert.Equal(t, "100000", cvOwed(t, &ct, cvSolo, "0", 1).String(), "owed must not double")
}

// Before the first epoch has fully elapsed there is nothing to distribute — and
// crucially nothing may be minted.
func TestCovEmit_BeforeFirstEpochDistributesZero(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{
		"epochLen": "10", "genesis": "0",
	})

	for _, h := range []uint64{1, 5, 9} {
		assert.Equal(t, "0", cvField(cvPoke(t, &ct, h), "distributed"),
			fmt.Sprintf("h=%d is inside epoch 0, nothing has elapsed", h))
		assert.Equal(t, "0", cvSupply(t, &ct, h).String(), "no premature mint")
	}
	// h=10: epoch 0 has now fully elapsed
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 10), "distributed"))
	assert.Equal(t, "1000000", cvSupply(t, &ct, 10).String(), "annual*epochLen/blocksPerYear")
}

// A keeper that goes down for a long time must catch up, minting EVERY elapsed
// epoch exactly once at the flat rate — and total supply must equal the sum of
// the per-epoch emissions with nothing lost or duplicated.
func TestCovEmit_CatchUpAfterDowntime(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{})

	// expected schedule, straight off the curve
	want := map[string]string{}
	sum := new(big.Int)
	for ep := 0; ep <= 10; ep++ {
		e := cvEmission(t, &ct, fmt.Sprint(ep), 1)
		want[fmt.Sprint(ep)] = e.String()
		if ep < 10 { // epochs 0..9 are the ones the catch-up poke covers
			sum.Add(sum, e)
		}
	}
	for _, ep := range []string{"0", "3", "4", "7", "8", "10"} {
		assert.Equal(t, "100000", want[ep], "flat emission at epoch "+ep)
	}
	assert.Equal(t, "1000000", sum.String(), "10 epochs x 100000")

	// keeper wakes up 10 epochs late and does it all in one poke
	cvDrain(t, &ct, 10) // backlog > maxCatch: keeper pokes until caught up

	got := new(big.Int)
	for ep := 0; ep < 10; ep++ {
		o := cvOwed(t, &ct, cvSolo, fmt.Sprint(ep), 10)
		assert.Equal(t, want[fmt.Sprint(ep)], o.String(),
			fmt.Sprintf("epoch %d must be credited at its own era rate", ep))
		got.Add(got, o)
	}
	supply := cvSupply(t, &ct, 10)
	assert.Equal(t, sum.String(), supply.String(), "totalSupply must equal the sum of emissions")
	assert.Equal(t, sum.String(), got.String(), "sum of owed must equal the sum of emissions")

	// catching up again changes nothing (each epoch minted exactly once)
	assert.Equal(t, "0", cvField(cvPoke(t, &ct, 10), "distributed"))
	assert.Equal(t, supply.String(), cvSupply(t, &ct, 10).String())
	// and the next epoch resumes cleanly from where it left off
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 11), "distributed"))
	assert.Equal(t, want["10"], cvOwed(t, &ct, cvSolo, "10", 11).String())
}

// ---- maxSupply exhaustion ------------------------------------------------

// The schedule must stop dead at the token's hard cap: the last funded epoch gets
// only the remaining headroom, `terminal` latches, later pokes are no-ops, and
// totalSupply NEVER exceeds maxSupply at any point.
func TestCovEmit_MaxSupplyExhaustsMidSchedule(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	const cvCap = "250000" // 2.5 epochs' worth at 100000/epoch
	cvBoot(t, &ct, cvCap, map[string]string{})

	cap_ := cvBig(cvCap)

	// step one epoch at a time so the cap is checked at every intermediate state
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"))
	assert.Equal(t, "100000", cvSupply(t, &ct, 1).String())
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 2), "distributed"))
	assert.Equal(t, "200000", cvSupply(t, &ct, 2).String())

	// epoch 2 wants 100000 but only 50000 of headroom is left
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 3), "distributed"))
	assert.Equal(t, "50000", cvOwed(t, &ct, cvSolo, "2", 3).String(),
		"final epoch must mint only the remaining headroom")
	s := cvSupply(t, &ct, 3)
	assert.Equal(t, cvCap, s.String(), "supply must land exactly on maxSupply")
	assert.False(t, s.Cmp(cap_) > 0, "totalSupply must never exceed maxSupply")

	// terminal latched; later pokes are no-ops and allocate nothing
	assert.Contains(t, cvPoke(t, &ct, 4), `"terminal":true`)
	assert.Contains(t, cvPoke(t, &ct, 50), `"terminal":true`)
	assert.Equal(t, cvCap, cvSupply(t, &ct, 50).String(), "no mint past the cap")
	assert.Equal(t, "0", cvOwed(t, &ct, cvSolo, "3", 50).String(), "no allocation past the cap")
	assert.Equal(t, "0", cvOwed(t, &ct, cvSolo, "4", 50).String())

	// conservation: everything minted is owed to somebody
	total := new(big.Int)
	for ep := 0; ep < 6; ep++ {
		total.Add(total, cvOwed(t, &ct, cvSolo, fmt.Sprint(ep), 50))
	}
	assert.Equal(t, cvCap, total.String(), "sum of owed == total minted")
}

// Same cap, but reached in a single catch-up poke — the headroom clamp must hold
// inside the loop too (it tracks supply locally rather than re-reading it).
func TestCovEmit_MaxSupplyClampInsideCatchUp(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	const cvCap = "250000"
	cvBoot(t, &ct, cvCap, map[string]string{})

	// one poke long after genesis: 10 epochs elapsed, only 2.5 fit under the cap
	assert.Equal(t, "3", cvField(cvPoke(t, &ct, 10), "distributed"),
		"catch-up must stop at the cap, not mint all 10 epochs")
	s := cvSupply(t, &ct, 10)
	assert.Equal(t, cvCap, s.String())
	assert.False(t, s.Cmp(cvBig(cvCap)) > 0, "totalSupply must never exceed maxSupply")

	total := new(big.Int)
	for ep := 0; ep < 10; ep++ {
		total.Add(total, cvOwed(t, &ct, cvSolo, fmt.Sprint(ep), 10))
	}
	assert.Equal(t, cvCap, total.String(), "sum of owed == total minted")
	assert.Equal(t, "100000", cvOwed(t, &ct, cvSolo, "0", 10).String())
	assert.Equal(t, "100000", cvOwed(t, &ct, cvSolo, "1", 10).String())
	assert.Equal(t, "50000", cvOwed(t, &ct, cvSolo, "2", 10).String())
	assert.Equal(t, "0", cvOwed(t, &ct, cvSolo, "3", 10).String())

	assert.Contains(t, cvPoke(t, &ct, 11), `"terminal":true`)
	assert.Equal(t, cvCap, cvSupply(t, &ct, 11).String())
}

// ---- multi-bucket splitting + dust ---------------------------------------

// cvThreeBuckets returns a buckets string for the three cv targets with the given
// weights, dust bucket last.
func cvThreeBuckets(wCore, wOps, wDust int) string {
	return fmt.Sprintf("core:%s:%d,ops:%s:%d,dust:%s:%d", cvCore, wCore, cvOps, wOps, cvDust, wDust)
}

// cvSumOwed totals the owed record of every bucket target over epochs [0,n).
func cvSumOwed(t *testing.T, ct *test_utils.ContractTest, n int, h uint64) *big.Int {
	sum := new(big.Int)
	for _, tgt := range []string{cvCore, cvOps, cvDust} {
		for ep := 0; ep < n; ep++ {
			sum.Add(sum, cvOwed(t, ct, tgt, fmt.Sprint(ep), h))
		}
	}
	return sum
}

// A 7000/1500/1500 split must credit each target its exact weight, and the three
// slices must add up to precisely what was minted.
func TestCovEmit_ThreeBucketWeights(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{
		"dustBucket": "dust", "buckets": cvThreeBuckets(7000, 1500, 1500),
	})

	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"))
	minted := cvSupply(t, &ct, 1)
	assert.Equal(t, "100000", minted.String())

	assert.Equal(t, "70000", cvOwed(t, &ct, cvCore, "0", 1).String(), "core = 70%")
	assert.Equal(t, "15000", cvOwed(t, &ct, cvOps, "0", 1).String(), "ops = 15%")
	assert.Equal(t, "15000", cvOwed(t, &ct, cvDust, "0", 1).String(), "dust = 15%")
	assert.Equal(t, minted.String(), cvSumOwed(t, &ct, 1, 1).String(),
		"sum of all bucket allocations must equal the minted amount")

	// hold across several epochs (incl. one that spans a halving at epoch 10)
	cvDrain(t, &ct, 12) // backlog > maxCatch
	minted = cvSupply(t, &ct, 12)
	assert.Equal(t, minted.String(), cvSumOwed(t, &ct, 12, 12).String(),
		"conservation must hold across every epoch and era")
}

// A weight split that cannot divide evenly must lose nothing: the truncation
// remainder has to land in the dust bucket.
func TestCovEmit_BucketRoundingRemainderToDust(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	// 100001 per epoch (blocksPerYear=1) is deliberately indivisible by 10000
	cvBoot(t, &ct, "1000000000", map[string]string{
		"baseAnnual": "100001", "blocksPerYear": "1",
		"dustBucket": "dust",
		"buckets":    cvThreeBuckets(3333, 3333, 3334),
	})

	assert.Equal(t, "100001", cvEmission(t, &ct, "0", 1).String())
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"))
	minted := cvSupply(t, &ct, 1)
	assert.Equal(t, "100001", minted.String())

	// floor(100001*3333/10000)=33330, floor(100001*3334/10000)=33340, remainder 1
	assert.Equal(t, "33330", cvOwed(t, &ct, cvCore, "0", 1).String())
	assert.Equal(t, "33330", cvOwed(t, &ct, cvOps, "0", 1).String())
	assert.Equal(t, "33341", cvOwed(t, &ct, cvDust, "0", 1).String(),
		"33340 weighted slice + 1 truncation remainder")
	assert.Equal(t, minted.String(), cvSumOwed(t, &ct, 1, 1).String(),
		"nothing may be lost to rounding")

	// the dust must keep accruing epoch after epoch, never leaking
	cvDrain(t, &ct, 10) // backlog > maxCatch
	minted = cvSupply(t, &ct, 10)
	assert.Equal(t, "1000010", minted.String(), "10 epochs * 100001")
	assert.Equal(t, minted.String(), cvSumOwed(t, &ct, 10, 10).String(),
		"exact conservation over 10 indivisible epochs")
}

// ---- claimBucket ---------------------------------------------------------

// Only the recorded target may pull its slice, exactly once, and only for an
// epoch that was actually funded.
func TestCovEmit_ClaimBucketAuthAndOnce(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{
		"dustBucket": "dust", "buckets": cvThreeBuckets(7000, 1500, 1500),
	})
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"))

	// a stranger is not a bucket target — nothing is owed to them
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, "hive:cvstranger", 1, false)
	// ...not even the contract owner or the keeper
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, owner, 1, false)
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, "hive:cvkeeper", 1, false)

	// a real target pulls its exact slice
	r := call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, cvCore, 1, true)
	assert.Equal(t, "70000", cvField(r.Ret, "claimed"))
	assert.Equal(t, "70000", cvBalance(t, &ct, cvCore, 1).String(), "tokens actually delivered")
	assert.Equal(t, "0", cvOwed(t, &ct, cvCore, "0", 1).String(), "owed zeroed on claim")

	// double pull must fail and must not move more tokens
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, cvCore, 1, false)
	assert.Equal(t, "70000", cvBalance(t, &ct, cvCore, 1).String())

	// an epoch that was never funded cannot be claimed
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"7"}`, cvCore, 1, false)
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"7"}`, cvDust, 1, false)

	// the other targets are unaffected by core's claim
	r = call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, cvOps, 1, true)
	assert.Equal(t, "15000", cvField(r.Ret, "claimed"))
	r = call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, cvDust, 1, true)
	assert.Equal(t, "15000", cvField(r.Ret, "claimed"))

	// conservation: everything minted for epoch 0 was pulled, nothing left over
	assert.Equal(t, "0", cvSumOwed(t, &ct, 1, 1).String())
	assert.Equal(t, "0", cvBalance(t, &ct, "contract:"+cvC2, 1).String(),
		"C2 must retain nothing once every bucket has pulled")
}

// ---- guardian passthrough (end to end) -----------------------------------

// The whole guardian round-trip against a REAL token: queue → too-early execute
// fails → post-timelock execute succeeds → the token is genuinely paused (a
// transfer reverts) → queue+execute unpause → transfers work again.
func TestCovEmit_GuardianPauseUnpauseEndToEnd(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{"timelock": "5"})

	// fund a real holder through the normal emission path
	assert.Equal(t, "1", cvField(cvPoke(t, &ct, 1), "distributed"))
	call(t, &ct, cvC2, "claimBucket", `{"epoch":"0"}`, cvSolo, 1, true)
	assert.Equal(t, "100000", cvBalance(t, &ct, cvSolo, 1).String())

	pause := `{"op":"pause","nonce":"1"}`
	// a non-guardian cannot queue
	call(t, &ct, cvC2, "queueTokenOp", pause, "hive:cvstranger", 10, false)
	call(t, &ct, cvC2, "queueTokenOp", pause, owner, 10, false)
	call(t, &ct, cvC2, "queueTokenOp", pause, "hive:cvveto", 10, false)
	// nothing was queued, so nothing can be executed
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvguardian", 10, false)

	// guardian queues it; timelock matures at 10+5=15
	call(t, &ct, cvC2, "queueTokenOp", pause, "hive:cvguardian", 10, true)
	// execution is not permissionless — only the guardian/veto may fire a matured op
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvstranger", 11, false)
	// ...and not before the timelock has elapsed
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvguardian", 12, false)
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvguardian", 14, false)
	// the token is still live while the timelock runs
	call(t, &ct, cvToken, "transfer", `{"to":"hive:cvsink","amount":"1"}`, cvSolo, 14, true)

	// matured — the op fires for real
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvguardian", 15, true)
	p := call(t, &ct, cvToken, "isPaused", `{}`, "hive:cvreader", 15, true)
	assert.Contains(t, p.Ret, `"paused":true`, "token must report paused")
	// and the pause is REAL: transfers revert
	call(t, &ct, cvToken, "transfer", `{"to":"hive:cvsink","amount":"100"}`, cvSolo, 16, false)
	assert.Equal(t, "1", cvBalance(t, &ct, "hive:cvsink", 16).String(), "no transfer while paused")
	// a spent op cannot be replayed
	call(t, &ct, cvC2, "executeTokenOp", pause, "hive:cvguardian", 16, false)

	// unpause goes through the same gauntlet (distinct nonce)
	unpause := `{"op":"unpause","nonce":"2"}`
	call(t, &ct, cvC2, "executeTokenOp", unpause, "hive:cvguardian", 16, false)
	call(t, &ct, cvC2, "queueTokenOp", unpause, "hive:cvguardian", 16, true)
	call(t, &ct, cvC2, "executeTokenOp", unpause, "hive:cvguardian", 20, false)
	call(t, &ct, cvC2, "executeTokenOp", unpause, "hive:cvguardian", 21, true)
	p = call(t, &ct, cvToken, "isPaused", `{}`, "hive:cvreader", 21, true)
	assert.Contains(t, p.Ret, `"paused":false`, "token must report unpaused")

	call(t, &ct, cvToken, "transfer", `{"to":"hive:cvsink","amount":"100"}`, cvSolo, 22, true)
	assert.Equal(t, "101", cvBalance(t, &ct, "hive:cvsink", 22).String(), "transfers work again")
}

// The VETO authority — not the guardian — owns cancellation, and a cancelled op
// is unexecutable.
func TestCovEmit_VetoCancelsQueuedOp(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	cvBoot(t, &ct, "1000000000", map[string]string{"timelock": "5"})

	op := `{"op":"changeOwner","nonce":"1","newOwner":"hive:cvevil"}`
	call(t, &ct, cvC2, "queueTokenOp", op, "hive:cvguardian", 10, true)
	// the guardian cannot veto its own op, nor can a bystander
	call(t, &ct, cvC2, "cancelTokenOp", op, "hive:cvguardian", 11, false)
	call(t, &ct, cvC2, "cancelTokenOp", op, "hive:cvstranger", 11, false)
	// the veto authority can
	call(t, &ct, cvC2, "cancelTokenOp", op, "hive:cvveto", 11, true)
	// and the cancelled op is gone for good — even for the guardian itself
	call(t, &ct, cvC2, "executeTokenOp", op, "hive:cvguardian", 15, false)
	call(t, &ct, cvC2, "executeTokenOp", op, "hive:cvveto", 100, false)
	call(t, &ct, cvC2, "cancelTokenOp", op, "hive:cvveto", 100, false)

	// the token owner is untouched — C2 still owns it and can still emit
	o := call(t, &ct, cvToken, "getOwner", `{}`, "hive:cvreader", 100, true)
	assert.Contains(t, o.Ret, "contract:"+cvC2, "a vetoed changeOwner must not have landed")
	assert.Equal(t, "50", cvField(cvPoke(t, &ct, 100), "distributed"))
}

// ---- init validation gauntlet --------------------------------------------

// Every economic parameter is immutable after init, so a bad config must be
// rejected at init time — there is no second chance. Each case gets its OWN
// freshly-registered C2 so a rejected init can never influence the next case.
func TestCovEmit_InitValidationRejectsBadConfig(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })

	ct.RegisterContract(cvToken, owner, read(tokenWasmPath))
	bad := []string{cvBad0, cvBad1, cvBad2, cvBad3, cvBad4, cvBad5,
		cvBad6, cvBad7, cvBad8, cvBad9, cvBad10, cvBad11}
	for _, id := range bad {
		ct.RegisterContract(id, owner, read(cvC2Wasm))
	}
	call(t, &ct, cvToken, "init",
		`{"name":"CV","symbol":"CV","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)

	cases := []struct {
		id   string
		why  string
		over map[string]string
	}{
		{cvBad0, "epochLen=0", map[string]string{"epochLen": "0"}},
		{cvBad2, "bucket weights sum < 10000",
			map[string]string{"buckets": "solo:" + cvSolo + ":9000"}},
		{cvBad3, "dustBucket names no configured bucket",
			map[string]string{"dustBucket": "nosuchbucket"}},
		{cvBad4, "timelock=0 (guardian ops would be instant)",
			map[string]string{"timelock": "0"}},
		{cvBad7, "bucket target is C2 itself (R21)",
			map[string]string{"buckets": "solo:contract:" + cvBad7 + ":10000"}},
		{cvBad8, "guardian and veto authorities overlap (CRIT-1)",
			map[string]string{"guardianAuth": "hive:cvsame", "vetoAuth": "hive:cvsame"}},
		{cvBad9, "non-numeric genesis (HIGH-4)", map[string]string{"genesis": "notanumber"}},
		// NOTE: "missing genesis" is now VALID — it defaults to the deployment block,
		// and a genesis in the PAST is rejected (that foot-gun is covered separately;
		// it can't be provoked here because this gauntlet inits at height 0).
		{cvBad10, "maxCatch out of range", map[string]string{"maxCatch": "5000"}},
	}
	for _, c := range cases {
		t.Log("init must be rejected: " + c.why)
		call(t, &ct, c.id, "init", cvCfg(c.over), owner, 0, false)
	}
	// over-sum weights are rejected too (same contract: both attempts abort, so
	// neither can have left `init` behind)
	call(t, &ct, cvBad2, "init",
		cvCfg(map[string]string{"buckets": "core:" + cvCore + ":6000,ops:" + cvOps + ":5000",
			"dustBucket": "core"}), owner, 0, false)

	// positive control: the unmutated baseline IS valid, so every rejection above
	// is attributable to its specific mutation and not to a broken template.
	call(t, &ct, cvBad11, "init", cvCfg(nil), owner, 0, true)
	// ...and it cannot be re-initialized
	call(t, &ct, cvBad11, "init", cvCfg(nil), owner, 0, false)
	// ...nor initialized by a non-owner (checked on a still-virgin contract)
	call(t, &ct, cvBad0, "init", cvCfg(nil), "hive:cvstranger", 0, false)
}
