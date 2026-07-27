// C1 — Dedicated Staking (plan §19.2). Stakes the value asset (fungible token in
// v1) via allowance, tracks per-account stake + a height-checkpointed running
// total so that stakeAtHeight/totalStakedAtHeight are consistent by construction
// (invariant #3). Single-tenant (D3). stakeFor conserves Σstake==custody (R7).
//
// Amounts are stored as decimal strings. Checkpoint/history entries are appended
// in block order, so heights are non-decreasing by index → binary-searchable.
package main

import (
	"magi_token/adapter"
	"magi_token/auth"
	"magi_token/sdk"
	"math/big"
	"strconv"
)

func main() {}

// ---- config / state keys -------------------------------------------------

const (
	kInit       = "init"
	kToken      = "cfg_token"
	kKind       = "cfg_kind"
	kTokenId    = "cfg_tokenid"
	kCooldown   = "cfg_cooldown"
	kTotal      = "total_staked"
	kCkptN      = "ckpt_n"
	maxClaim    = 20 // bound RC per claimUnstaked call
)

func assertInit() {
	if !present(kInit) {
		sdk.Abort("not initialized")
	}
}

func asset() adapter.Asset {
	k := adapter.Fungible
	if v := sdk.StateGetObject(kKind); v != nil && *v == "1" {
		k = adapter.EditionedNFT
	}
	return adapter.Asset{Kind: k, Contract: getStr(kToken), TokenId: getStr(kTokenId)}
}

func cooldown() uint64 {
	n, _ := strconv.ParseUint(getStr(kCooldown), 10, 64)
	return n
}

// ---- exports -------------------------------------------------------------

// Init: {"token","kind","tokenId","cooldown","allow"} — allow is a comma-separated
// list of accounts permitted to call stakeFor. All immutable after init (D2/R7).
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
	tok := field(payload, "token")
	if tok == "" {
		sdk.Abort("token required")
	}
	validateAddr(tok)
	sdk.StateSetObject(kToken, tok)
	// FAIL CLOSED on the editioned-NFT value asset: kind must be "0". See the
	// adapter package doc — NFT mode's R4/R16 prerequisites are not implemented,
	// so a kind="1" deployment must be impossible, not merely broken later.
	sdk.StateSetObject(kKind, adapter.RequireFungible(field(payload, "kind")))
	adapter.RequireNoTokenId(field(payload, "tokenId"))
	sdk.StateSetObject(kTokenId, "")
	cd := field(payload, "cooldown")
	cdN, err := strconv.ParseUint(cd, 10, 64)
	if err != nil {
		sdk.Abort("cooldown must be uint")
	}
	// R15: cooldown must exceed the emission epoch length so a one-block stake
	// can't capture a full epoch of C7 yield. epochLen is supplied at init.
	elN, err2 := strconv.ParseUint(field(payload, "epochLen"), 10, 64)
	if err2 != nil {
		sdk.Abort("epochLen must be uint")
	}
	if cdN <= elN {
		sdk.Abort("cooldown must be > epochLen (R15)")
	}
	sdk.StateSetObject(kCooldown, cd)
	// stakeFor allowlist (immutable)
	for _, a := range splitComma(field(payload, "allow")) {
		if a != "" {
			validateAddr(a)
			sdk.StateSetObject("allow|"+a, "1")
		}
	}
	setBig(kTotal, new(big.Int))
	sdk.StateSetObject(kCkptN, "0")
	sdk.StateSetObject(kInit, "1")
	return ok()
}

// stake: {"amount"} — caller must have approved this contract on the token.
//
//go:wasmexport stake
func Stake(payload *string) *string {
	assertInit()
	c := mustCaller()
	amt := mustAmount(payload)
	credit(c, amt) // pulls into custody, then credits + checkpoints
	return ok()
}

// stakeFor: {"acct","amount"} — allowlisted caller stakes on acct's behalf; tokens
// are pulled from the caller (conserves Σstake==custody, R7).
//
//go:wasmexport stakeFor
func StakeFor(payload *string) *string {
	assertInit()
	c := mustCaller()
	if !present("allow|"+c) {
		sdk.Abort("caller not in stakeFor allowlist")
	}
	auth.RequireActive(c, reqAuths()) // CRIT-2
	acct := field(payload, "acct")
	if acct == "" {
		sdk.Abort("acct required")
	}
	validateAddr(acct)
	amt := mustAmount(payload)
	creditFor(c, acct, amt)
	return ok()
}

