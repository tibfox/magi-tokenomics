// C6 — Migration/Bootstrap (plan §19.2, R19 option a). Owner-only paginated airdrop
// of an initial holder snapshot. C6 holds a bootstrap balance (funded at deploy —
// e.g. C2 mints to itself then transfers to C6) and distributes it out. Per-batch
// idempotency guard prevents double-run.
package main

import (
	"magi_token/adapter"
	"magi_token/auth"
	"magi_token/sdk"
	"math/big"
)

func main() {}

const kInit = "init"

// Init: {"token","kind","tokenId"}
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
	set("cfg_token", f(payload, "token"))
	// FAIL CLOSED on the editioned-NFT value asset: kind must be "0". See the
	// adapter package doc — NFT mode's R4/R16 prerequisites are not implemented,
	// so a kind="1" deployment must be impossible, not merely broken later.
	set("cfg_kind", adapter.RequireFungible(f(payload, "kind")))
	adapter.RequireNoTokenId(f(payload, "tokenId"))
	set("cfg_tokenId", "")
	set("cfg_owner", *owner)
	// Hard cap on total airdroppable amount — bounds a post-deploy owner-key
	// compromise from draining the whole bootstrap balance (HIGH-3).
	cap := f(payload, "maxAirdrop")
	if parseBig(cap).Sign() <= 0 {
		sdk.Abort("maxAirdrop required (>0)")
	}
	set("cfg_maxAirdrop", cap)
	setBig("airdrop_total", new(big.Int))
	// OPTIONAL residual treasury, pinned here and immutable.
	//
	// C6 is funded by transferring a bootstrap balance to it, and value can otherwise
	// leave ONLY through airdropBatch, bounded by maxAirdrop. So every token above the
	// cap, every token under it that the snapshot turns out not to need, and every
	// entry the ledger-address filter skips is LOCKED IN THIS CONTRACT FOREVER. C3/C5
	// have sweepUnallocated and C7 has sweepResidual; C6, which holds the largest
	// single balance at launch, had no exit at all.
	//
	// Left optional on purpose. A sweep is also a clawback capability, and a launch
	// may deliberately want the stronger promise that nothing can ever be reclaimed —
	// omitting treasury keeps exactly that guarantee, at the cost of locking any
	// excess. sweepResidual can never touch tokens a pending airdrop could still need.
	if tre := f(payload, "treasury"); tre != "" {
		validateLedgerAddr(tre)
		if tre == selfAddr() {
			sdk.Abort("treasury cannot be C6 itself")
		}
		set("cfg_treasury", tre)
	}
	set(kInit, "1")
	return ok()
}

// airdropBatch — owner-only, idempotent per batchId. Transfers to each holder from
// C6's own balance. {"batchId","entries":"acct:amount,acct:amount"}
//
//go:wasmexport airdropBatch
func AirdropBatch(payload *string) *string {
	assertInit()
	c := mustCaller()
	if c != getStr("cfg_owner") {
		sdk.Abort("only owner")
	}
	auth.RequireActive(c, reqAuths()) // CRIT-2: posting key must not move funds
	batch := f(payload, "batchId")
	if batch == "" {
		sdk.Abort("batchId required")
	}
	validateAddr(batch)
	bk := "done|" + batch
	if present(bk) {
		sdk.Abort("batch already applied")
	}
	set(bk, "1") // mark before transfers (idempotent; whole tx reverts on any failure)
	total := getBig("airdrop_total")
	for _, e := range splitComma(f(payload, "entries")) {
		acct, amtStr := split2(e)
		if acct == "" {
			continue
		}
		validateAddr(acct)
		// Airdrop recipients are transfer destinations: a bare "alice" would credit
		// a balance no key can ever spend. Skipped like any other malformed entry.
		if !isLedgerAddr(acct) {
			continue
		}
		amt := parseBig(amtStr)
		if amt.Sign() <= 0 {
			continue
		}
		adapter.Transfer(asset(), acct, amt)
		total.Add(total, amt)
	}
	if total.Cmp(parseBig(getStr("cfg_maxAirdrop"))) > 0 {
		sdk.Abort("exceeds maxAirdrop cap") // HIGH-3
	}
	setBig("airdrop_total", total)
	return str(`{"airdropped_total":"` + total.String() + `"}`)
}

