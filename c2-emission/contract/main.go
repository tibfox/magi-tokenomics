// C2 — Emission Controller (plan §19.2, §21/F1 pull model). Becomes the value
// asset's owner and mints a flat per-epoch emission into named buckets. distributeEpoch
// only mints-to-self + records per-epoch bucket allocations (NO external calls to
// bucket targets) so a hostile/paused target cannot freeze emission (H-1). Each
// target PULLS its slice via claimBucket. Guardian passthrough (pauseToken/
// changeTokenOwner) is auth-gated (all modes) + timelocked (R6/D6).
package main

import (
	"magi_token/adapter"
	"magi_token/auth"
	"magi_token/sdk"
	"math/big"
	"strconv"
)

func main() {}

const (
	kInit           = "init"
	defaultMaxCatch = 50
)

// ---- init ----------------------------------------------------------------

// Init config (all economic params immutable after init, D2):
// {"token","kind","tokenId","genesis","epochLen","baseAnnual","blocksPerYear","dustBucket",
//
//	"guardianMode","guardianAuth","guardianThreshold","timelock",
//	"buckets":"name:target:weightBps,name:target:weightBps,..."}
//
//go:wasmexport init
func Init(payload *string) *string {
	if present(kInit) {
		sdk.Abort("already initialized")
	}
	owner := sdk.GetEnvKey("contract.owner")
	caller := sdk.GetEnvKey("msg.caller")
	if owner == nil || caller == nil || *owner != *caller {
		sdk.Abort("only contract owner can init")
	}
	tok := f(payload, "token")
	validateAddr(tok)
	set("cfg_token", tok)
	// FAIL CLOSED on the editioned-NFT value asset: kind must be "0". See the
	// adapter package doc — NFT mode's R4/R16 prerequisites are not implemented,
	// so a kind="1" deployment must be impossible, not merely broken later.
	set("cfg_kind", adapter.RequireFungible(f(payload, "kind")))
	adapter.RequireNoTokenId(f(payload, "tokenId"))
	set("cfg_tokenId", "")
	// genesis: omit it to start the schedule at THIS (the init) block. A genesis in
	// the past would make the first poke try to catch up (head-genesis)/epochLen
	// epochs and blow the RC budget, so it is rejected outright.
	hNow := blockHeight()
	gs := f(payload, "genesis")
	gv := hNow
	if gs != "" {
		parsed, gerr := strconv.ParseUint(gs, 10, 64)
		if gerr != nil {
			sdk.Abort("genesis must be a valid uint")
		}
		if parsed < hNow {
			sdk.Abort("genesis must be >= current block height (omit it to start now)")
		}
		gv = parsed
	}
	setU("cfg_genesis", gv)
	el := pu(f(payload, "epochLen"))
	if el == 0 {
		sdk.Abort("epochLen>0")
	}
	setU("cfg_epochLen", el)
	if parseBig(f(payload, "baseAnnual")).Sign() <= 0 {
		sdk.Abort("baseAnnual>0")
	}
	set("cfg_baseAnnual", f(payload, "baseAnnual"))
	by := pu(f(payload, "blocksPerYear"))
	if by == 0 {
		sdk.Abort("blocksPerYear>0")
	}
	setU("cfg_blocksPerYear", by)
	// Emission is baseAnnual*epochLen/blocksPerYear with INTEGER division, so all
	// three being individually valid does not make the result non-zero: e.g.
	// baseAnnual=1000, epochLen=1, blocksPerYear=10512000 truncates to 0.
	//
	// A zero rate does not fail visibly. distributeEpoch would keep advancing epochs
	// and marking them done while funding nothing at all, forever, with every poke
	// reporting success. Reject the configuration instead of shipping a schedule that
	// silently pays nobody. Checked here, after all three inputs are stored, because
	// emissionForEpoch reads them back from state.
	if emissionForEpoch(0).Sign() <= 0 {
		sdk.Abort("emission rounds to zero: baseAnnual*epochLen must be >= blocksPerYear")
	}
	set("cfg_dustBucket", f(payload, "dustBucket"))
	// guardian auth config
	set("cfg_gMode", f(payload, "guardianMode"))
	set("cfg_gAuth", f(payload, "guardianAuth"))
	set("cfg_gThr", f(payload, "guardianThreshold"))
	tl := pu(f(payload, "timelock"))
	if tl == 0 {
		sdk.Abort("timelock must be > 0 blocks")
	}
	setU("cfg_timelock", tl)
	// maxCatch: epochs distributed per poke. Measured cost ~995 RC/epoch (1 bucket)
	// and ~1437 RC/epoch (3 buckets) plus ~280 base, so size it against the rc_limit
	// the keeper will actually use (e.g. 50 epochs x 3 buckets ~ 72k RC). Pokes are
	// permissionless and repeatable, so a lower value simply needs more pokes.
	if mc := f(payload, "maxCatch"); mc != "" {
		mcv := pu(mc)
		if mcv == 0 || mcv > 1000 {
			sdk.Abort("maxCatch must be 1..1000")
		}
		setU("cfg_maxCatch", mcv)
	}
	auth.Validate(guardianCfg()) // reject bad guardian policy up front
	// Separate VETO authority for cancelTokenOp — must be able to abort a queued
	// op even if the guardian set is compromised, so it must be DISJOINT (CRIT-1).
	set("cfg_vMode", f(payload, "vetoMode"))
	set("cfg_vAuth", f(payload, "vetoAuth"))
	set("cfg_vThr", f(payload, "vetoThreshold"))
	auth.Validate(vetoCfg())
	for _, g := range splitComma(f(payload, "guardianAuth")) {
		for _, v := range splitComma(f(payload, "vetoAuth")) {
			if g != "" && g == v {
				sdk.Abort("guardian and veto authorities must be disjoint")
			}
		}
	}

	// buckets
	selfId := "contract:" + *sdk.GetEnvKey("contract.id") // ledger identity (MED-2)
	buckets := splitComma(f(payload, "buckets"))
	total := 0
	n := uint64(0)
	dust := f(payload, "dustBucket")
	dustSeen := false
	for _, b := range buckets {
		if b == "" {
			continue
		}
		name, target, wbps := split3(b)
		if name == "" || target == "" {
			sdk.Abort("bad bucket")
		}
		validateAddr(target)
		// Bucket targets are matched against msg.caller, which is always domain-
		// prefixed; a bare id could never claim and would strand emission (MED).
		if !hasPrefix(target, "hive:") && !hasPrefix(target, "contract:") && !hasPrefix(target, "did:") {
			sdk.Abort("bucket target must start with hive:/contract:/did:")
		}
		if target == selfId {
			sdk.Abort("bucket target cannot be C2 itself (R21)")
		}
		w, err := strconv.Atoi(wbps)
		if err != nil || w < 0 || w > 10000 {
			sdk.Abort("weightBps 0..10000")
		}
		total += w
		if name == dust {
			dustSeen = true
		}
		set("bucket|"+strconv.FormatUint(n, 10), name+":"+target+":"+strconv.Itoa(w))
		n++
	}
	if total != 10000 {
		sdk.Abort("bucket weights must sum to 10000")
	}
	if !dustSeen {
		sdk.Abort("dustBucket must be one of the configured buckets (R14)")
	}
	setU("bucket_n", n)

	// read + store the token's immutable maxSupply (getInfo → {"maxSupply":"..."} )
	info := sdk.ContractCall(tok, "getInfo", "", nil)
	// SOURCE of the emission pool. C2 no longer mints: it pulls each epoch's
	// emission from an account that has approved it (token.transferFrom, the same
	// allowance mechanism C1 uses for staking). Defaults to the deploying owner.
	//
	// This is why C2 does NOT need to own the token. Owning it was the framework's
	// single largest trust concession — the owner contract permanently held mint,
	// pause and changeOwner. Under the allowance model C2 holds no authority over
	// the token at all; it is merely an approved spender, and token ownership can be
	// renounced or handed to a DAO independently.
	src := f(payload, "source")
	if src == "" {
		src = *owner
	}
	validateLedgerAddr(src)
	set("cfg_source", src)
	set("cfg_maxSupply", pickField(info, "maxSupply"))
	if parseBig(getStr("cfg_maxSupply")).Sign() <= 0 {
		sdk.Abort("token maxSupply must be > 0 (getInfo) — M-A")
	}

	// NB: do NOT write cfg_lastEpoch here — its ABSENCE is the "no epoch yet"
	// sentinel so the first poke starts at epoch 0 (H1).
	set(kInit, "1")
	return ok()
}