// unstake: {"amount"} — removes from stake immediately (weight drops now), queues
// for cooldown withdrawal.
//
//go:wasmexport unstake
func Unstake(payload *string) *string {
	assertInit()
	c := mustCaller()
	amt := mustAmount(payload)
	cur := getBig("stake|" + c)
	if cur.Cmp(amt) < 0 {
		sdk.Abort("insufficient stake")
	}
	newStake := new(big.Int).Sub(cur, amt)
	setBig("stake|"+c, newStake)
	total := new(big.Int).Sub(getBig(kTotal), amt)
	setBig(kTotal, total)
	h := blockHeight()
	appendHist(c, h, newStake)
	appendCkpt(h, total)
	// queue withdrawal
	tail := idx("us_tail|" + c)
	sdk.StateSetObject("us|"+c+"|"+strconv.FormatUint(tail, 10),
		amt.String()+":"+strconv.FormatUint(h+cooldown(), 10))
	sdk.StateSetObject("us_tail|"+c, strconv.FormatUint(tail+1, 10))
	return ok()
}

// claimUnstaked: withdraw matured unstake entries (FIFO, bounded per call).
//
//go:wasmexport claimUnstaked
func ClaimUnstaked(_ *string) *string {
	assertInit()
	c := mustCaller()
	h := blockHeight()
	head := idx("us_head|" + c)
	tail := idx("us_tail|" + c)
	paid := new(big.Int)
	n := 0
	for head < tail && n < maxClaim {
		key := "us|" + c + "|" + strconv.FormatUint(head, 10)
		v := sdk.StateGetObject(key)
		if v == nil || *v == "" {
			head++
			continue
		}
		amtStr, readyStr := splitColon(*v)
		ready, _ := strconv.ParseUint(readyStr, 10, 64)
		if ready > h {
			break // FIFO: earliest not matured ⇒ none after it are either
		}
		amt := parseBig(amtStr)
		// CEI: advance head + delete entry BEFORE the external transfer (R21)
		sdk.StateDeleteObject(key)
		head++
		n++
		adapter.Transfer(asset(), c, amt)
		paid.Add(paid, amt)
	}
	sdk.StateSetObject("us_head|"+c, strconv.FormatUint(head, 10))
	return str(`{"claimed":"` + paid.String() + `"}`)
}

// ---- queries -------------------------------------------------------------

//go:wasmexport stakeOf
func StakeOf(payload *string) *string {
	assertInit()
	return str(`{"stake":"` + getBig("stake|"+field(payload, "account")).String() + `"}`)
}

//go:wasmexport totalStaked
func TotalStaked(_ *string) *string {
	assertInit()
	return str(`{"total":"` + getBig(kTotal).String() + `"}`)
}

//go:wasmexport stakeAtHeight
func StakeAtHeight(payload *string) *string {
	assertInit()
	acct := field(payload, "account")
	h := mustHeight(payload)
	return str(`{"stake":"` + histAt(acct, h).String() + `"}`)
}

//go:wasmexport totalStakedAtHeight
func TotalStakedAtHeight(payload *string) *string {
	assertInit()
	h := mustHeight(payload)
	return str(`{"total":"` + ckptAt(h).String() + `"}`)
}

// ---- core: credit + checkpoints ------------------------------------------

func credit(acct string, amt *big.Int) {
	adapter.PullFrom(asset(), acct, amt) // pull from acct (must have approved us)
	applyCredit(acct, amt)
}

func creditFor(from, acct string, amt *big.Int) {
	adapter.PullFrom(asset(), from, amt) // pull from the allowlisted caller
	applyCredit(acct, amt)
}

func applyCredit(acct string, amt *big.Int) {
	newStake := new(big.Int).Add(getBig("stake|"+acct), amt)
	setBig("stake|"+acct, newStake)
	total := new(big.Int).Add(getBig(kTotal), amt)
	setBig(kTotal, total)
	h := blockHeight()
	appendHist(acct, h, newStake)
	appendCkpt(h, total)
}

