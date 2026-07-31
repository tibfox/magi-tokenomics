package itest_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strconv"
)

// Coverage tests for the previously untested surface of the C3 author/curation
// distributor, its C5 LP clone and the C6 migration contract:
//   - multi-PAGE submitShares accumulation + exactly-once page application
//   - malformed share entries (empty acct, zero/negative, no colon)
//   - claim rounding / dust and the Σ(payouts) ≤ funded conservation invariant
//   - the guardian veto path (cancelEpoch → unallocated → pinned-treasury sweep)
//   - C3 init validation (window, treasury, reporter∩guardian, funder schedule)
//   - C6 batch accumulation, per-batch idempotency and the maxAirdrop cap
//
// All ids/helpers here are prefixed `cov` so they never collide with the other
// files in this package.

const (
	covTokenID = "vsc1BftPdnUXT4LJtAK86rcDVJhtedgULZPFPP"
	covC2ID    = "vsc1BdxvBvpwKko8XfgN35iWwzHWq5tM9huT1W"
	covC3ID    = "vsc1BdxvBvpwKko8XfmENyGBA54hGdZzxfLcQ1"
	covC5ID    = "vsc1BdxvBvpwKko8Xfvy4kMVaEd49iwJXdzEdV"
	covC6ID    = "vsc1BdxvBvpwKko8Xg1qQdu9nKQEbGcxLPhBuc"
	covC3bID   = "vsc1Bec5LYi8har9F16zdEetirFVErkyKuM6wQ"
	covC6bID   = "vsc1Bec5LYi8har9F1MbeuHsM6b2ZVouhmCa8j"

	covReporter = "hive:covreporter"
	covGuardian = "hive:covguardian"
	covTreasury = "hive:covtreasury"

	covC3Wasm = "../c3-distributor/artifacts/main.wasm"
	covC5Wasm = "../c5-lp/artifacts/main.wasm"
	covC6Wasm = "../c6-migration/artifacts/main.wasm"
	covC2Wasm = "../c2-emission/artifacts/main.wasm"
)

// Per-epoch emission with this C2 config is baseAnnual*epochLen/blocksPerYear =
// 1000000*1/10 = 100000, 100% of which is routed to the distributor under test.
const covEpochFunding = 100000

func covTokenInit() string {
	return `{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`
}

func covC2Init(target string) string {
	return fmt.Sprintf(`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000","blocksPerYear":"10","dustBucket":"share","timelock":"1","guardianMode":"0","guardianAuth":"hive:covc2guardian","guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:covc2veto","vetoThreshold":"1","buckets":"share:contract:%s:10000"}`, covTokenID, target)
}

// covDistInit builds a valid C3/C5 init payload; individual fields are overridden
// by the init-validation test via covDistInitRaw.
func covDistInit(window, reporter string) string {
	return covDistInitRaw(covC2ID, window, reporter, covGuardian, covTreasury)
}

func covDistInitRaw(funder, window, reporterAuth, guardianAuth, treasury string) string {
	win := ""
	if window != "" {
		win = fmt.Sprintf(`"window":"%s",`, window)
	}
	tre := ""
	if treasury != "" {
		tre = fmt.Sprintf(`"treasury":"%s",`, treasury)
	}
	return fmt.Sprintf(`{"token":"%s","kind":"0","funder":"%s",%s"reporterMode":"0","reporterAuth":"%s","reporterThreshold":"1",%s"guardianMode":"0","guardianAuth":"%s","guardianThreshold":"1"}`,
		covTokenID, funder, win, reporterAuth, tre, guardianAuth)
}

// covBoot registers + initializes token, C2 and the distributor under test, and
// hands token ownership to C2 so it can mint. C2 MUST be initialized before the
// distributor — the distributor pulls scheduleInfo from its funder at init.
func covBoot(t *testing.T, distWasm, distID, window, reporter string) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(covC2ID, owner, read(covC2Wasm))
	ct.RegisterContract(distID, owner, read(distWasm))

	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)
	// C2 no longer mints — it PULLS each epoch's emission from an account that has
	// approved it. So the pool must exist and be approved before any poke. Minting
	// the full maxSupply as the pool keeps the old semantics exactly: the pool IS
	// the supply cap, so "emission stops at maxSupply" still holds.
	call(t, &ct, covTokenID, "mint", `{"amount":"1000000000"}`, owner, 0, true)
	call(t, &ct, covTokenID, "approve",
		fmt.Sprintf(`{"spender":"contract:%s","amount":"1000000000"}`, covC2ID), owner, 0, true)
	call(t, &ct, covC2ID, "init", covC2Init(distID), owner, 0, true)
	call(t, &ct, distID, "init", covDistInit(window, reporter), owner, 0, true)
	call(t, &ct, covTokenID, "changeOwner", fmt.Sprintf(`{"newOwner":"contract:%s"}`, covC2ID), owner, 0, true)
	return &ct
}

// covFundEpoch pokes C2 at `height` and pulls the given epoch's slice into the
// distributor, asserting the on-chain funding record.
func covFundEpoch(t *testing.T, ct *test_utils.ContractTest, distID, epoch string, height uint64) {
	t.Helper()
	call(t, ct, covC2ID, "distributeEpoch", ``, "hive:covkeeper", height, true)
	r := call(t, ct, distID, "pullFunding", fmt.Sprintf(`{"epoch":"%s"}`, epoch), "hive:covanyone", height, true)
	assert.Contains(t, r.Ret, fmt.Sprintf(`"funded":"%d"`, covEpochFunding))
}