// ---- emission ------------------------------------------------------------

// distributeEpoch — permissionless keeper poke; idempotent on height.
//
//go:wasmexport distributeEpoch
func DistributeEpoch(_ *string) *string {
	assertInit()
	genesis := getU("cfg_genesis")
	el := getU("cfg_epochLen")
	h := blockHeight()
	if h < genesis+el {
		return str(`{"distributed":0}`) // first epoch not elapsed yet
	}
	current := (h - genesis) / el // number of fully-elapsed epochs
	last, has := lastEpoch()
	next := uint64(0)
	if has {
		next = last + 1
	}
	// Already caught up — return before touching the token so an idle poke costs
	// two ContractCalls less. This is the common case once a keeper is running.
	if next >= current {
		return str(`{"distributed":0}`)
	}
	// How much the source can actually give us: min(what it approved, what it
	// holds). Snapshot ONCE and decrement locally — read-your-writes across
	// ContractCalls within a tx is not reliable (M3).
	//
	// Exhaustion is NOT permanent. The pool is meant to be refilled in batches, so
	// a poke that cannot fund the next epoch reports `starved` and changes no
	// state; a later top-up (mint + increaseAllowance) resumes from exactly here
	// and pays the backlog that accrued meanwhile.
	source := getStr("cfg_source")
	tok := getStr("cfg_token")
	allowed := parseBig(pickField(sdk.ContractCall(tok, "allowance",
		`{"owner":"`+source+`","spender":"`+self()+`"}`, nil), "allowance"))
	held := parseBig(pickField(sdk.ContractCall(tok, "balanceOf",
		`{"account":"`+source+`"}`, nil), "balance"))
	available := allowed
	if held.Cmp(available) < 0 {
		available = held
	}
	done := uint64(0)
	// Fully-elapsed epoch indices are 0..current-1 (H1).
	maxCatch := getU("cfg_maxCatch")
	if maxCatch == 0 {
		maxCatch = defaultMaxCatch
	}
	starved := false
	for ep := next; ep < current && done < maxCatch; ep++ {
		emission := emissionForEpoch(ep)
		// All-or-nothing per epoch. A partially funded epoch would still be marked
		// done and never revisited, so a later top-up could not repair it — and with
		// a pool refilled in batches that boundary recurs at EVERY batch, not just
		// once at the end. Waiting instead means every epoch is eventually paid in
		// full. The cost is that a remainder smaller than one epoch never emits;
		// size the final approve to land on a whole multiple if that matters.
		if available.Cmp(emission) < 0 {
			starved = true
			break
		}
		if emission.Sign() > 0 {
			// transferFrom(source -> C2). Aborts if the source revoked the allowance
			// or moved the tokens between our snapshot and now; the whole poke then
			// reverts and can simply be retried.
			adapter.PullFrom(asset(), source, emission)
			available = new(big.Int).Sub(available, emission)
			recordAllocations(ep, emission)
		}
		setU("cfg_lastEpoch_v", ep)
		set("cfg_lastEpoch", "1")
		done++
	}
	out := `{"distributed":"` + strconv.FormatUint(done, 10) + `"`
	if starved {
		// NOT terminal — a refill resumes from here and pays the backlog.
		out += `,"starved":true`
	}
	return str(out + `}`)
}

