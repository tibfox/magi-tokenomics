// C3 — Author/Curation Distributor (plan §19.2, pull model §21/F1). Pulls its
// per-epoch funding from C2 (contract-authoritative amount, R1), accepts pushed
// per-account shares from the reporter (auth pkg, incl. attest M-of-N), computes
// totalShares ITSELF (R2), and pays via claim with Σclaims≤funded (invariant #1).
package main

import (
	"magi_token/adapter"
	"magi_token/auth"
	"magi_token/events"
	"magi_token/sdk"
	"math/big"
	"strconv"
)

func main() {}

const kInit = "init"

// Init: {"token","kind","tokenId","funder"(C2 id),"treasury",
//
//	"guardianMode","guardianAuth","guardianThreshold"}
//
// Reward channels — each with its own bucket, challenge window and reporter
// authority — are registered afterwards with addChannel.
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
	set("cfg_token", f(payload, "token"))
	// FAIL CLOSED on the editioned-NFT value asset: kind must be "0". See the
	// adapter package doc — NFT mode's R4/R16 prerequisites are not implemented,
	// so a kind="1" deployment must be impossible, not merely broken later.
	set("cfg_kind", adapter.RequireFungible(f(payload, "kind")))
	adapter.RequireNoTokenId(f(payload, "tokenId"))
	set("cfg_tokenId", "")
	set("cfg_funder", f(payload, "funder"))
	set("cfg_gMode", f(payload, "guardianMode"))
	set("cfg_gAuth", f(payload, "guardianAuth"))
	set("cfg_gThr", f(payload, "guardianThreshold"))
	auth.Validate(guardianCfg())
	// Pinned sweep destination — sweepUnallocated can ONLY send here, so a malicious
	// guardian cannot cancel+sweep an epoch to an arbitrary address (H2).
	tre := f(payload, "treasury")
	if tre == "" {
		sdk.Abort("treasury required (cancel/sweep destination)")
	}
	validateAddr(tre)
	validateLedgerAddr(tre) // typo-proof the immutable payout destination
	// A guardian-controlled treasury turns cancel+sweep into a drain, and a
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
	// Pull the emission schedule from the funder so staleness is measured against
	// the EPOCH CADENCE, not the (permissionless) pullFunding time (HIGH-1). This
	// also cross-checks that distributor and emitter agree on the schedule.
	sch := sdk.ContractCall(f(payload, "funder"), "scheduleInfo", "", nil)
	sg, se := pickField(sch, "genesis"), pickField(sch, "epochLen")
	if sg == "" || se == "" {
		sdk.Abort("funder scheduleInfo unavailable — init C2 first")
	}
	set("cfg_genesis", sg)
	set("cfg_epochLen", se)
	// STAKED PAYOUTS. A pool may deliver part of every claim as stake rather than
	// liquid tokens — SCOT's staked_reward_percentage — which ties earners to the
	// token instead of handing them something sellable the moment it lands.
	//
	// Capability follows config: with no stakeContract there is no split and claim
	// pays entirely liquid, exactly as before. Both are pinned here and immutable,
	// because a redirectable stake target is a redirectable payout.
	if sc := f(payload, "stakeContract"); sc != "" {
		validateAddr(sc)
		bpsStr := f(payload, "stakedBps")
		bps, berr := strconv.ParseUint(bpsStr, 10, 64)
		if berr != nil || bps == 0 || bps > 10000 {
			sdk.Abort("stakedBps must be 1..10000 when stakeContract is set")
		}
		// The staking contract must agree about the asset, or a claim would credit
		// stake denominated in something the claimant never earned.
		si := sdk.ContractCall(sc, "scheduleInfo", "", nil)
		if tok := pickField(si, "token"); tok != "" && tok != f(payload, "token") {
			sdk.Abort("stakeContract holds a different token than this distributor")
		}
		set("cfg_stakeContract", sc)
		set("cfg_stakedBps", bpsStr)
	}
	// Reward channels are added afterwards with addChannel, because verifying each
	// one's bucket requires calling the funder and a deployment may carry several.
	set(kInit, "1")
	// Discovery anchor for the indexer — see the note on c2_init.
	events.New("c3_init").
		Str("token", f(payload, "token")).
		Str("owner", *owner).
		Str("funder", f(payload, "funder")).
		Str("treasury", tre).
		Str("genesis", sg).
		Str("epoch_len", se).
		Str("guardian_mode", f(payload, "guardianMode")).
		Str("guardian_auth", f(payload, "guardianAuth")).
		Str("guardian_threshold", f(payload, "guardianThreshold")).
		Emit()
	return ok()
}