// covJSONField pulls a string field out of a flat JSON contract return.
func covJSONField(t *testing.T, ret, name string) string {
	t.Helper()
	m := map[string]json.RawMessage{}
	require.NoError(t, json.Unmarshal([]byte(ret), &m), "unmarshal %q", ret)
	raw, ok := m[name]
	require.True(t, ok, "field %q missing from %q", name, ret)
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw)
	}
	return s
}

func covBig(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	require.True(t, ok, "not a decimal integer: %q", s)
	return n
}

// covBalance reads a token balance as a big.Int (substring asserts on Ret are
// ambiguous — "1" also matches "1000").
func covBalance(t *testing.T, ct *test_utils.ContractTest, account string, height uint64) *big.Int {
	t.Helper()
	r := call(t, ct, covTokenID, "balanceOf", fmt.Sprintf(`{"account":"%s"}`, account), "hive:covquery", height, true)
	return covBig(t, covJSONField(t, r.Ret, "balance"))
}

// covShareOf returns (share, totalShares, funded, status) for an account.
func covShareOf(t *testing.T, ct *test_utils.ContractTest, distID, epoch, account string, height uint64) (string, string, string, string) {
	t.Helper()
	r := call(t, ct, distID, "shareOf", fmt.Sprintf(`{"epoch":"%s","account":"%s"}`, epoch, account), "hive:covquery", height, true)
	return covJSONField(t, r.Ret, "share"),
		covJSONField(t, r.Ret, "totalShares"),
		covJSONField(t, r.Ret, "funded"),
		covJSONField(t, r.Ret, "status")
}

func covSubmit(t *testing.T, ct *test_utils.ContractTest, distID, reporter, epoch, page, entries string, height uint64, expectOK bool) {
	t.Helper()
	call(t, ct, distID, "submitShares",
		fmt.Sprintf(`{"epoch":"%s","page":"%s","entries":"%s"}`, epoch, page, entries),
		reporter, height, expectOK)
}

// ---------------------------------------------------------------------------
// C3 — multi-page share submission
// ---------------------------------------------------------------------------

// Pages 0/1/2 must accumulate into ONE totalShares, each page must apply exactly
// once, and no page may be added after finalize.
func TestCovDist_MultiPageSharesAccumulate(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "1", covReporter)
	covFundEpoch(t, ct, covC3ID, "0", 1)

	covSubmit(t, ct, covC3ID, covReporter, "0", "0", "hive:cova:60", 1, true)
	_, ts, _, _ := covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "60", ts, "after page 0")

	covSubmit(t, ct, covC3ID, covReporter, "0", "1", "hive:covb:30", 1, true)
	_, ts, _, _ = covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "90", ts, "after page 1")

	covSubmit(t, ct, covC3ID, covReporter, "0", "2", "hive:covc:10", 1, true)
	sh, ts, funded, status := covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "60", sh)
	assert.Equal(t, "100", ts, "3 pages must accumulate")
	assert.Equal(t, "100000", funded, "shareOf must report the on-chain funding")
	assert.Equal(t, "", status, "epoch still open")

	// per-page exactly-once: identical AND different entries are both refused
	covSubmit(t, ct, covC3ID, covReporter, "0", "1", "hive:covb:30", 1, false)
	covSubmit(t, ct, covC3ID, covReporter, "0", "1", "hive:covb:999", 1, false)
	// non-canonical page ids must not open a second slot for page 1
	covSubmit(t, ct, covC3ID, covReporter, "0", "01", "hive:covb:999", 1, false)
	_, ts, _, _ = covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "100", ts, "rejected re-submissions must not mutate totalShares")

	// a non-reporter cannot push a page at all
	covSubmit(t, ct, covC3ID, "hive:coveve", "0", "3", "hive:coveve:1000", 1, false)

	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 1, true)

	// ...and no further page may be added once the epoch is frozen
	covSubmit(t, ct, covC3ID, covReporter, "0", "3", "hive:covd:1000", 1, false)
	sh, ts, funded, status = covShareOf(t, ct, covC3ID, "0", "hive:covc", 1)
	assert.Equal(t, "10", sh)
	assert.Equal(t, "100", ts, "totalShares frozen at finalize")
	assert.Equal(t, "100000", funded)
	assert.Equal(t, "finalized", status)
}

