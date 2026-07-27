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
	if !hasPrefix(tre, "hive:") && !hasPrefix(tre, "contract:") && !hasPrefix(tre, "did:") {
		sdk.Abort("treasury must start with hive:/contract:/did:")
	}
	// HIGH-4: a guardian-controlled treasury turns cancel+sweep into a drain, and a
	// self-pointing treasury bricks the sweep (token forbids transfer-to-self).
	if tre == "contract:"+*sdk.GetEnvKey("contract.id") {
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
	if !present("fundedAt|" + ep) {
		// Anchor the claim window to when funding ACTUALLY arrived — the keeper
		// pokes are permissionless and may lag past hEnd+grace (MEDIUM).
		set("fundedAt|"+ep, strconv.FormatUint(blockHeight(), 10))
	}
	return str(`{"funded":"` + getBig(k).String() + `"}`)
}

// claim — self-serve pro-rata yield. {"epoch"} — payout = funded*stakeAt/totalAt.
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
	// defeating the 1-block flash-stake capture (H3). Conservation holds:
	// Σ min(start,end) ≤ Σ end = totalAt(hEnd) = denominator.
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	hStart := g + pu(ep)*el
	hEnd := g + (pu(ep)+1)*el - 1 // LAST block of the epoch, not the first of the next
	if hEnd >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	// Claims close at the same deadline the guardian may sweep from, so swept
	// funds are genuinely unclaimable and Σclaims≤funded holds (HIGH-1).
	if blockHeight() >= deadlineOf(ep, hEnd) {
		sdk.Abort("claim window closed")
	}
	if present("swept|" + ep) {
		sdk.Abort("epoch residual already swept")
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
	// Denominator MUST use the same measure as the numerator: Σ min(aᵢ,bᵢ) ≤ min(Σa,Σb).
	// Using only totalAt(hEnd) let a stake-at-end actor inflate the denominator and
	// burn everyone else's yield (HIGH-1 regression from the R1 flash-stake fix).
	total := totalAt(hEnd)
	if ts := totalAt(hStart); ts.Cmp(total) < 0 {
		total = ts
	}
	if total.Sign() <= 0 {
		sdk.Abort("no total stake at snapshot")
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

// sweepResidual — guardian recovers only the UNCLAIMABLE residue of ONE epoch
// (funded-paid), and only after a grace period. Never an arbitrary amount (HIGH-2).
//
//go:wasmexport sweepResidual
func SweepResidual(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	hEnd := g + (pu(ep)+1)*el - 1
	if blockHeight() < deadlineOf(ep, hEnd) {
		sdk.Abort("residual not mature yet")
	}
	funded := getBig("funded|" + ep)
	residual := new(big.Int).Sub(funded, getBig("paid|"+ep))
	if residual.Sign() <= 0 {
		sdk.Abort("no residual for epoch")
	}
	ak := "sweepres:" + ep
	if !auth.Authorize(guardianCfg(), "grd", ak, ak, mustCaller(), reqAuths()) {
		return str(`{"swept":false}`)
	}
	set("swept|"+ep, "1")     // CEI: close the epoch before transferring
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
// deadlineOf: claims close (and the sweep opens) at
// max(epoch end, funding arrival) + grace — so keeper lag never strands an epoch.
func deadlineOf(ep string, hEnd uint64) uint64 {
	anchor := hEnd
	if fa := getU("fundedAt|" + ep); fa > anchor {
		anchor = fa
	}
	return anchor + graceOf()
}

// graceOf: shared claim-deadline / sweep-maturity window.
func graceOf() uint64 {
	g := getU("cfg_epochLen") * 10
	if g < 1000 {
		g = 1000
	}
	return g
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
func pu(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
func getU(k string) uint64 { return pu(getStr(k)) }
func set(k, v string)          { sdk.StateSetObject(k, v) }
func getBig(k string) *big.Int { return parseBig(getStr(k)) }
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