// pullFunding — pull this epoch's slice from C2 (permissionless trigger).
// Records funded[epoch] from the amount actually received (R1).
//
//go:wasmexport pullFunding
func PullFunding(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	if statusOf(ch, ep) != "" {
		sdk.Abort("epoch finalized/cancelled — funding locked") // LOW: no late growth
	}
	res := sdk.ContractCall(getStr("cfg_funder"), "claimBucket",
		`{"epoch":"`+ep+`","bucket":"`+getStr("ch_bucket|"+ch)+`"}`, nil)
	got := parseBig(pickField(res, "claimed"))
	k := "funded|" + ch + "|" + ep
	setBig(k, new(big.Int).Add(getBig(k), got))
	if !present("fundedAt|" + ch + "|" + ep) {
		set("fundedAt|"+ch+"|"+ep, strconv.FormatUint(blockHeight(), 10)) // MED-2 staleness clock
	}
	events.New("pull").
		Str("channel", ch).
		Str("epoch", ep).
		Big("received", got).
		Big("cumulative", getBig(k)).
		U64("funded_at", getU("fundedAt|"+ch+"|"+ep)).
		Emit()
	return str(`{"funded":"` + getBig(k).String() + `"}`)
}

// submitShares — reporter pushes a page of {acct:shares,...}. Contract accumulates
// totalShares (R2). Auth via the configured mode; in attest mode the SAME page must
// be pushed by >=threshold reporter accounts before it applies.
// {"epoch","page","entries":"acct:shares,acct:shares"}
//
//go:wasmexport submitShares
func SubmitShares(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	if statusOf(ch, ep) != "" {
		sdk.Abort("epoch not open")
	}
	entries := f(payload, "entries")
	page := canon(f(payload, "page"), "page") // canonical → no "0"/"00" idempotency bypass (M1)
	actionKey := "ss:" + ch + ":" + ep + ":" + page
	committed := auth.Authorize(reporterCfg(ch), "rep", actionKey, entries, mustCaller(), reqAuths())
	if committed {
		// Apply each (epoch,page) exactly once across ALL auth modes (H2/LOW-1).
		ak := "ssdone|" + ch + "|" + ep + "|" + page
		if present(ak) {
			sdk.Abort("page already applied")
		}
		set(ak, "1")
		applyEntries(ch, ep, page, entries)
	}
	return str(`{"applied":` + boolStr(committed) + `}`)
}

// emitSkip records an entry that submitShares silently dropped.
//
// This is the most valuable log this contract emits. A skipped entry is a no-op
// inside a transaction that SUCCEEDS: the page is marked applied, totalShares
// looks correct, and the earner is simply never paid. Nothing in the return value
// or the state records that the entry was ever present, so a reporter shipping
// one typo'd account produced an invisible loss. Logs survive only successful
// transactions — which is precisely this case.
func emitSkip(ch, ep, page, raw, reason string) {
	events.New("skip").
		Str("scope", "shares").
		Str("channel", ch).
		Str("epoch", ep).
		Str("page", page).
		Str("raw_entry", raw).
		Str("reason", reason).
		Emit()
}