// recordAllocations splits `minted` across buckets (remainder→dustBucket) and
// records per-(target,epoch) owed amounts. No external calls (pull model).
func recordAllocations(epoch uint64, minted *big.Int) {
	n := getU("bucket_n")
	dust := getStr("cfg_dustBucket")
	distributed := new(big.Int)
	var dustTarget string
	for i := uint64(0); i < n; i++ {
		name, target, w := split3(getStr("bucket|" + strconv.FormatUint(i, 10)))
		wb, _ := strconv.Atoi(w)
		if name == dust {
			dustTarget = target
		}
		slice := new(big.Int).Mul(minted, big.NewInt(int64(wb)))
		slice.Div(slice, big.NewInt(10000))
		distributed.Add(distributed, slice)
		addOwed(target, epoch, slice)
	}
	// remainder → dust bucket
	rem := new(big.Int).Sub(minted, distributed)
	if rem.Sign() > 0 && dustTarget != "" {
		addOwed(dustTarget, epoch, rem)
	}
}

func addOwed(target string, epoch uint64, amt *big.Int) {
	if amt.Sign() == 0 {
		return
	}
	k := "owed|" + target + "|" + strconv.FormatUint(epoch, 10)
	setBig(k, new(big.Int).Add(getBig(k), amt))
}