// Malformed entries inside a page must be skipped without corrupting the running
// totalShares: empty account, zero, negative, missing colon, empty amount.
func TestCovDist_MalformedEntriesSkipped(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "1", covReporter)
	covFundEpoch(t, ct, covC3ID, "0", 1)

	// good: cova=10, covd=7 (17). skipped: empty acct, zero, negative, no colon,
	// empty amount, and an empty comma element.
	entries := "hive:cova:10,:5,hive:covb:0,hive:covc:-5,nocolonentry,hive:covd:7,hive:cove:"
	covSubmit(t, ct, covC3ID, covReporter, "0", "0", entries, 1, true)

	sh, ts, _, _ := covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "10", sh)
	assert.Equal(t, "17", ts, "only well-formed positive entries may count")
	for _, bad := range []string{"hive:covb", "hive:covc", "hive:cove", "nocolonentry", ""} {
		s, _, _, _ := covShareOf(t, ct, covC3ID, "0", bad, 1)
		assert.Equal(t, "0", s, "malformed entry must not create a share for %q", bad)
	}
	s, _, _, _ := covShareOf(t, ct, covC3ID, "0", "hive:covd", 1)
	assert.Equal(t, "7", s)

	// a page consisting solely of junk applies (consuming its page id) but adds nothing
	covSubmit(t, ct, covC3ID, covReporter, "0", "1", ":1,badentry,hive:covz:0,hive:covy:-1", 1, true)
	_, ts, _, _ = covShareOf(t, ct, covC3ID, "0", "hive:cova", 1)
	assert.Equal(t, "17", ts, "junk-only page must not change totalShares")

	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 1, true)

	// the surviving shares still pay out proportionally (10/17, 7/17 of 100000)
	ra := call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:cova", 2, true)
	rd := call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covd", 2, true)
	assert.Equal(t, "58823", covJSONField(t, ra.Ret, "claimed"))
	assert.Equal(t, "41176", covJSONField(t, rd.Ret, "claimed"))
	// accounts that only appeared in malformed entries cannot claim
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covb", 2, false)
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:cove", 2, false)
}

// ---------------------------------------------------------------------------
// C3 — claims, rounding/dust and conservation
// ---------------------------------------------------------------------------

// 3 claimants with equal shares over a funding amount that is NOT divisible by 3:
// every claim must succeed, each payout must be floor(funded*share/total), and
// Σ(payouts) must be <= funded (dust stays behind — nothing is over-paid).
func TestCovDist_RoundingDustAndConservation(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "5", covReporter)
	covFundEpoch(t, ct, covC3ID, "0", 1)

	covSubmit(t, ct, covC3ID, covReporter, "0", "0", "hive:covr1:1,hive:covr2:1,hive:covr3:1", 1, true)
	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 1, true)

	// finalize at h=1 with window=5 → claims open at h=6
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covr1", 2, false)
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covr1", 5, false)

	funded := big.NewInt(covEpochFunding)
	total := big.NewInt(3)
	sum := new(big.Int)
	for _, acct := range []string{"hive:covr1", "hive:covr2", "hive:covr3"} {
		r := call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, acct, 6, true)
		paid := covBig(t, covJSONField(t, r.Ret, "claimed"))
		want := new(big.Int).Div(new(big.Int).Mul(funded, big.NewInt(1)), total) // floor
		assert.Equal(t, want.String(), paid.String(), "payout for %s must be floor(funded*share/total)", acct)
		assert.Equal(t, want.String(), covBalance(t, ct, acct, 6).String(), "token balance for %s", acct)
		sum.Add(sum, paid)
	}
	assert.Equal(t, "99999", sum.String())
	assert.True(t, sum.Cmp(funded) <= 0, "conservation: Σ(payouts)=%s must be <= funded=%s", sum, funded)

	// the rounding dust must still be sitting in the distributor, unpaid
	dust := new(big.Int).Sub(funded, sum)
	assert.Equal(t, "1", dust.String())
	assert.Equal(t, dust.String(), covBalance(t, ct, "contract:"+covC3ID, 6).String(),
		"undistributed dust must remain in the distributor")

	// double claim and no-share claim must both be refused
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covr1", 6, false)
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covnobody", 6, false)
	assert.Equal(t, "0", covBalance(t, ct, "hive:covnobody", 6).String())
	// ...and the paid accounts' balances did not move on the failed second claim
	assert.Equal(t, "33333", covBalance(t, ct, "hive:covr1", 6).String())
}

// ---------------------------------------------------------------------------
// C3 — guardian veto path
// ---------------------------------------------------------------------------