func applyEntries(ch, ep, page, entries string) {
	total := getBig("totalShares|" + ch + "|" + ep)
	counted := 0
	pageTotal := new(big.Int)
	for _, e := range splitComma(entries) {
		acct, sh := split2(e)
		if acct == "" {
			emitSkip(ch, ep, page, e, "no account")
			continue
		}
		validateAddr(acct)
		// Share recipients are PAYOUT DESTINATIONS and need a real ledger domain.
		// Skipped, not aborted, to match how every other malformed entry is handled.
		// Before this check an entry like "alice:100" was COUNTED into totalShares
		// and then unclaimable forever — `claim` looks up share|<ep>|hive:alice and
		// finds nothing — so it silently diluted everyone else and stranded that
		// slice of the funding permanently.
		if !isLedgerAddr(acct) {
			emitSkip(ch, ep, page, e, "not a ledger address")
			continue
		}
		s := parseBig(sh)
		if s.Sign() <= 0 {
			emitSkip(ch, ep, page, e, "shares not positive")
			continue
		}
		sk := "share|" + ch + "|" + ep + "|" + acct
		setBig(sk, new(big.Int).Add(getBig(sk), s))
		total.Add(total, s)
		counted++
		pageTotal.Add(pageTotal, s)
	}
	setBig("totalShares|"+ch+"|"+ep, total)
	// ONE log per page carrying the submitted entries verbatim, rather than one log
	// per entry.
	//
	// Both shapes reconstruct the share book exactly — applied = submitted minus
	// whatever the skip logs above named — but the per-entry shape cost 229 RC an
	// entry against 91 before, taking a 60-entry page from 5,942 RC to 14,127 and a
	// 500-earner epoch from ~44,000 to ~127,000. submitShares is the single
	// most-repeated privileged call in the system and the reporter is its heaviest
	// RC consumer, so the page-level form is the right default: it holds the same
	// information for a fraction of the spend.
	//
	// The entries string is bounded by the 4,096-byte payload cap that already binds
	// before RC does, so this log cannot grow unboundedly either.
	events.New("shares").
		Str("channel", ch).
		Str("epoch", ep).
		Str("page", page).
		Int("entries", counted).
		Big("page_total", pageTotal).
		Big("new_total", total).
		Str("submitted", entries).
		Emit()
}

// finalizeEpoch — reporter freezes the epoch and opens the challenge window.
//
//go:wasmexport finalizeEpoch
func FinalizeEpoch(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	if statusOf(ch, ep) != "" {
		sdk.Abort("already finalized/cancelled")
	}
	// Must be funded before finalize, else pullFunding is locked out afterwards and
	// the epoch's C2 allocation is stranded forever (⓸).
	if getBig("funded|"+ch+"|"+ep).Sign() <= 0 {
		sdk.Abort("epoch not funded — pull funding first")
	}
	// An empty report would freeze the epoch (every claim aborts "no shares"),
	// so reject finalizing with nothing to distribute (MED-5).
	if getBig("totalShares|"+ch+"|"+ep).Sign() <= 0 {
		sdk.Abort("no shares submitted for epoch")
	}
	// Gate on the auth result — in Attest mode this must reach threshold (CRIT-1).
	// The attested payload is a CONSTANT, not chain state.
	//
	// It used to bind totalShares:funded, read at attestation time. But submitShares
	// stays open until status is set — which finalize itself sets only on commit — so
	// totalShares provably moves while a finalize vote is pending. Two honest
	// reporters attesting at different page-completion points therefore produced
	// DIFFERENT payloads. The tally is per payload hash while the seen-marker is one
	// per (action, authority) with no payload component and no way to clear it, so
	// each burned its only vote in a different bucket, the threshold was never
	// reachable, and the epoch became permanently unfinalizable — recoverable only by
	// a guardian cancel that pays the treasury rather than the earners.
	//
	// Anti-equivocation is untouched: one vote per authority per action still holds.
	// cancelEpoch already attests over a constant for the same reason.
	if !auth.Authorize(reporterCfg(ch), "rep", "fin:"+ch+":"+ep, "fin:"+ch+":"+ep, mustCaller(), reqAuths()) {
		return str(`{"finalized":false}`) // pending more attestations
	}
	set("status|"+ch+"|"+ep, "finalized")
	chal := blockHeight() + getU("ch_window|"+ch)
	set("chal|"+ch+"|"+ep, strconv.FormatUint(chal, 10))
	events.New("epoch_status").
		Str("channel", ch).
		Str("epoch", ep).
		Str("status", "finalized").
		U64("challenge_height", chal).
		Big("total_shares", getBig("totalShares|"+ch+"|"+ep)).
		Big("funded", getBig("funded|"+ch+"|"+ep)).
		Big("rolled_amount", new(big.Int)).
		Emit()
	return ok()
}

