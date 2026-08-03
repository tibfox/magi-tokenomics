// C7 — Staking-Yield Distributor (plan §19.2). Pays a pro-rata yield to stakers of
// a staking source (C1) from a C2 bucket. FULLY TRUSTLESS: no reporter — it reads
// on-chain stake snapshots. Pull model (§21/F1): pullFunding pulls the epoch slice
// from C2; claim pays funded[ep]*stakeAt(h_e)/totalAt(h_e), h_e=genesis+ep*epochLen.
package main

import (
	"magi_token/adapter"
	"magi_token/auth"
	"magi_token/sdk"
	"math/big"
	"strconv"
)

func main() {}

const kInit = "init"

// Init: {"token","kind","tokenId","funder"(C2),"stakeSource"(C1),"genesis","epochLen"}
//
//go:wasmexport init
func Init(payload *string) *string {
	if present(kInit) {
		sdk.Abort("already initialized")
	}
	owner := sdk.GetEnvKey("contract.owner")
	caller := sdk.GetEnvKey("msg.caller")
	if owner == nil || caller == nil || *owner != *caller {
		sdk.Abort("only owner can init")
	}
	validateAddr(f(payload, "token"))
	validateAddr(f(payload, "funder"))
	validateAddr(f(payload, "stakeSource"))
	set("cfg_token", f(payload, "token"))
	// FAIL CLOSED on the editioned-NFT value asset: kind must be "0". See the
	// adapter package doc — NFT mode's R4/R16 prerequisites are not implemented,
	// so a kind="1" deployment must be impossible, not merely broken later.
	set("cfg_kind", adapter.RequireFungible(f(payload, "kind")))
	adapter.RequireNoTokenId(f(payload, "tokenId"))
	set("cfg_tokenId", "")
	set("cfg_funder", f(payload, "funder"))
	set("cfg_stake", f(payload, "stakeSource"))
	tre := f(payload, "treasury")
	if tre == "" {
		sdk.Abort("treasury required (residual sweep destination)")
	}
	validateAddr(tre)
	// Same ledger-address rule as C3/C5's treasury (their validateLedgerAddr). This
	// previously omitted system:, so the two sweep destinations in one framework had
	// two different validation rules with no stated reason — the kind of drift that
	// makes a reviewer assume the stricter one is deliberate when it is an accident.
	if !hasPrefix(tre, "hive:") && !hasPrefix(tre, "contract:") &&
		!hasPrefix(tre, "did:") && !hasPrefix(tre, "system:") {
		sdk.Abort("treasury must start with hive:/contract:/did:/system:")
	}
	// HIGH-4: a guardian-controlled treasury turns cancel+sweep into a drain, and a
	// self-pointing treasury bricks the sweep (token forbids transfer-to-self).
	if tre == selfAddr() {
		sdk.Abort("treasury cannot be this contract")
	}
	for _, g := range splitComma(f(payload, "guardianAuth")) {
		if g != "" && g == tre {
			sdk.Abort("treasury must not be a guardian authority")
		}
	}
	set("cfg_treasury", tre)
	gm, ga, gt := f(payload, "guardianMode"), f(payload, "guardianAuth"), f(payload, "guardianThreshold")
	set("cfg_gMode", gm)
	set("cfg_gAuth", ga)
	set("cfg_gThr", gt)
	auth.Validate(guardianCfg())
	// Schedule: ADOPT the funder's genesis/epochLen when they are omitted (the
	// normal case — C2's genesis auto-defaults to its deploy block, so an operator
	// cannot know it in advance and must not be made to copy it by hand). If they
	// ARE supplied, cross-check them so a misconfigured C7 can't silently snapshot
	// the wrong heights. Mirrors what C3/C5 already do.
	sch := sdk.ContractCall(f(payload, "funder"), "scheduleInfo", "", nil)
	sg, se := pickField(sch, "genesis"), pickField(sch, "epochLen")
	if sg == "" || se == "" {
		sdk.Abort("funder scheduleInfo unavailable — init the emission contract (C2) first")
	}
	if g := f(payload, "genesis"); g != "" && g != sg {
		sdk.Abort("genesis mismatch with funder — omit it to adopt the funder's schedule")
	}
	if e := f(payload, "epochLen"); e != "" && e != se {
		sdk.Abort("epochLen mismatch with funder — omit it to adopt the funder's schedule")
	}
	set("cfg_genesis", sg)
	set("cfg_epochLen", se)

	// CROSS-CHECK C1's R15 basis against the real emission schedule.
	//
	// R15 ("cooldown must exceed epochLen, so a one-block stake cannot capture a full
	// epoch of yield") is enforced inside C1 against an epochLen the OPERATOR supplies,
	// and C1 cannot verify it: the deploy order puts C1 before C2, because stake has to
	// exist before C2's genesis or epoch 0's yield is funded-but-unclaimable. So C1 has
	// nothing to ask when it runs.
	//
	// C7 is the first contract that knows both schedules, and it is the contract R15
	// exists to protect. A C1 carrying a typo'd or stale epochLen otherwise enforces a
	// cooldown that silently fails to cover an epoch.
	//
	si := sdk.ContractCall(f(payload, "stakeSource"), "scheduleInfo", "", nil)
	if si == nil {
		sdk.Abort("stakeSource scheduleInfo unavailable — C1 must be initialised before C7")
	}
	if c1El := pickField(si, "epochLen"); c1El != "" && c1El != se {
		sdk.Abort("stakeSource epochLen disagrees with the funder — C1's R15 cooldown check was made against the wrong epoch length")
	}
	// C1 must ALSO be anchored to the same genesis, because that is what decides which
	// epoch bucket each drawdown lands in — and the drawdown is what makes claim's
	// denominator exact (see C1's accumulator doc). A C1 that never adopted a schedule
	// reports an empty genesis and accumulates nothing; paying against it would divide
	// by an over-counting total, strand part of every epoch, and bring back the claim
	// deadline this design removed. Refusing at init is the only place that is visible.
	c1G := pickField(si, "genesis")
	if c1G == "" {
		sdk.Abort("stakeSource has no adopted schedule — call C1.adoptSchedule({funder}) after C2 is initialised, or C7 cannot compute an exact denominator")
	}
	if c1G != sg {
		sdk.Abort("stakeSource genesis disagrees with the funder — C1's drawdown buckets and C7's epoch snapshots would straddle each other")
	}
	set(kInit, "1")
	return ok()
}

