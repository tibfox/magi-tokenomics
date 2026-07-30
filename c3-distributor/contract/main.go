// C3 — Author/Curation Distributor (plan §19.2, pull model §21/F1). Pulls its
// per-epoch funding from C2 (contract-authoritative amount, R1), accepts pushed
// per-account shares from the reporter (auth pkg, incl. attest M-of-N), computes
// totalShares ITSELF (R2), and pays via claim with Σclaims≤funded (invariant #1).
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

// Init: {"token","kind","tokenId","funder"(C2 id),"window",
//
//	"reporterMode","reporterAuth","reporterThreshold",
//	"guardianMode","guardianAuth","guardianThreshold"}
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
	set("cfg_window", canon(f(payload, "window"), "window")) // MED-3: must be present+>0
	if getU("cfg_window") == 0 {
		sdk.Abort("window must be > 0")
	}
	set("cfg_rMode", f(payload, "reporterMode"))
	set("cfg_rAuth", f(payload, "reporterAuth"))
	set("cfg_rThr", f(payload, "reporterThreshold"))
	set("cfg_gMode", f(payload, "guardianMode"))
	set("cfg_gAuth", f(payload, "guardianAuth"))
	set("cfg_gThr", f(payload, "guardianThreshold"))
	// Pinned sweep destination — sweepUnallocated can ONLY send here, so a malicious
	// guardian cannot cancel+sweep an epoch to an arbitrary address (H2).
	tre := f(payload, "treasury")
	if tre == "" {
		sdk.Abort("treasury required (cancel/sweep destination)") // MED-1
	}
	validateAddr(tre)
	validateLedgerAddr(tre) // MED: typo-proof the immutable payout destination
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
	auth.Validate(reporterCfg())
	auth.Validate(guardianCfg())
	// reporter and guardian authority sets MUST be disjoint, else one coalition can
	// finalize a fraudulent report AND refuse to cancel it (HIGH-3).
	for _, r := range splitComma(f(payload, "reporterAuth")) {
		for _, g := range splitComma(f(payload, "guardianAuth")) {
			if r != "" && r == g {
				sdk.Abort("reporter and guardian authorities must be disjoint")
			}
		}
	}
	set(kInit, "1")
	return ok()
}

// pullFunding — pull this epoch's slice from C2 (permissionless trigger).
// Records funded[epoch] from the amount actually received (R1).
//
//go:wasmexport pullFunding
func PullFunding(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	if statusOf(ep) != "" {
		sdk.Abort("epoch finalized/cancelled — funding locked") // LOW: no late growth
	}
	res := sdk.ContractCall(getStr("cfg_funder"), "claimBucket", `{"epoch":"`+ep+`"}`, nil)
	got := parseBig(pickField(res, "claimed"))
	k := "funded|" + ep
	setBig(k, new(big.Int).Add(getBig(k), got))
	if !present("fundedAt|" + ep) {
		set("fundedAt|"+ep, strconv.FormatUint(blockHeight(), 10)) // MED-2 staleness clock
	}
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
	ep := mustEpoch(payload)
	if statusOf(ep) != "" {
		sdk.Abort("epoch not open")
	}
	entries := f(payload, "entries")
	page := canon(f(payload, "page"), "page") // canonical → no "0"/"00" idempotency bypass (M1)
	actionKey := "ss:" + ep + ":" + page
	committed := auth.Authorize(reporterCfg(), "rep", actionKey, entries, mustCaller(), reqAuths())
	if committed {
		// Apply each (epoch,page) exactly once across ALL auth modes (H2/LOW-1).
		ak := "ssdone|" + ep + "|" + page
		if present(ak) {
			sdk.Abort("page already applied")
		}
		set(ak, "1")
		applyEntries(ep, entries)
	}
	return str(`{"applied":` + boolStr(committed) + `}`)
}

func applyEntries(ep, entries string) {
	total := getBig("totalShares|" + ep)
	for _, e := range splitComma(entries) {
		acct, sh := split2(e)
		if acct == "" {
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
			continue
		}
		s := parseBig(sh)
		if s.Sign() <= 0 {
			continue
		}
		sk := "share|" + ep + "|" + acct
		setBig(sk, new(big.Int).Add(getBig(sk), s))
		total.Add(total, s)
	}
	setBig("totalShares|"+ep, total)
}