// claimBucket: a bucket target pulls its slice for a given epoch.
// {"epoch":"N"} — msg.caller must be the target recorded for some bucket.
//
//go:wasmexport claimBucket
func ClaimBucket(payload *string) *string {
	assertInit()
	c := mustCaller()
	// Canonical + range-checked: pu() returns 0 on overflow, so a >uint64 string
	// used to alias onto epoch 0 and drain it (MED-1).
	eps := f(payload, "epoch")
	if eps == "" || len(eps) > 19 {
		sdk.Abort("epoch required/too long")
	}
	for i := 0; i < len(eps); i++ {
		if eps[i] < '0' || eps[i] > '9' {
			sdk.Abort("epoch must be numeric")
		}
	}
	if len(eps) > 1 && eps[0] == '0' {
		sdk.Abort("epoch must be canonical")
	}
	ep, perr := strconv.ParseUint(eps, 10, 64)
	if perr != nil {
		sdk.Abort("epoch out of range")
	}
	k := "owed|" + c + "|" + strconv.FormatUint(ep, 10)
	amt := getBig(k)
	if amt.Sign() <= 0 {
		sdk.Abort("nothing owed for caller/epoch")
	}
	// CEI: zero the owed record before the external transfer
	sdk.StateDeleteObject(k)
	adapter.Transfer(asset(), c, amt)
	return str(`{"claimed":"` + amt.String() + `"}`)
}

// ---- guardian passthrough (auth + timelock, R6/D6) -----------------------

// queueTokenOp — guardian queues a token op behind the timelock.
// {"op":"pause"|"unpause"|"changeOwner","newOwner":"..."}
//
//go:wasmexport queueTokenOp
func QueueTokenOp(payload *string) *string {
	assertInit()
	opKey := opKeyOf(payload)
	if present("tl|" + opKey) {
		sdk.Abort("op already queued") // HIGH-2: no re-queue to escape a spent veto
	}
	committed := auth.Authorize(guardianCfg(), "guardian", opKey, opKey, mustCaller(), reqAuths())
	if committed {
		seq := getU("qseq|"+opKey) + 1
		setU("qseq|"+opKey, seq) // monotonic: distinct veto key even same-block
		setU("tl|"+opKey, blockHeight()+getU("cfg_timelock"))
	}
	return str(`{"queued":` + boolStr(committed) + `}`)
}