// cancelEpoch inside the challenge window rolls funded→unallocated and blocks all
// later claims; sweepUnallocated then moves EXACTLY that amount to the PINNED
// treasury (it has no `to` parameter, so it cannot be redirected), and a second
// sweep fails. Neither action is available to a non-guardian.
func TestCovDist_VetoCancelAndPinnedSweep(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "10", covReporter)
	covFundEpoch(t, ct, covC3ID, "0", 1)
	covSubmit(t, ct, covC3ID, covReporter, "0", "0", "hive:cova:60,hive:covb:40", 1, true)
	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 1, true)

	// only the guardian may veto (reporter/random/owner may not)
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covReporter, 2, false)
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, "hive:coveve", 2, false)
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, owner, 2, false)

	// guardian veto inside the window (finalized at h=1, window 10 → open until h=11)
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covGuardian, 2, true)
	sh, ts, funded, status := covShareOf(t, ct, covC3ID, "0", "hive:cova", 2)
	assert.Equal(t, "60", sh, "shares are retained, the funding is what moves")
	assert.Equal(t, "100", ts)
	assert.Equal(t, "0", funded, "funded must be zeroed into `unallocated`")
	assert.Equal(t, "cancelled", status)

	// every claim is dead, before and after the (now irrelevant) window
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:cova", 3, false)
	call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, "hive:covb", 20, false)
	assert.Equal(t, "0", covBalance(t, ct, "hive:cova", 20).String())

	// a non-guardian cannot sweep
	call(t, ct, covC3ID, "sweepUnallocated", `{"nonce":"1"}`, "hive:coveve", 20, false)
	call(t, ct, covC3ID, "sweepUnallocated", `{"nonce":"1"}`, covReporter, 20, false)

	// the guardian sweeps — and a `to` field in the payload is IGNORED: the only
	// destination is the treasury pinned at init (H2).
	s := call(t, ct, covC3ID, "sweepUnallocated", `{"nonce":"1","to":"hive:coveve"}`, covGuardian, 20, true)
	assert.Equal(t, "100000", covJSONField(t, s.Ret, "swept"))
	assert.Equal(t, "100000", covBalance(t, ct, covTreasury, 20).String(),
		"cancelled funding must land in the PINNED treasury")
	assert.Equal(t, "0", covBalance(t, ct, "hive:coveve", 20).String(),
		"sweepUnallocated must not be redirectable via a `to` payload field")
	assert.Equal(t, "0", covBalance(t, ct, "contract:"+covC3ID, 20).String(),
		"the whole cancelled epoch must have left the distributor")

	// a second sweep (fresh nonce, so auth is not the blocker) has nothing to move
	call(t, ct, covC3ID, "sweepUnallocated", `{"nonce":"2"}`, covGuardian, 20, false)
	assert.Equal(t, "100000", covBalance(t, ct, covTreasury, 20).String())

	// ---- and the veto EXPIRES: a second epoch, cancelled after its window ----
	covFundEpoch(t, ct, covC3ID, "1", 25)
	covSubmit(t, ct, covC3ID, covReporter, "1", "0", "hive:cova:1", 25, true)
	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"1"}`, covReporter, 25, true)
	// finalized at h=25, window 10 → challenge window elapsed at h=35
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"1"}`, covGuardian, 40, false)
	call(t, ct, covC3ID, "claim", `{"epoch":"1"}`, "hive:cova", 40, true)
	assert.Equal(t, "100000", covBalance(t, ct, "hive:cova", 40).String(),
		"an epoch past its challenge window must be payable, not vetoable")
}

// ---------------------------------------------------------------------------
// C3 — init validation
// ---------------------------------------------------------------------------

// Each misconfiguration must abort init rather than deploy a subtly broken
// distributor. The final (fully valid) init proves the harness itself is sane.
func TestCovDist_InitValidationRejectsBadConfig(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(covC2ID, owner, read(covC2Wasm))
	ct.RegisterContract(covC3bID, owner, read(covC3Wasm))
	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)

	good := covDistInitRaw(covC2ID, "1", covReporter, covGuardian, covTreasury)

	// (1) funder scheduleInfo unavailable — C2 has not been initialized yet
	call(t, &ct, covC3bID, "init", good, owner, 0, false)

	call(t, &ct, covC2ID, "init", covC2Init(covC3bID), owner, 0, true)

	// (2) window missing entirely
	call(t, &ct, covC3bID, "init", covDistInitRaw(covC2ID, "", covReporter, covGuardian, covTreasury), owner, 0, false)
	// (3) window = 0 (would silently disable the guardian veto)
	call(t, &ct, covC3bID, "init", covDistInitRaw(covC2ID, "0", covReporter, covGuardian, covTreasury), owner, 0, false)
	// (4) treasury missing — sweepUnallocated would have no pinned destination
	call(t, &ct, covC3bID, "init", covDistInitRaw(covC2ID, "1", covReporter, covGuardian, ""), owner, 0, false)
	// (5) reporter ∩ guardian overlap — one coalition could both forge and refuse to veto
	call(t, &ct, covC3bID, "init", covDistInitRaw(covC2ID, "1", "hive:covsame", "hive:covsame", covTreasury), owner, 0, false)
	// (6) non-owner cannot init
	call(t, &ct, covC3bID, "init", good, "hive:coveve", 0, false)

	// none of the rejected attempts may have left the contract initialized...
	call(t, &ct, covC3bID, "shareOf", `{"epoch":"0","account":"hive:cova"}`, "hive:covquery", 0, false)
	// ...and the fully valid config still initializes
	call(t, &ct, covC3bID, "init", good, owner, 0, true)
	call(t, &ct, covC3bID, "init", good, owner, 0, false) // no re-init
}

// ---------------------------------------------------------------------------
// C5 — structural clone parity (same pages/rounding/veto semantics as C3)
// ---------------------------------------------------------------------------