// pullFunding — pull this epoch's slice from C2 (permissionless). Write-once:
// C2.claimBucket deletes owed on first pull, so a second call reverts the tx.
//
//go:wasmexport pullFunding
func PullFunding(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	res := sdk.ContractCall(getStr("cfg_funder"), "claimBucket", `{"epoch":"`+ep+`"}`, nil)
	got := parseBig(pickField(res, "claimed"))
	k := "funded|" + ep
	setBig(k, new(big.Int).Add(getBig(k), got))
	// `fundedAt` used to be recorded here to anchor the claim window against keeper
	// lag. With no window to anchor, keeping it would be state written every epoch and
	// read by nothing.
	return str(`{"funded":"` + getBig(k).String() + `"}`)
}

// claim — self-serve pro-rata yield. {"epoch"} — payout = funded*minStake/Σmin.
//
//go:wasmexport claim
func Claim(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	c := mustCaller()
	validateAddr(c) // sanitize before interpolating into cross-contract JSON (L2)
	ck := "claimed|" + ep + "|" + c
	if present(ck) {
		sdk.Abort("already claimed")
	}
	funded := getBig("funded|" + ep)
	if funded.Sign() <= 0 {
		sdk.Abort("epoch not funded yet")
	}
	set(ck, "1") // CEI: mark claimed BEFORE any external call (defense-in-depth, LOW-1)
	// Two epoch-boundary snapshots. Require the epoch FULLY elapsed so both are
	// historical (blocks future-snapshot config abuse), and credit the MINIMUM
	// stake over the epoch → only stakers committed for the WHOLE epoch earn,
	// defeating the 1-block flash-stake capture (H3).
	//
	// Conservation is now an EQUALITY rather than an inequality: the denominator below
	// is Σᵢ min(aᵢ,bᵢ) exactly, so Σ payouts = funded·(Σ minᵢ)/(Σ minᵢ) = funded, less
	// the ≤1 unit each payout loses to integer division. It used to be
	// Σ min(aᵢ,bᵢ) ≤ min(Σa,Σb), and the slack in that ≤ was the unclaimable residue.
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	hStart := g + pu(ep)*el
	hEnd := g + (pu(ep)+1)*el - 1 // LAST block of the epoch, not the first of the next
	if hEnd >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	// NO CLAIM DEADLINE. Yield stays claimable forever, exactly like C3 and C5.
	//
	// There used to be one, ~10 days wide. It existed only to make the residual sweep
	// safe: the denominator over-counted, every epoch left tokens no account could
	// claim, and a guardian sweep to recover them could not be allowed to take a slow
	// staker's share — so claims had to close first. The exact denominator below
	// removes the residual, which removes the sweep, which removes the deadline.
	if present("swept|" + ep) {
		sdk.Abort("epoch was swept as unclaimable")
	}
	sStart := stakeAt(c, hStart)
	sEnd := stakeAt(c, hEnd)
	stake := sStart
	if sEnd.Cmp(sStart) < 0 {
		stake = sEnd
	}
	if stake.Sign() <= 0 {
		sdk.Abort("not staked across the whole epoch")
	}
	// THE EXACT DENOMINATOR: Σᵢ min(aᵢ,bᵢ), read from C1's drawdown accumulator.
	//
	// It must use the same measure as the numerator or the two do not reconcile. This
	// previously used min(Σa,Σb) — the closest thing C7 could compute unaided — which
	// is ≥ Σ min(aᵢ,bᵢ) whenever stakers move in both directions during an epoch. The
	// difference was paid to nobody and needed a guardian sweep, and the sweep needed
	// a claim deadline. C1 now maintains the exact figure in O(1), so payouts sum to
	// `funded` less truncation dust and every one of those consequences disappears.
	total := minStakeSum(ep)
	if total.Sign() <= 0 {
		sdk.Abort("no stake held across the whole epoch")
	}
	payout := new(big.Int).Mul(funded, stake)
	payout.Div(payout, total)
	if payout.Sign() <= 0 {
		sdk.Abort("payout rounds to zero")
	}
	pk := "paid|" + ep
	setBig(pk, new(big.Int).Add(getBig(pk), payout)) // accounting for bounded sweep
	adapter.Transfer(asset(), c, payout)
	return str(`{"claimed":"` + payout.String() + `"}`)
}