// executeTokenOp — after the timelock elapses, perform the op on the token.
//
//go:wasmexport executeTokenOp
func ExecuteTokenOp(payload *string) *string {
	assertInit()
	opKey := opKeyOf(payload)
	if !present("tl|" + opKey) {
		sdk.Abort("op not queued")
	}
	// HIGH-2: not permissionless — a matured op must be executed by the guardian
	// (or the veto), so "let it expire" is a real option for honest parties.
	c := mustCaller()
	ra := reqAuths()
	if !isAuthority(guardianCfg(), c) && !isAuthority(vetoCfg(), c) {
		sdk.Abort("only guardian or veto may execute")
	}
	auth.RequireActive(c, ra)
	ready := getU("tl|" + opKey)
	if blockHeight() < ready {
		sdk.Abort("timelock not elapsed")
	}
	// HIGH-2: queued ops expire, so a stale approval cannot be fired years later.
	if blockHeight() > ready+getU("cfg_timelock") {
		sdk.StateDeleteObject("tl|" + opKey)
		sdk.Abort("queued op expired")
	}
	sdk.StateDeleteObject("tl|" + opKey)
	op := f(payload, "op")
	tok := getStr("cfg_token")
	switch op {
	case "pause":
		sdk.ContractCall(tok, "pause", "", nil)
	case "unpause":
		sdk.ContractCall(tok, "unpause", "", nil)
	case "changeOwner":
		no := f(payload, "newOwner")
		validateAddr(no)
		sdk.ContractCall(tok, "changeOwner", `{"newOwner":"`+no+`"}`, nil)
	default:
		sdk.Abort("unknown op")
	}
	return ok()
}

// cancelTokenOp — guardian aborts a queued token op during its timelock window,
// making the delay actually usable against a maliciously-queued changeOwner (H1).
// Same {"op","nonce","newOwner"} identifying the queued op.
//
//go:wasmexport cancelTokenOp
func CancelTokenOp(payload *string) *string {
	assertInit()
	opKey := opKeyOf(payload)
	if !present("tl|" + opKey) {
		sdk.Abort("op not queued")
	}
	// HIGH-3: `unpause` restores liveness — a paused token freezes every claim AND
	// stakers' own withdrawals. The veto must not be able to hold funds hostage.
	if f(payload, "op") == "unpause" {
		sdk.Abort("unpause cannot be vetoed")
	}
	// Bind the veto to THIS queue instance (its maturity height), so a re-queued
	// op cannot inherit an already-committed cancellation (HIGH-2).
	ck := "cancelop:" + opKey + ":" + getStr("qseq|"+opKey)
	// VETO authority (not the guardian) — a compromised guardian must not be able
	// to block its own cancellation (CRIT-1).
	if !auth.Authorize(vetoCfg(), "veto", ck, ck, mustCaller(), reqAuths()) {
		return str(`{"cancelled":false}`)
	}
	sdk.StateDeleteObject("tl|" + opKey)
	return ok()
}

// ---- queries -------------------------------------------------------------

//go:wasmexport scheduleInfo
func ScheduleInfo(_ *string) *string {
	assertInit()
	return str(`{"genesis":"` + getStr("cfg_genesis") + `","epochLen":"` + getStr("cfg_epochLen") + `"}`)
}

//go:wasmexport owedOf
func OwedOf(payload *string) *string {
	assertInit()
	return str(`{"owed":"` + getBig("owed|"+f(payload, "target")+"|"+f(payload, "epoch")).String() + `"}`)
}

//go:wasmexport emissionForEpochQ
func EmissionForEpochQ(payload *string) *string {
	assertInit()
	return str(`{"emission":"` + emissionForEpoch(pu(f(payload, "epoch"))).String() + `"}`)
}

// ---- emission math (R13: single big.Int division) ------------------------