func TestCovDist_C5PagesRoundingAndVetoParity(t *testing.T) {
	os.RemoveAll("data/badger")
	const lpReporter = "hive:covlpreporter"
	ct := covBoot(t, covC5Wasm, covC5ID, "5", lpReporter)
	covFundEpoch(t, ct, covC5ID, "0", 1)

	// three pages, one LP each, equal shares → same 1/3 rounding case as C3
	covSubmit(t, ct, covC5ID, lpReporter, "0", "0", "hive:covlp1:1", 1, true)
	covSubmit(t, ct, covC5ID, lpReporter, "0", "1", "hive:covlp2:1", 1, true)
	covSubmit(t, ct, covC5ID, lpReporter, "0", "2", "hive:covlp3:1", 1, true)
	covSubmit(t, ct, covC5ID, lpReporter, "0", "1", "hive:covlp2:1", 1, false) // page replay
	_, ts, _, _ := covShareOf(t, ct, covC5ID, "0", "hive:covlp1", 1)
	assert.Equal(t, "3", ts)

	call(t, ct, covC5ID, "finalizeEpoch", `{"epoch":"0"}`, lpReporter, 1, true)
	call(t, ct, covC5ID, "claim", `{"epoch":"0"}`, "hive:covlp1", 3, false) // window not elapsed

	sum := new(big.Int)
	for _, acct := range []string{"hive:covlp1", "hive:covlp2", "hive:covlp3"} {
		r := call(t, ct, covC5ID, "claim", `{"epoch":"0"}`, acct, 6, true)
		assert.Equal(t, "33333", covJSONField(t, r.Ret, "claimed"))
		sum.Add(sum, covBig(t, covJSONField(t, r.Ret, "claimed")))
	}
	assert.True(t, sum.Cmp(big.NewInt(covEpochFunding)) <= 0, "Σ(payouts) must be <= funded")
	call(t, ct, covC5ID, "claim", `{"epoch":"0"}`, "hive:covlp1", 6, false)    // double claim
	call(t, ct, covC5ID, "claim", `{"epoch":"0"}`, "hive:covnobody", 6, false) // no share

	// veto path on a second epoch: cancel → sweep to the pinned treasury
	covFundEpoch(t, ct, covC5ID, "1", 10)
	covSubmit(t, ct, covC5ID, lpReporter, "1", "0", "hive:covlp1:1", 10, true)
	call(t, ct, covC5ID, "finalizeEpoch", `{"epoch":"1"}`, lpReporter, 10, true)
	call(t, ct, covC5ID, "cancelEpoch", `{"epoch":"1"}`, "hive:coveve", 11, false)
	call(t, ct, covC5ID, "cancelEpoch", `{"epoch":"1"}`, covGuardian, 11, true)
	call(t, ct, covC5ID, "claim", `{"epoch":"1"}`, "hive:covlp1", 20, false)
	s := call(t, ct, covC5ID, "sweepUnallocated", `{"nonce":"1","to":"hive:coveve"}`, covGuardian, 20, true)
	assert.Equal(t, "100000", covJSONField(t, s.Ret, "swept"))
	assert.Equal(t, "100000", covBalance(t, ct, covTreasury, 20).String())
	assert.Equal(t, "0", covBalance(t, ct, "hive:coveve", 20).String())
	call(t, ct, covC5ID, "sweepUnallocated", `{"nonce":"2"}`, covGuardian, 20, false)
}

// ---------------------------------------------------------------------------
// C6 — migration airdrop
// ---------------------------------------------------------------------------

// covBootC6 registers + initializes the token and C6, then funds C6 with a
// bootstrap balance held by the contract itself.
// covBootC6WithTreasury is covBootC6 plus a pinned residual treasury, so the sweep
// path is reachable.
func covBootC6WithTreasury(t *testing.T, c6, maxAirdrop, bootstrap, treasury string) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c6, owner, read(covC6Wasm))
	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)
	call(t, &ct, c6, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","maxAirdrop":"%s","treasury":"%s"}`, covTokenID, maxAirdrop, treasury),
		owner, 0, true)
	call(t, &ct, covTokenID, "mint", fmt.Sprintf(`{"amount":"%s"}`, bootstrap), owner, 0, true)
	call(t, &ct, covTokenID, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"%s"}`, c6, bootstrap), owner, 0, true)
	return &ct
}

func covBootC6(t *testing.T, c6 string, maxAirdrop string, bootstrap string) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(c6, owner, read(covC6Wasm))
	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)
	call(t, &ct, c6, "init", fmt.Sprintf(`{"token":"%s","kind":"0","maxAirdrop":"%s"}`, covTokenID, maxAirdrop), owner, 0, true)
	call(t, &ct, covTokenID, "mint", fmt.Sprintf(`{"amount":"%s"}`, bootstrap), owner, 0, true)
	call(t, &ct, covTokenID, "transfer", fmt.Sprintf(`{"to":"contract:%s","amount":"%s"}`, c6, bootstrap), owner, 0, true)
	return &ct
}

func covAirdropTotal(t *testing.T, ct *test_utils.ContractTest, c6 string) string {
	t.Helper()
	r := call(t, ct, c6, "airdropTotal", ``, "hive:covquery", 0, true)
	return covJSONField(t, r.Ret, "total")
}

// Batches accumulate into airdrop_total, a batchId never applies twice, the
// maxAirdrop cap genuinely binds (and no tokens move when it does), malformed
// entries are skipped, and only the owner may airdrop.
func TestCovDist_C6BatchesCapAndMalformed(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBootC6(t, covC6ID, "1000", "5000")

	// two batches accumulate
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb1","entries":"hive:cova:300,hive:covb:200"}`, owner, 0, true)
	assert.Equal(t, "500", covAirdropTotal(t, ct, covC6ID))
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb2","entries":"hive:covc:200"}`, owner, 0, true)
	assert.Equal(t, "700", covAirdropTotal(t, ct, covC6ID))
	assert.Equal(t, "300", covBalance(t, ct, "hive:cova", 0).String())
	assert.Equal(t, "200", covBalance(t, ct, "hive:covb", 0).String())
	assert.Equal(t, "200", covBalance(t, ct, "hive:covc", 0).String())
	assert.Equal(t, "4300", covBalance(t, ct, "contract:"+covC6ID, 0).String())

	// the SAME batchId must never apply twice, even with different entries
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb1","entries":"hive:cova:300,hive:covb:200"}`, owner, 0, false)
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb1","entries":"hive:cova:1"}`, owner, 0, false)
	assert.Equal(t, "700", covAirdropTotal(t, ct, covC6ID))
	assert.Equal(t, "300", covBalance(t, ct, "hive:cova", 0).String())

	// a batch that would push the running total past cfg_maxAirdrop must abort —
	// and the whole tx must revert, so NO tokens move.
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb3","entries":"hive:covd:400"}`, owner, 0, false)
	assert.Equal(t, "0", covBalance(t, ct, "hive:covd", 0).String(), "capped batch must not transfer")
	assert.Equal(t, "700", covAirdropTotal(t, ct, covC6ID), "capped batch must not raise the total")
	assert.Equal(t, "4300", covBalance(t, ct, "contract:"+covC6ID, 0).String(), "C6 balance must be untouched")
	// a single over-cap entry in an otherwise fine batch takes the whole batch down
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb4","entries":"hive:cove:1,hive:covd:400"}`, owner, 0, false)
	assert.Equal(t, "0", covBalance(t, ct, "hive:cove", 0).String())

	// a non-owner cannot airdrop
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb5","entries":"hive:coveve:1"}`, "hive:coveve", 0, false)
	assert.Equal(t, "0", covBalance(t, ct, "hive:coveve", 0).String())

	// malformed entries are skipped without disturbing the total: only covg:100 counts
	call(t, ct, covC6ID, "airdropBatch",
		`{"batchId":"covb6","entries":"hive:covf:0,:5,hive:covd:-3,nocolonentry,hive:covg:100,hive:covh:"}`, owner, 0, true)
	assert.Equal(t, "800", covAirdropTotal(t, ct, covC6ID))
	assert.Equal(t, "100", covBalance(t, ct, "hive:covg", 0).String())
	for _, a := range []string{"hive:covf", "hive:covd", "hive:covh", "nocolonentry"} {
		assert.Equal(t, "0", covBalance(t, ct, a, 0).String(), "malformed entry must not pay %q", a)
	}

	// the cap is inclusive: hitting it exactly is fine, exceeding it by 1 is not
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb7","entries":"hive:covi:200"}`, owner, 0, true)
	assert.Equal(t, "1000", covAirdropTotal(t, ct, covC6ID))
	call(t, ct, covC6ID, "airdropBatch", `{"batchId":"covb8","entries":"hive:covj:1"}`, owner, 0, false)
	assert.Equal(t, "1000", covAirdropTotal(t, ct, covC6ID))
	assert.Equal(t, "0", covBalance(t, ct, "hive:covj", 0).String())
	// C6 still holds the untouched remainder of the bootstrap balance
	assert.Equal(t, "4000", covBalance(t, ct, "contract:"+covC6ID, 0).String())
}