// sweepEmptyEpoch — guardian recovers an epoch that NOBODY can ever claim.
//
// This replaces the old sweepResidual, and the difference is what let the claim
// deadline go. That one swept `funded-paid` after a grace period: an amount which is
// only unclaimable because claims were forced shut at the same moment. This one fires
// on a condition that is decided by history and can never change afterwards — no
// stake was held across the whole epoch, so the denominator is zero and every
// possible claim aborts. There is no slow claimant to strand, so no deadline is
// needed, so claims stay open forever.
//
// Epochs that DO have stakers are never swept at all. Their leftover is truncation
// dust, under one unit per claimant, and it stays in the contract exactly as C3's and
// C5's does — nobody, including the guardian, can take it.
//
//go:wasmexport sweepEmptyEpoch
func SweepEmptyEpoch(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	hEnd := g + (pu(ep)+1)*el - 1
	if hEnd >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	// The whole safety argument. Both snapshots are historical and immutable, so a
	// zero here is permanent: no account can ever produce a payout for this epoch.
	if minStakeSum(ep).Sign() != 0 {
		sdk.Abort("epoch has stakers — their yield is claimable forever and is not sweepable")
	}
	funded := getBig("funded|" + ep)
	residual := new(big.Int).Sub(funded, getBig("paid|"+ep))
	if residual.Sign() <= 0 {
		sdk.Abort("nothing to sweep for epoch")
	}
	ak := "sweepempty:" + ep
	if !auth.Authorize(guardianCfg(), "grd", ak, ak, mustCaller(), reqAuths()) {
		return str(`{"swept":false}`)
	}
	set("swept|"+ep, "1") // CEI: close the epoch before transferring
	setBig("paid|"+ep, funded)
	adapter.Transfer(asset(), getStr("cfg_treasury"), residual)
	return str(`{"swept":"` + residual.String() + `"}`)
}

func guardianCfg() auth.Config {
	var mode auth.Mode
	switch getStr("cfg_gMode") {
	case "0":
		mode = auth.ModeSingle
	case "1":
		mode = auth.ModeCosigned
	case "2":
		mode = auth.ModeAttest
	default:
		sdk.Abort("auth: unknown mode")
	}
	thr, terr := strconv.Atoi(getStr("cfg_gThr"))
	if terr != nil || thr < 1 {
		sdk.Abort("auth threshold must be a positive integer") // HIGH-1: no silent 1-of-N
	}
	return auth.Config{Mode: mode, Authorities: splitComma(getStr("cfg_gAuth")), Threshold: thr}
}

func reqAuths() []string {
	env := sdk.GetEnv()
	out := []string{}
	for _, a := range env.Sender.RequiredAuths {
		out = append(out, a.String())
	}
	return out
}

func splitComma(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

//go:wasmexport fundedOf
func FundedOf(payload *string) *string {
	assertInit()
	return str(`{"funded":"` + getBig("funded|"+mustEpoch(payload)).String() + `"}`)
}

// ---- helpers -------------------------------------------------------------

func assertInit() {
	if !present(kInit) {
		sdk.Abort("not initialized")
	}
}
func asset() adapter.Asset {
	k := adapter.Fungible
	if getStr("cfg_kind") == "1" {
		k = adapter.EditionedNFT
	}
	return adapter.Asset{Kind: k, Contract: getStr("cfg_token"), TokenId: getStr("cfg_tokenId")}
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if s[i] != p[i] {
			return false
		}
	}
	return true
}

func present(k string) bool { v := sdk.StateGetObject(k); return v != nil && *v != "" }