// cancelEpoch — guardian veto during the challenge window (rolls funds forward).
//
//go:wasmexport cancelEpoch
func CancelEpoch(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	// Either: finalized and still inside the challenge window (the veto), OR the
	// epoch was funded but never finalized for > staleness (rescue, MED-2) — else
	// a silent reporter would strand the funding forever.
	st := statusOf(ch, ep)
	if st == "finalized" {
		if blockHeight() >= getU("chal|"+ch+"|"+ep) {
			sdk.Abort("challenge window elapsed")
		}
	} else if st == "" {
		// Rescue only an epoch that actually HOLDS money here.
		if getBig("funded|"+ch+"|"+ep).Sign() <= 0 {
			sdk.Abort("epoch not funded")
		}
		// Anchor on the LATER of the schedule and the block the funding actually
		// arrived. C7 used the same rule for its claim deadline until that deadline
		// was removed entirely (its denominator is now exact, so it has no residue to
		// sweep and no reason to close claims); this stale-rescue anchor is unrelated
		// and stays.
		//
		// Anchoring on epochEnd alone broke once C2 stopped latching on exhaustion.
		// Backlog funding is now a designed mode: a starved schedule resumes when the
		// pool is refilled and pays epoch indices long past their epochEnd, so their
		// rescue gate was ALREADY OPEN the block the money landed — and pullFunding is
		// permissionless, so anyone could start that clock. A guardian, or an
		// automated stale-cancel monitor, would convert a whole refilled backlog to
		// unallocated while the reporter was still submitting in good faith.
		//
		// max() can only ever REFUSE a cancel the old rule allowed, never permit a new
		// one, so the HIGH-1 property (a reporter working a live epoch cannot be
		// front-run) is preserved by construction.
		anchor := epochEnd(ep)
		if fa := getU("fundedAt|" + ch + "|" + ep); fa > anchor {
			anchor = fa
		}
		if blockHeight() < anchor+staleBlocks(ch) {
			sdk.Abort("epoch not finalized (and not stale yet)")
		}
	} else {
		sdk.Abort("epoch already cancelled")
	}
	// Gate on the auth result (CRIT-1: Attest-mode guardian needs threshold).
	if !auth.Authorize(guardianCfg(), "grd", "cancel:"+ch+":"+ep, "cancel:"+ch+":"+ep, mustCaller(), reqAuths()) {
		return str(`{"cancelled":false}`)
	}
	set("status|"+ch+"|"+ep, "cancelled")
	// Roll the pulled funding forward into the unallocated pool (M-B/R11) so it
	// is not stranded — recoverable via sweepUnallocated.
	fk := "funded|" + ch + "|" + ep
	rolled := new(big.Int)
	if amt := getBig(fk); amt.Sign() > 0 {
		setBig("unalloc|"+ch, new(big.Int).Add(getBig("unalloc|"+ch), amt))
		setBig(fk, new(big.Int))
		rolled = amt
	}
	events.New("epoch_status").
		Str("channel", ch).
		Str("epoch", ep).
		Str("status", "cancelled").
		U64("challenge_height", getU("chal|"+ch+"|"+ep)).
		Big("total_shares", getBig("totalShares|"+ch+"|"+ep)).
		Big("funded", new(big.Int)).
		Big("rolled_amount", rolled).
		Emit()
	return ok()
}

// sweepUnallocated — guardian moves the rolled-forward (cancelled-epoch) pool to
// a target. {"to","nonce"} — nonce (numeric) makes it repeatable in Attest mode.
//
//go:wasmexport sweepUnallocated
func SweepUnallocated(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	to := getStr("cfg_treasury") // pinned at init — NOT caller-chosen (H2)
	if to == "" {
		sdk.Abort("no treasury configured")
	}
	nonce := canon(f(payload, "nonce"), "nonce")
	amt := getBig("unalloc|" + ch)
	// Bind the attestation to the AMOUNT — otherwise a co-signer approving a sweep
	// of 0 could be reused weeks later to move a large balance (MED). Authorize
	// BEFORE the emptiness check so a replayed nonce still reports "already
	// committed" rather than masking it behind an empty pool.
	ak := "sweep:" + ch + ":" + nonce
	if !auth.Authorize(guardianCfg(), "grd", ak, amt.String(), mustCaller(), reqAuths()) {
		return str(`{"swept":false}`)
	}
	if amt.Sign() <= 0 {
		sdk.Abort("nothing to sweep")
	}
	setBig("unalloc|"+ch, new(big.Int)) // CEI before transfer
	adapter.Transfer(asset(), to, amt)
	events.New("sweep_unallocated").
		Str("channel", ch).
		Big("amount", amt).
		Str("to", to).
		Str("nonce", nonce).
		Emit()
	return str(`{"swept":"` + amt.String() + `"}`)
}