// init must reject a missing / zero / negative maxAirdrop — without the cap an
// owner-key compromise drains the whole bootstrap balance.
func TestCovDist_C6InitRejectsBadCap(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(covC6bID, owner, read(covC6Wasm))
	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)

	base := `{"token":"` + covTokenID + `","kind":"0"`
	call(t, &ct, covC6bID, "init", base+`}`, owner, 0, false)                   // missing
	call(t, &ct, covC6bID, "init", base+`,"maxAirdrop":"0"}`, owner, 0, false)  // zero
	call(t, &ct, covC6bID, "init", base+`,"maxAirdrop":"-5"}`, owner, 0, false) // negative
	call(t, &ct, covC6bID, "init", base+`,"maxAirdrop":""}`, owner, 0, false)   // empty
	// still uninitialized after all of the above
	call(t, &ct, covC6bID, "airdropTotal", ``, "hive:covquery", 0, false)
	// a positive cap works
	call(t, &ct, covC6bID, "init", base+`,"maxAirdrop":"1"}`, owner, 0, true)
	assert.Equal(t, "0", covAirdropTotal(t, &ct, covC6bID))
}

// The stale-rescue deadline must be anchored to when the funding ACTUALLY ARRIVED,
// not to the epoch's place in the schedule.
//
// This became reachable when C2 stopped latching on exhaustion. Backlog funding is
// now a designed mode: a starved schedule resumes on refill and pays epoch indices
// long past their epochEnd. Anchored on epochEnd alone, those epochs arrive with the
// rescue gate ALREADY OPEN — so a guardian, or an automated "cancel funded but
// unfinalized" monitor, can cancel them the block the money lands, while the reporter
// is still submitting in good faith. The funding then goes to the treasury, not the
// earners.
//
// epochLen=1 and genesis=0, so epochEnd(0)=0 and staleBlocks()=1000 (the floor).
func TestCovDist_StaleRescueAnchorsOnFundedAtNotEpochEnd(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "10", covReporter)

	// Fund epoch 0 LATE, the way a backlog epoch is funded after a pool refill.
	// Under the old rule its rescue window opened at block 1000 — 500 blocks BEFORE
	// the money even arrived.
	const fundedAt = 1500
	covFundEpoch(t, ct, covC3ID, "0", fundedAt)

	// Past the old epochEnd+stale deadline (1000) but not past fundedAt+stale (2500):
	// the guardian must be REFUSED. This is the assertion the old code failed.
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covGuardian, 2000, false)
	_, _, funded, status := covShareOf(t, ct, covC3ID, "0", "hive:cova", 2000)
	assert.Equal(t, "", status, "the epoch must still be open")
	assert.NotEqual(t, "0", funded, "the funding must not have been swept away")

	// The reporter has time to do its job inside that window.
	covSubmit(t, ct, covC3ID, covReporter, "0", "0", "hive:cova:60,hive:covb:40", 2100, true)
	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 2100, true)
	_, _, _, status = covShareOf(t, ct, covC3ID, "0", "hive:cova", 2100)
	assert.Equal(t, "finalized", status)
}