// finalizeEpoch — reporter freezes the epoch and opens the challenge window.
//
//go:wasmexport finalizeEpoch
func FinalizeEpoch(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	if statusOf(ep) != "" {
		sdk.Abort("already finalized/cancelled")
	}
	// Must be funded before finalize, else pullFunding is locked out afterwards and
	// the epoch's C2 allocation is stranded forever (⓸).
	if getBig("funded|"+ep).Sign() <= 0 {
		sdk.Abort("epoch not funded — pull funding first")
	}
	// An empty report would freeze the epoch (every claim aborts "no shares"),
	// so reject finalizing with nothing to distribute (MED-5).
	if getBig("totalShares|"+ep).Sign() <= 0 {
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
	if !auth.Authorize(reporterCfg(), "rep", "fin:"+ep, "fin:"+ep, mustCaller(), reqAuths()) {
		return str(`{"finalized":false}`) // pending more attestations
	}
	set("status|"+ep, "finalized")
	set("chal|"+ep, strconv.FormatUint(blockHeight()+getU("cfg_window"), 10))
	return ok()
}

// cancelEpoch — guardian veto during the challenge window (rolls funds forward).
//
//go:wasmexport cancelEpoch
func CancelEpoch(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	// Either: finalized and still inside the challenge window (the veto), OR the
	// epoch was funded but never finalized for > staleness (rescue, MED-2) — else
	// a silent reporter would strand the funding forever.
	st := statusOf(ep)
	if st == "finalized" {
		if blockHeight() >= getU("chal|"+ep) {
			sdk.Abort("challenge window elapsed")
		}
	} else if st == "" {
		// Rescue only an epoch that actually HOLDS money here.
		if getBig("funded|"+ep).Sign() <= 0 {
			sdk.Abort("epoch not funded")
		}
		// Anchor on the LATER of the schedule and the block the funding actually
		// arrived — the same rule as C7.deadlineOf.
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
		if fa := getU("fundedAt|" + ep); fa > anchor {
			anchor = fa
		}
		if blockHeight() < anchor+staleBlocks() {
			sdk.Abort("epoch not finalized (and not stale yet)")
		}
	} else {
		sdk.Abort("epoch already cancelled")
	}
	// Gate on the auth result (CRIT-1: Attest-mode guardian needs threshold).
	if !auth.Authorize(guardianCfg(), "grd", "cancel:"+ep, "cancel:"+ep, mustCaller(), reqAuths()) {
		return str(`{"cancelled":false}`)
	}
	set("status|"+ep, "cancelled")
	// Roll the pulled funding forward into the unallocated pool (M-B/R11) so it
	// is not stranded — recoverable via sweepUnallocated.
	fk := "funded|" + ep
	if amt := getBig(fk); amt.Sign() > 0 {
		setBig("unallocated", new(big.Int).Add(getBig("unallocated"), amt))
		setBig(fk, new(big.Int))
	}
	return ok()
}

// sweepUnallocated — guardian moves the rolled-forward (cancelled-epoch) pool to
// a target. {"to","nonce"} — nonce (numeric) makes it repeatable in Attest mode.
//
//go:wasmexport sweepUnallocated
func SweepUnallocated(payload *string) *string {
	assertInit()
	to := getStr("cfg_treasury") // pinned at init — NOT caller-chosen (H2)
	if to == "" {
		sdk.Abort("no treasury configured")
	}
	nonce := canon(f(payload, "nonce"), "nonce")
	amt := getBig("unallocated")
	// Bind the attestation to the AMOUNT — otherwise a co-signer approving a sweep
	// of 0 could be reused weeks later to move a large balance (MED). Authorize
	// BEFORE the emptiness check so a replayed nonce still reports "already
	// committed" rather than masking it behind an empty pool.
	ak := "sweep:" + nonce
	if !auth.Authorize(guardianCfg(), "grd", ak, amt.String(), mustCaller(), reqAuths()) {
		return str(`{"swept":false}`)
	}
	if amt.Sign() <= 0 {
		sdk.Abort("nothing to sweep")
	}
	setBig("unallocated", new(big.Int)) // CEI before transfer
	adapter.Transfer(asset(), to, amt)
	return str(`{"swept":"` + amt.String() + `"}`)
}

// claim — self-serve payout after finalize + challenge window.
// {"epoch"} — pays funded[ep]*share[ep][caller]/totalShares[ep]. CEI + conservation.
//
//go:wasmexport claim
func Claim(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	if statusOf(ep) != "finalized" {
		sdk.Abort("epoch not finalized")
	}
	if blockHeight() < getU("chal|"+ep) {
		sdk.Abort("challenge window not elapsed")
	}
	c := mustCaller()
	ck := "claimed|" + ep + "|" + c
	if present(ck) {
		sdk.Abort("already claimed")
	}
	funded := getBig("funded|" + ep)
	if funded.Sign() <= 0 {
		sdk.Abort("epoch not funded yet") // don't burn the claimed flag (M5)
	}
	ts := getBig("totalShares|" + ep)
	if ts.Sign() <= 0 {
		sdk.Abort("no shares")
	}
	share := getBig("share|" + ep + "|" + c)
	if share.Sign() <= 0 {
		sdk.Abort("no share for caller")
	}
	payout := new(big.Int).Mul(funded, share)
	payout.Div(payout, ts)
	if payout.Sign() <= 0 {
		sdk.Abort("payout rounds to zero")
	}
	// CEI: mark claimed before external transfer
	set(ck, "1")
	adapter.Transfer(asset(), c, payout)
	return str(`{"claimed":"` + payout.String() + `"}`)
}

// ---- queries -------------------------------------------------------------

//go:wasmexport shareOf
func ShareOf(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	return str(`{"share":"` + getBig("share|"+ep+"|"+f(payload, "account")).String() +
		`","totalShares":"` + getBig("totalShares|"+ep).String() +
		`","funded":"` + getBig("funded|"+ep).String() +
		`","status":"` + statusOf(ep) + `"}`)
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

func statusOf(ep string) string { return getStr("status|" + ep) }

// epochEnd: last block of the given epoch, from the funder's schedule.
func epochEnd(ep string) uint64 {
	g, el := getU("cfg_genesis"), getU("cfg_epochLen")
	n, _ := strconv.ParseUint(ep, 10, 64)
	return g + (n+1)*el - 1
}

// staleBlocks: grace after the EPOCH ENDS before a guardian may rescue an
// unfinalized epoch — at least 2 full epochs, and at least 10x the challenge
// window, so a reporter working a live epoch can never be front-run (HIGH-1).
func staleBlocks() uint64 {
	n := getU("cfg_window") * 10
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
func reporterCfg() auth.Config { return authCfg("cfg_rMode", "cfg_rAuth", "cfg_rThr") }
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