// claim — self-serve payout after finalize + challenge window.
// {"epoch"} — pays funded[ep]*share[ep][caller]/totalShares[ep]. CEI + conservation.
//
//go:wasmexport claim
func Claim(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	if statusOf(ch, ep) != "finalized" {
		sdk.Abort("epoch not finalized")
	}
	if blockHeight() < getU("chal|"+ch+"|"+ep) {
		sdk.Abort("challenge window not elapsed")
	}
	c := mustCaller()
	ck := "claimed|" + ch + "|" + ep + "|" + c
	if present(ck) {
		sdk.Abort("already claimed")
	}
	funded := getBig("funded|" + ch + "|" + ep)
	if funded.Sign() <= 0 {
		sdk.Abort("epoch not funded yet") // don't burn the claimed flag (M5)
	}
	ts := getBig("totalShares|" + ch + "|" + ep)
	if ts.Sign() <= 0 {
		sdk.Abort("no shares")
	}
	share := getBig("share|" + ch + "|" + ep + "|" + c)
	if share.Sign() <= 0 {
		sdk.Abort("no share for caller")
	}
	payout := new(big.Int).Mul(funded, share)
	payout.Div(payout, ts)
	if payout.Sign() <= 0 {
		sdk.Abort("payout rounds to zero")
	}
	// CEI: mark claimed before any external call
	set(ck, "1")
	staked := stakedPart(payout)
	liquid := new(big.Int).Sub(payout, staked)
	if liquid.Sign() > 0 {
		adapter.Transfer(asset(), c, liquid)
	}
	if staked.Sign() > 0 {
		creditStake(c, staked)
	}
	// share and total_shares travel with the payout so funded*share/totalShares can
	// be re-derived off-chain — an amount on its own cannot be checked by anyone.
	// The liquid/staked split travels too, because the two are not interchangeable
	// to the recipient and a single total hides which they got.
	events.New("claim").
		Str("channel", ch).
		Str("epoch", ep).
		Str("acct", c).
		Big("payout", payout).
		Big("liquid", liquid).
		Big("staked", staked).
		Big("share", share).
		Big("total_shares", ts).
		Big("funded", funded).
		Emit()
	return str(`{"claimed":"` + payout.String() +
		`","liquid":"` + liquid.String() +
		`","staked":"` + staked.String() + `"}`)
}

// stakedPart is how much of a payout is delivered as stake. Zero unless the pool
// configured a stake contract.
//
// Rounding goes to the LIQUID side by floor division. Both directions are
// defensible; this one is chosen because the liquid part is what the claimant can
// act on, and a dust unit locked into a cooldown is worth less to them than a dust
// unit in hand.
func stakedPart(payout *big.Int) *big.Int {
	if !present("cfg_stakeContract") {
		return new(big.Int)
	}
	bps := getU("cfg_stakedBps")
	if bps == 0 {
		return new(big.Int)
	}
	out := new(big.Int).Mul(payout, big.NewInt(int64(bps)))
	return out.Div(out, big.NewInt(10000))
}

// creditStake hands `amt` to the staking contract as stake held FOR acct.
//
// Approve-then-stakeFor rather than a plain transfer: a transfer would move the
// tokens and leave the staking contract with no idea whose they are. stakeFor PULLS
// exactly what it was told about, so the amount credited and the amount moved are
// the same number by construction rather than by trust — and the allowance granted
// is exactly this payout, never standing.
//
// This contract must appear in the staking contract's stakeFor allowlist, which is
// fixed at ITS init. That is the authorisation: a contract caller carries no key and
// so can never satisfy the active-authority check.
func creditStake(acct string, amt *big.Int) {
	sc := getStr("cfg_stakeContract")
	adapter.Approve(asset(), "contract:"+sc, amt)
	sdk.ContractCall(sc, "stakeFor",
		`{"acct":"`+acct+`","amount":"`+amt.String()+`"}`, nil)
}