// The rescue still works — it is only DELAYED, never removed. A genuinely abandoned
// epoch must remain recoverable once the funding-anchored deadline passes.
func TestCovDist_StaleRescueStillOpensAfterTheFundedAnchor(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "10", covReporter)
	const fundedAt = 1500
	covFundEpoch(t, ct, covC3ID, "0", fundedAt)

	// still shut just before fundedAt + staleBlocks()
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covGuardian, 2499, false)
	// and open just after
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covGuardian, 2501, true)
	_, _, funded, status := covShareOf(t, ct, covC3ID, "0", "hive:cova", 2501)
	assert.Equal(t, "cancelled", status)
	assert.Equal(t, "0", funded, "funding rolls into unallocated for the guardian to sweep")
}

// An epoch holding no money must not be cancellable through the rescue branch at all:
// the branch exists to recover funds, and letting a bookkeeping key open it would let
// a cancel lock an epoch out of ever being funded.
func TestCovDist_StaleRescueRequiresActualFunding(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "10", covReporter)
	// never funded — far past any conceivable deadline
	call(t, ct, covC3ID, "cancelEpoch", `{"epoch":"0"}`, covGuardian, 99999, false)
}

// covBootRaw brings up the token + C2 only, so a test can drive several distributor
// inits against one chain and observe which are accepted.
func covBootRaw(t *testing.T) *test_utils.ContractTest {
	t.Helper()
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(covTokenID, owner, read(tokenWasmPath))
	ct.RegisterContract(covC2ID, owner, read(covC2Wasm))
	call(t, &ct, covTokenID, "init", covTokenInit(), owner, 0, true)
	call(t, &ct, covC2ID, "init", covC2Init(covC3ID), owner, 0, true)
	return &ct
}

// The challenge window is immutable after init and gates BOTH when claims open and
// when the guardian's veto expires. A fat-fingered value — an extra zero, or blocks
// confused with seconds — is unrecoverable: claims never open, the veto never
// expires, and the funding is stuck with no correction path. covC2Init uses
// epochLen=1, so the bound is 10 blocks.
func TestCovDist_InitRejectsAnAbsurdChallengeWindow(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBootRaw(t)

	ct.RegisterContract(covC3ID, owner, read(covC3Wasm))
	call(t, ct, covC3ID, "init", covDistInit("11", covReporter), owner, 0, false)

	// exactly at the bound is still legal — the guard must reject typos, not tighten
	// the contract's stated policy
	ct.RegisterContract(covC3bID, owner, read(covC3Wasm))
	call(t, ct, covC3bID, "init", covDistInit("10", covReporter), owner, 0, true)
}

// The role label lets a reporter tell a content distributor from an LP one. Nothing
// else can: C3 and C5 are the same code deployed twice, so a swapped id in a reporter
// config passes every other cross-check and would score an epoch from the wrong data
// source. It is optional, so existing deployments and payloads are unaffected.
func TestCovDist_RoleLabelIsOptionalAndValidated(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBootRaw(t)

	withRole := func(role string) string {
		base := covDistInit("5", covReporter)
		return base[:len(base)-1] + fmt.Sprintf(`,"role":"%s"}`, role)
	}

	// a value that is neither "content" nor "lp" is rejected outright
	ct.RegisterContract(covC3ID, owner, read(covC3Wasm))
	call(t, ct, covC3ID, "init", withRole("distributor"), owner, 0, false)

	// a legal one is accepted and readable back
	ct.RegisterContract(covC3bID, owner, read(covC3Wasm))
	call(t, ct, covC3bID, "init", withRole("lp"), owner, 0, true)

	// and omitting it entirely stays legal
	ct.RegisterContract(covC5ID, owner, read(covC5Wasm))
	call(t, ct, covC5ID, "init", covDistInit("5", covReporter), owner, 0, true)
}

// C6 holds the largest single balance at launch and, until now, had NO exit for it.
// Value could leave only through airdropBatch, bounded by maxAirdrop, so every token
// above the cap — and every one under it the snapshot did not need, and every entry
// the ledger-address filter skipped — was locked in the contract forever. C3/C5 have
// sweepUnallocated and C7 has sweepResidual; C6 had nothing.
func TestCovDist_C6ResidualSweepOnlyTakesTheExcess(t *testing.T) {
	os.RemoveAll("data/badger")
	// bootstrap 1000, cap 600 -> 400 can never be airdropped
	ct := covBootC6WithTreasury(t, covC6ID, "600", "1000", covTreasury)

	// while the full capacity is unspent, NOTHING is residual beyond the excess
	call(t, ct, covC6ID, "sweepResidual", `{}`, owner, 1, true)
	assert.Equal(t, "400", covBalance(t, ct, covTreasury, 1).String(),
		"only the un-airdroppable excess may move")
	assert.Equal(t, "600", covBalance(t, ct, "contract:"+covC6ID, 1).String(),
		"every token the remaining capacity could still pay must stay put")

	// a second sweep has nothing left to take
	call(t, ct, covC6ID, "sweepResidual", `{}`, owner, 2, false)

	// after airdropping part of the capacity, the freed reservation becomes sweepable
	call(t, ct, covC6ID, "airdropBatch",
		`{"batchId":"b1","entries":"hive:cova:200"}`, owner, 3, true)
	call(t, ct, covC6ID, "sweepResidual", `{}`, owner, 4, false) // still fully reserved
	assert.Equal(t, "400", covBalance(t, ct, "contract:"+covC6ID, 4).String())
}