// emissionForEpoch is FLAT: every epoch emits the same amount.
//
// A decaying (halving) schedule was removed as out of scope. Emission pauses
// whenever the approved pool cannot cover a whole epoch, and resumes on refill;
// it has no permanent end state. The epoch index is therefore unused, but is
// kept in the signature so a future schedule can be reintroduced without
// touching callers.
func emissionForEpoch(_ uint64) *big.Int {
	el := getU("cfg_epochLen")
	base := parseBig(getStr("cfg_baseAnnual"))
	// emission = baseAnnual * epochLen / blocksPerYear
	em := new(big.Int).Mul(base, new(big.Int).SetUint64(el))
	em.Div(em, new(big.Int).SetUint64(getU("cfg_blocksPerYear")))
	return em
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

// isAuthority reports whether acct is in a policy's authority set.
func isAuthority(cfg auth.Config, acct string) bool {
	for _, a := range cfg.Authorities {
		if a == acct {
			return true
		}
	}
	return false
}

func vetoCfg() auth.Config { return authCfgFrom("cfg_vMode", "cfg_vAuth", "cfg_vThr") }

func guardianCfg() auth.Config { return authCfgFrom("cfg_gMode", "cfg_gAuth", "cfg_gThr") }

func authCfgFrom(mk, ak, tk string) auth.Config {
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
		sdk.Abort("auth threshold must be a positive integer")
	}
	return auth.Config{Mode: mode, Authorities: splitComma(getStr(ak)), Threshold: thr}
}

// opKeyOf builds a unique-per-invocation key. A caller-supplied numeric `nonce`
// makes repeatable ops (pause/unpause) work in Attest mode — without it the
// attest doneKey would permanently disable the op after first use (H-B). All
// M guardians must share the same nonce to tally.
func opKeyOf(payload *string) string {
	op := f(payload, "op")
	if op != "pause" && op != "unpause" && op != "changeOwner" {
		sdk.Abort("unknown op") // LOW-2: reject junk ops at queue time
	}
	nonce := f(payload, "nonce")
	if nonce == "" {
		sdk.Abort("nonce required")
	}
	for i := 0; i < len(nonce); i++ {
		if nonce[i] < '0' || nonce[i] > '9' {
			sdk.Abort("nonce must be numeric")
		}
	}
	if len(nonce) > 1 && nonce[0] == '0' {
		sdk.Abort("nonce must be canonical")
	}
	key := op + ":" + nonce
	if op == "changeOwner" {
		no := f(payload, "newOwner")
		validateAddr(no) // reject '|'/'"'/'\\' at queue time (LOW)
		key = key + ":" + no
	}
	return key
}

func reqAuths() []string {
	env := sdk.GetEnv()
	out := []string{}
	for _, a := range env.Sender.RequiredAuths {
		out = append(out, a.String())
	}
	return out
}

func lastEpoch() (uint64, bool) {
	if !present("cfg_lastEpoch") {
		return 0, false
	}
	return getU("cfg_lastEpoch_v"), true
}

func present(k string) bool { v := sdk.StateGetObject(k); return v != nil && *v != "" }

func self() string { return "contract:" + *sdk.GetEnvKey("contract.id") }

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

func pu(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
func getU(k string) uint64        { return pu(getStr(k)) }
func setU(k string, v uint64)     { set(k, strconv.FormatUint(v, 10)) }
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

// validateLedgerAddr requires a known ledger domain. The emission source is an
// account we call transferFrom against, so a bare id would be unusable.
func validateLedgerAddr(a string) {
	validateAddr(a)
	if !hasPrefix(a, "hive:") && !hasPrefix(a, "contract:") &&
		!hasPrefix(a, "did:") && !hasPrefix(a, "system:") {
		sdk.Abort("source must be a ledger address (hive:/contract:/did:/system:)")
	}
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

func split3(s string) (string, string, string) {
	a := indexByte(s, ':')
	if a < 0 {
		return s, "", ""
	}
	rest := s[a+1:]
	b := lastIndexByte(rest, ':')
	if b < 0 {
		return s[:a], rest, ""
	}
	return s[:a], rest[:b], rest[b+1:]
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
func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// f extracts a flat JSON field. Matches "name" only in KEY position (followed by
// ':') so a value equal to a field name can't be mis-parsed (M4).
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