// ---- queries -------------------------------------------------------------

//go:wasmexport shareOf
func ShareOf(payload *string) *string {
	assertInit()
	ch := mustChannel(payload)
	ep := mustEpoch(payload)
	return str(`{"share":"` + getBig("share|"+ch+"|"+ep+"|"+f(payload, "account")).String() +
		`","totalShares":"` + getBig("totalShares|"+ch+"|"+ep).String() +
		`","funded":"` + getBig("funded|"+ch+"|"+ep).String() +
		`","status":"` + statusOf(ch, ep) + `"}`)
}

// ---- helpers -------------------------------------------------------------

func assertInit() {
	if !present(kInit) {
		sdk.Abort("not initialized")
	}
}
func present(k string) bool { v := sdk.StateGetObject(k); return v != nil && *v != "" }

// canon validates a canonical decimal (non-empty, digits only, no leading zeros)
// so "5"/"05"/"005" can't map to distinct keys while C2 normalizes numerically (H1/M1).
func canon(s, name string) string {
	if s == "" {
		sdk.Abort(name + " required")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			sdk.Abort(name + " must be numeric")
		}
	}
	if len(s) > 1 && s[0] == '0' {
		sdk.Abort(name + " must be canonical (no leading zeros)")
	}
	if len(s) > 19 {
		sdk.Abort(name + " out of range") // keep within uint64 (MED-1 class)
	}
	return s
}

func statusOf(ch, ep string) string { return getStr("status|" + ch + "|" + ep) }

// epochEnd: last block of the given epoch, from the funder's schedule.
//
// `ep` is caller-supplied and canon() only bounds it to 19 digits, so g + (n+1)*el can
// wrap uint64. A wrapped end is SMALL, which would make cancelEpoch's stale-rescue
// anchor tiny and open the guardian's rescue window immediately for an epoch that does
// not exist. Nothing can reach it — the rescue path requires funded > 0 first, and
// funding only exists for epochs C2 distributed — but that is a precondition
// elsewhere, not a check here, and C1 carries the same guard in epochBounds.
func epochEnd(ep string) uint64 {
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	n, _ := strconv.ParseUint(ep, 10, 64)
	if el == 0 {
		sdk.Abort("no schedule adopted")
	}
	if n+1 < n || n+1 > (^uint64(0)-g)/el {
		sdk.Abort("epoch out of range")
	}
	return g + (n+1)*el - 1
}

// staleBlocks: grace after the EPOCH ENDS before a guardian may rescue an
// unfinalized epoch — at least 2 full epochs, and at least 10x the challenge
// window, so a reporter working a live epoch can never be front-run (HIGH-1).
func staleBlocks(ch string) uint64 {
	n := getU("ch_window|"+ch) * 10
	if el2 := getU("cfg_epochLen") * 2; n < el2 {
		n = el2
	}
	if n < 1000 {
		n = 1000
	}
	return n
}

// mustEpoch reads and validates the epoch as a bare uint string (numeric only),
// so it can't inject '|'/':' into state keys (MED-1). Kept as string for keys.
func mustEpoch(payload *string) string { return canon(f(payload, "epoch"), "epoch") }