func stakeAt(acct string, h uint64) *big.Int {
	hs := strconv.FormatUint(h, 10)
	return parseBig(pickField(sdk.ContractCall(getStr("cfg_stake"), "stakeAtHeight",
		`{"account":"`+acct+`","height":"`+hs+`"}`, nil), "stake"))
}
func totalAt(h uint64) *big.Int {
	hs := strconv.FormatUint(h, 10)
	return parseBig(pickField(sdk.ContractCall(getStr("cfg_stake"), "totalStakedAtHeight",
		`{"height":"`+hs+`"}`, nil), "total"))
}

// minStakeSum = Σᵢ min(stakeᵢ(start), stakeᵢ(end)) for the epoch, from C1.
//
// An EMPTY answer is not zero and must never be treated as one: it means the C1 being
// read has no adopted schedule and is accumulating nothing. Zero would abort claim as
// "no stake held", which reads like an empty epoch rather than a misconfigured one.
// C7's init already refuses such a C1, so reaching this is a swapped/upgraded
// stakeSource — worth its own message rather than a misleading one.
func minStakeSum(ep string) *big.Int {
	raw := pickField(sdk.ContractCall(getStr("cfg_stake"), "minStakeSum",
		`{"epoch":"`+ep+`"}`, nil), "total")
	if raw == "" {
		sdk.Abort("stakeSource reports no adopted schedule — it cannot supply an exact denominator")
	}
	return parseBig(raw)
}
func blockHeight() uint64 {
	h := sdk.GetEnvKey("block.height")
	if h == nil {
		sdk.Abort("no height")
	}
	n, err := strconv.ParseUint(*h, 10, 64)
	if err != nil {
		sdk.Abort("bad height")
	}
	return n
}
func mustCaller() string {
	c := sdk.GetEnvKey("msg.caller")
	if c == nil {
		sdk.Abort("no caller")
	}
	return *c
}
func mustEpoch(payload *string) string {
	e := f(payload, "epoch")
	if e == "" {
		sdk.Abort("epoch required")
	}
	for i := 0; i < len(e); i++ {
		if e[i] < '0' || e[i] > '9' {
			sdk.Abort("epoch must be numeric")
		}
	}
	if len(e) > 1 && e[0] == '0' {
		sdk.Abort("epoch must be canonical")
	}
	if len(e) > 19 {
		sdk.Abort("epoch out of range") // LOW-3
	}
	return e
}
func pu(s string) uint64          { n, _ := strconv.ParseUint(s, 10, 64); return n }
func getU(k string) uint64        { return pu(getStr(k)) }
func set(k, v string)             { sdk.StateSetObject(k, v) }
func getBig(k string) *big.Int    { return parseBig(getStr(k)) }
func setBig(k string, v *big.Int) { set(k, v.String()) }
func parseBig(s string) *big.Int {
	n := new(big.Int)
	if s != "" {
		n.SetString(s, 10)
	}
	return n
}
func getStr(k string) string {
	if v := sdk.StateGetObject(k); v != nil {
		return *v
	}
	return ""
}
func validateAddr(a string) {
	if len(a) == 0 || len(a) > 256 {
		sdk.Abort("bad address")
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c == '|' || c == '"' || c == '\\' {
			sdk.Abort("illegal char in address")
		}
	}
}
func ok() *string          { return str(`{"success":true}`) }
func str(s string) *string { return &s }
func pickField(res *string, name string) string {
	if res == nil {
		return ""
	}
	return f(res, name)
}
func f(payload *string, name string) string {
	if payload == nil {
		return ""
	}
	s := *payload
	needle := `"` + name + `"`
	from := 0
	for {
		rel := indexOf(s[from:], needle)
		if rel < 0 {
			return ""
		}
		i := from + rel + len(needle)
		from = i
		k := i
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			continue
		}
		k++
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k < len(s) && s[k] == '"' {
			k++
			j := k
			for j < len(s) && s[j] != '"' {
				j++
			}
			return s[k:j]
		}
		j := k
		for j < len(s) && s[j] != ',' && s[j] != '}' && s[j] != ' ' {
			j++
		}
		return s[k:j]
	}
}
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		k := 0
		for k < m && s[i+k] == sub[k] {
			k++
		}
		if k == m {
			return i
		}
	}
	return -1
}

// selfAddr returns this contract's ledger identity.
//
// The nil check is not defensive noise. Contracts are built with -panic=trap, so a
// nil dereference produces a bare wasm trap with NO reason string — the operator sees
// a failed transaction and nothing else. An sdk.Abort at least names the cause. The
// adapter already does this; the contracts were dereferencing directly.
func selfAddr() string {
	id := sdk.GetEnvKey("contract.id")
	if id == nil {
		sdk.Abort("no contract.id in env")
	}
	return "contract:" + *id
}