//go:wasmexport airdropTotal
func AirdropTotal(_ *string) *string {
	assertInit()
	return str(`{"total":"` + getBig("airdrop_total").String() + `"}`)
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
func present(k string) bool { v := sdk.StateGetObject(k); return v != nil && *v != "" }
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

// isLedgerAddr reports whether a has a known ledger domain.
func isLedgerAddr(a string) bool {
	return hasPrefix(a, "hive:") || hasPrefix(a, "contract:") ||
		hasPrefix(a, "did:") || hasPrefix(a, "system:")
}

// validateLedgerAddr requires a known ledger domain. Used for values that are
// TRANSFER DESTINATIONS (not contract ids, which are bare vsc1... strings).
func validateLedgerAddr(a string) {
	validateAddr(a)
	if !hasPrefix(a, "hive:") && !hasPrefix(a, "contract:") && !hasPrefix(a, "did:") && !hasPrefix(a, "system:") {
		sdk.Abort("ledger address must start with hive:/contract:/did:/system:")
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
func ok() *string          { return str(`{"success":true}`) }
func str(s string) *string { return &s }

// split2 splits "acct:amount" on the LAST colon (acct contains a colon).
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

// sweepResidual — owner-only, moves ONLY the provably un-airdroppable excess to the
// treasury pinned at init.
//
// The amount is balance - (maxAirdrop - airdropped), i.e. what remains after
// reserving every token the remaining airdrop capacity could still pay out. It can
// therefore never take funds a pending batch needs, which is what separates this from
// a clawback. Without it, that excess is locked in the contract permanently.
//
//go:wasmexport sweepResidual
func SweepResidual(_ *string) *string {
	assertInit()
	tre := getStr("cfg_treasury")
	if tre == "" {
		sdk.Abort("no treasury was pinned at init — this deployment has no residual exit by design")
	}
	c := mustCaller()
	if c != getStr("cfg_owner") {
		sdk.Abort("only owner may sweep the residual")
	}
	// Fund-moving, so an ACTIVE authority is required — a posting key must not be
	// able to move the bootstrap balance (CRIT-2), exactly as airdropBatch requires.
	//
	// STRICT RequireActive here, not the contract-exempting variant C1 uses. That
	// difference is deliberate: C1 guards a CALLER-RELATIVE position that a contract
	// can legitimately hold (a DAO or vault can stake), and an unconditional check
	// would strand its stake forever. cfg_owner is a DEPLOYER, and a deploy is an L1
	// transaction — owners are hive: or did:pkh: identities, both of which appear in
	// required_auths and satisfy the strict check. A contract cannot be an owner, so
	// there is nothing to strand.
	auth.RequireActive(c, reqAuths())

	// airdrop_total counts only entries that were actually PAID: airdropBatch
	// increments it after the transfer, so entries skipped by the ledger-address
	// filter never inflate it. That is what makes `reserved` the true remaining
	// capacity rather than an overstatement — if skipped entries counted, reserved
	// would shrink and this would sweep tokens a later batch still needs.
	bal := adapter.BalanceOfSelf(asset())
	reserved := new(big.Int).Sub(parseBig(getStr("cfg_maxAirdrop")), getBig("airdrop_total"))
	if reserved.Sign() < 0 {
		reserved = new(big.Int)
	}
	residual := new(big.Int).Sub(bal, reserved)
	if residual.Sign() <= 0 {
		sdk.Abort("no residual: the balance is still reserved for the remaining airdrop capacity")
	}
	adapter.Transfer(asset(), tre, residual)
	return str(`{"swept":"` + residual.String() + `","to":"` + tre + `"}`)
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