func appendCkpt(h uint64, total *big.Int) {
	n := idx(kCkptN)
	sdk.StateSetObject("ckpt|"+strconv.FormatUint(n, 10),
		strconv.FormatUint(h, 10)+":"+total.String())
	sdk.StateSetObject(kCkptN, strconv.FormatUint(n+1, 10))
}

func appendHist(acct string, h uint64, stake *big.Int) {
	nk := "hist_n|" + acct
	n := idx(nk)
	sdk.StateSetObject("hist|"+acct+"|"+strconv.FormatUint(n, 10),
		strconv.FormatUint(h, 10)+":"+stake.String())
	sdk.StateSetObject(nk, strconv.FormatUint(n+1, 10))
}

// histAt: largest-index entry with height<=h (binary search; heights non-decreasing).
func histAt(acct string, h uint64) *big.Int {
	n := idx("hist_n|" + acct)
	return searchVal(func(i uint64) *string { return sdk.StateGetObject("hist|" + acct + "|" + strconv.FormatUint(i, 10)) }, n, h)
}

func ckptAt(h uint64) *big.Int {
	n := idx(kCkptN)
	return searchVal(func(i uint64) *string { return sdk.StateGetObject("ckpt|" + strconv.FormatUint(i, 10)) }, n, h)
}

// searchVal binary-searches [0,n) for the rightmost entry "height:amount" with
// height<=target and returns its amount (0 if none).
func searchVal(get func(uint64) *string, n, target uint64) *big.Int {
	res := new(big.Int)
	if n == 0 {
		return res
	}
	lo, hi := uint64(0), n-1
	found := false
	var foundI uint64
	for lo <= hi {
		mid := (lo + hi) / 2
		v := get(mid)
		if v == nil || *v == "" {
			break
		}
		hs, _ := splitColon(*v)
		hval, _ := strconv.ParseUint(hs, 10, 64)
		if hval <= target {
			found = true
			foundI = mid
			if mid == n-1 {
				break
			}
			lo = mid + 1
		} else {
			if mid == 0 {
				break
			}
			hi = mid - 1
		}
	}
	if found {
		if v := get(foundI); v != nil {
			_, amt := splitColon(*v)
			res.SetString(amt, 10)
		}
	}
	return res
}

// ---- small helpers -------------------------------------------------------

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

func blockHeight() uint64 {
	h := sdk.GetEnvKey("block.height")
	if h == nil {
		sdk.Abort("no block height")
	}
	n, err := strconv.ParseUint(*h, 10, 64)
	if err != nil {
		sdk.Abort("bad block height")
	}
	return n
}

func mustAmount(payload *string) *big.Int {
	a := parseBig(field(payload, "amount"))
	if a.Sign() <= 0 {
		sdk.Abort("amount must be > 0")
	}
	return a
}

func mustHeight(payload *string) uint64 {
	n, err := strconv.ParseUint(field(payload, "height"), 10, 64)
	if err != nil {
		sdk.Abort("height required")
	}
	return n
}

func idx(key string) uint64 {
	v := sdk.StateGetObject(key)
	if v == nil {
		return 0
	}
	n, _ := strconv.ParseUint(*v, 10, 64)
	return n
}

func getBig(key string) *big.Int { return parseBig(getStr(key)) }

func setBig(key string, v *big.Int) { sdk.StateSetObject(key, v.String()) }

func parseBig(s string) *big.Int {
	n := new(big.Int)
	if s != "" {
		n.SetString(s, 10)
	}
	return n
}

func getStr(key string) string {
	if v := sdk.StateGetObject(key); v != nil {
		return *v
	}
	return ""
}

func validateAddr(a string) {
	if len(a) == 0 || len(a) > 256 {
		sdk.Abort("bad address length")
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c == '|' || c == '"' || c == '\\' {
			sdk.Abort("illegal char in address")
		}
	}
}

func ok() *string  { return str(`{"success":true}`) }
func str(s string) *string { return &s }

func splitColon(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func splitComma(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// field extracts a flat JSON value by key. Matches "name" only in KEY position
// (followed by ':') so a value equal to a field name can't be mis-parsed (M4).
func field(payload *string, name string) string {
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