// The sweep is owner-only and must not be reachable with a posting key: it moves the
// bootstrap balance, which is exactly what CRIT-2 exists to prevent.
func TestCovDist_C6SweepIsOwnerAndActiveOnly(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBootC6WithTreasury(t, covC6ID, "600", "1000", covTreasury)
	call(t, ct, covC6ID, "sweepResidual", `{}`, "hive:coveve", 1, false)
	pvCallPosting(t, ct, covC6ID, "sweepResidual", `{}`, owner, 1, false)
	assert.Equal(t, "0", covBalance(t, ct, covTreasury, 1).String())
}

// Omitting the treasury is a deliberate choice — it keeps the stronger promise that
// nothing can ever be reclaimed — and must fail closed rather than sweep somewhere.
func TestCovDist_C6WithoutTreasuryCannotSweep(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBootC6(t, covC6ID, "600", "1000")
	call(t, ct, covC6ID, "sweepResidual", `{}`, owner, 1, false)
}

// SCALE. Every other test in this repo reports a handful of accounts; a real tribe
// epoch has hundreds. This drives 500 earners across 9 pages and checks the things
// that only break at size: cumulative rounding, totalShares accumulation across many
// submitShares calls, and whether the sum of what everyone can actually claim still
// equals what was funded.
//
// 500 x 60-per-page is also the RC shape an operator will really run — see
// docs/rc-costs.md — so a failure here is a deployment blocker rather than a
// curiosity.
func TestCovDist_FiveHundredEarnersAcrossNinePages(t *testing.T) {
	os.RemoveAll("data/badger")
	ct := covBoot(t, covC3Wasm, covC3ID, "1", covReporter)
	covFundEpoch(t, ct, covC3ID, "0", 1)

	const total, perPage = 500, 60
	type earner struct {
		acct  string
		share int
	}
	earners := make([]earner, 0, total)
	for i := 0; i < total; i++ {
		// deliberately uneven shares, including many that will round down
		earners = append(earners, earner{fmt.Sprintf("hive:s%03d", i), 1 + i%37})
	}

	wantTotal := 0
	page := 0
	for i := 0; i < len(earners); i += perPage {
		end := i + perPage
		if end > len(earners) {
			end = len(earners)
		}
		entries := ""
		for j := i; j < end; j++ {
			if j > i {
				entries += ","
			}
			entries += fmt.Sprintf("%s:%d", earners[j].acct, earners[j].share)
			wantTotal += earners[j].share
		}
		covSubmit(t, ct, covC3ID, covReporter, "0", strconv.Itoa(page), entries, 1, true)
		page++
	}
	t.Logf("submitted %d earners across %d pages, totalShares should be %d", total, page, wantTotal)

	call(t, ct, covC3ID, "finalizeEpoch", `{"epoch":"0"}`, covReporter, 1, true)
	_, ts, funded, status := covShareOf(t, ct, covC3ID, "0", earners[0].acct, 2)
	assert.Equal(t, "finalized", status)
	assert.Equal(t, strconv.Itoa(wantTotal), ts,
		"totalShares must accumulate exactly across every page")

	// Everyone claims. The invariant that matters at scale is CONSERVATION: the sum
	// actually paid must never exceed what was funded, and the shortfall must be pure
	// truncation dust rather than a systematic leak.
	fundedN, _ := strconv.Atoi(funded)
	paid := 0
	claimed := 0
	for _, e := range earners {
		before := covBalance(t, ct, e.acct, 3).String()
		if before != "0" {
			t.Fatalf("%s started with a balance of %s", e.acct, before)
		}
		expect := fundedN * e.share / wantTotal
		r := call(t, ct, covC3ID, "claim", `{"epoch":"0"}`, e.acct, 3, expect > 0)
		_ = r
		got := covBalance(t, ct, e.acct, 3)
		if got.String() != strconv.Itoa(expect) {
			t.Fatalf("%s got %s, want %d (share %d of %d, funded %d)",
				e.acct, got, expect, e.share, wantTotal, fundedN)
		}
		paid += expect
		if expect > 0 {
			claimed++
		}
	}
	t.Logf("scale OK: %d/%d earners paid %d of %d funded; %d retained as truncation dust",
		claimed, total, paid, fundedN, fundedN-paid)
	if paid > fundedN {
		t.Fatalf("CONSERVATION BROKEN: paid %d exceeds funded %d", paid, fundedN)
	}
	// dust must be a rounding remainder, not a systematic loss
	if fundedN-paid > total {
		t.Fatalf("retained %d for %d earners — more than one unit each, so this is not "+
			"truncation dust but a systematic shortfall", fundedN-paid, total)
	}
}