func asset() adapter.Asset {
	k := adapter.Fungible
	if getStr("cfg_kind") == "1" {
		k = adapter.EditionedNFT
	}
	return adapter.Asset{Kind: k, Contract: getStr("cfg_token"), TokenId: getStr("cfg_tokenId")}
}
func reporterCfg(ch string) auth.Config {
	return authCfg("ch_rMode|"+ch, "ch_rAuth|"+ch, "ch_rThr|"+ch)
}
func guardianCfg() auth.Config { return authCfg("cfg_gMode", "cfg_gAuth", "cfg_gThr") }
func authCfg(mk, ak, tk string) auth.Config {
	var mode auth.Mode
	switch getStr(mk) {
	case "0":
		mode = auth.ModeSingle
	case "1":
		mode = auth.ModeCosigned
	case "2":
		mode = auth.ModeAttest
	default:
		sdk.Abort("auth: unknown mode") // MED-1: no silent downgrade to Single
	}
	thr, terr := strconv.Atoi(getStr(tk))
	if terr != nil || thr < 1 {
		sdk.Abort("auth threshold must be a positive integer") // HIGH-1: no silent 1-of-N
	}
	return auth.Config{Mode: mode, Authorities: splitComma(getStr(ak)), Threshold: thr}
}
func reqAuths() []string {
	env := sdk.GetEnv()
	out := []string{}
	for _, a := range env.Sender.RequiredAuths {
		out = append(out, a.String())
	}
	return out
}
func mustCaller() string {
	c := sdk.GetEnvKey("msg.caller")
	if c == nil {
		sdk.Abort("no caller")
	}
	return *c
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
func getU(k string) uint64 {
	n, _ := strconv.ParseUint(getStr(k), 10, 64)
	return n
}
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

// validateLedgerAddr requires a known ledger domain. Used only for values that are
// TRANSFER DESTINATIONS (not contract ids, which are bare vsc1... strings).
// isLedgerAddr reports whether a has a known ledger domain.
func isLedgerAddr(a string) bool {
	return hasPrefix(a, "hive:") || hasPrefix(a, "contract:") ||
		hasPrefix(a, "did:") || hasPrefix(a, "system:")
}

func validateLedgerAddr(a string) {
	if !hasPrefix(a, "hive:") && !hasPrefix(a, "contract:") && !hasPrefix(a, "did:") && !hasPrefix(a, "system:") {
		sdk.Abort("ledger address must start with hive:/contract:/did:/system:")
	}
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

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func ok() *string          { return str(`{"success":true}`) }
func str(s string) *string { return &s }

// split2 splits "acct:shares" on the LAST colon — account ids themselves contain
// a colon (hive:alice), and shares is the trailing numeric field (HIGH-1).
func split2(s string) (string, string) {
	i := -1
	for k := len(s) - 1; k >= 0; k-- {
		if s[k] == ':' {
			i = k
			break
		}
	}
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
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
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// f extracts a flat JSON field. It only matches "name" in KEY position (followed
// by ':'), so a value equal to a field name can't be mis-parsed (M4).
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
		from = i // next search resumes past this occurrence
		k := i
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			continue // not a key here
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
func pickField(res *string, name string) string {
	if res == nil {
		return ""
	}
	return f(res, name)
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

// ---- CHANNELS ------------------------------------------------------------
//
// One deployed distributor serves several reward streams — content, LP, whatever a
// tenant adds later — instead of one contract per stream. Every epoch key is scoped
// by channel, so the streams share code and a treasury but nothing else: separate
// funding, separate share books, separate claims, separate reporter authority.
//
// The reporter authority MUST be per channel. A content reporter and an LP reporter
// are different services holding different keys, and one should not be able to submit
// the other's shares.
//
// Auth action keys are channel-scoped for a subtler reason. The auth module records
// one vote per (action, authority) with no payload component, so an action key of
// `fin:5` would let a reporter attesting content epoch 5 burn its single vote for LP
// epoch 5 at the same time — the second channel could then never reach threshold.
//
// Channels are added after init, not in it, because verifying a channel's bucket
// requires calling the funder, and they are append-only: re-pointing a live channel's
// reporter authority or bucket would rewrite the rules under an epoch already in
// flight.

// addChannel: {"channel","bucket","window","reporterMode","reporterAuth",
//
//	"reporterThreshold","role"} — owner-only, once per name.
//
//go:wasmexport addChannel
func AddChannel(payload *string) *string {
	assertInit()
	ownerK := sdk.GetEnvKey("contract.owner")
	if ownerK == nil || mustCaller() != *ownerK {
		sdk.Abort("only owner can add a channel")
	}
	ch := f(payload, "channel")
	if ch == "" {
		sdk.Abort("channel required")
	}
	// Channel names are part of every state key, so they must not contain the
	// separator or they could be made to collide with a different channel's keys.
	for i := 0; i < len(ch); i++ {
		if ch[i] == '|' || ch[i] == ':' || ch[i] == ',' {
			sdk.Abort("channel name must not contain | : or ,")
		}
	}
	if present("ch_bucket|" + ch) {
		sdk.Abort("channel already exists — channels are append-only")
	}

	// The bucket that funds this channel, verified against the funder now: a name C2
	// does not know, or one paying a different contract, would otherwise surface only
	// at the first pullFunding, after the epoch it should have funded had elapsed.
	bucket := f(payload, "bucket")
	if bucket == "" {
		sdk.Abort("bucket required — the funder bucket that pays this channel")
	}
	bt := pickField(sdk.ContractCall(getStr("cfg_funder"), "bucketTarget",
		`{"bucket":"`+bucket+`"}`, nil), "target")
	if bt == "" {
		sdk.Abort("the funder has no bucket by that name")
	}
	if bt != selfAddr() {
		sdk.Abort("the funder's bucket of that name pays a different contract")
	}
	// Two channels drawing one bucket would split a single allocation unpredictably
	// between two share books.
	if present("ch_forbucket|" + bucket) {
		sdk.Abort("that bucket already funds another channel")
	}

	w := f(payload, "window")
	wn, werr := strconv.ParseUint(w, 10, 64)
	if werr != nil || wn == 0 {
		sdk.Abort("window must be a uint > 0")
	}
	// An immutable window longer than ten epochs is a typo, and it would hold every
	// claim shut for that long with no way to correct it.
	if el := getU("cfg_epochLen"); el > 0 && wn > 10*el {
		sdk.Abort("window must be <= 10 * epochLen (an immutable window that long is a typo)")
	}

	set("ch_rMode|"+ch, f(payload, "reporterMode"))
	set("ch_rAuth|"+ch, f(payload, "reporterAuth"))
	set("ch_rThr|"+ch, f(payload, "reporterThreshold"))
	auth.Validate(reporterCfg(ch))
	// The reporter and the guardian must stay disjoint per channel, or one coalition
	// could finalize a fraudulent report AND refuse to cancel it.
	for _, r := range splitComma(f(payload, "reporterAuth")) {
		for _, g := range splitComma(getStr("cfg_gAuth")) {
			if r != "" && r == g {
				sdk.Abort("reporter and guardian authorities must be disjoint")
			}
		}
	}
	if tre := getStr("cfg_treasury"); tre != "" {
		for _, r := range splitComma(f(payload, "reporterAuth")) {
			if r != "" && r == tre {
				sdk.Abort("treasury must not be a reporter authority")
			}
		}
	}
	if role := f(payload, "role"); role != "" {
		if role != "content" && role != "lp" {
			sdk.Abort("role must be \"content\" or \"lp\" if set")
		}
		set("ch_role|"+ch, role)
	}
	set("ch_window|"+ch, w)
	set("ch_forbucket|"+bucket, ch)
	set("ch_bucket|"+ch, bucket) // written LAST: it is what mustChannel tests
	events.New("channel").
		Str("channel", ch).
		Str("bucket", bucket).
		Str("window", w).
		Str("role", getStr("ch_role|"+ch)).
		Str("reporter_mode", f(payload, "reporterMode")).
		Str("reporter_auth", f(payload, "reporterAuth")).
		Str("reporter_threshold", f(payload, "reporterThreshold")).
		Emit()
	return ok()
}

// mustChannel resolves and proves the channel exists. Requiring it to exist is what
// stops a typo'd name quietly opening a fresh, unfunded namespace whose epochs would
// look empty rather than wrong.
func mustChannel(payload *string) string {
	ch := f(payload, "channel")
	if ch == "" {
		sdk.Abort("channel required")
	}
	if !present("ch_bucket|" + ch) {
		sdk.Abort("no such channel — add it with addChannel first")
	}
	return ch
}

// channelInfo: {"channel"} — what a reporter cross-checks itself against.
//
//go:wasmexport channelInfo
func ChannelInfo(payload *string) *string {
	assertInit()
	ch := f(payload, "channel")
	return str(`{"bucket":"` + getStr("ch_bucket|"+ch) +
		`","window":"` + getStr("ch_window|"+ch) +
		`","role":"` + getStr("ch_role|"+ch) +
		`","genesis":"` + getStr("cfg_genesis") +
		`","epochLen":"` + getStr("cfg_epochLen") + `"}`)
}
