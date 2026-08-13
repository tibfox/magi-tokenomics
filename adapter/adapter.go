// Package adapter is the value-asset abstraction (plan §4.3/§17.7). A deployment's
// backing asset is a fungible token (magi_token-contract). All framework contracts
// (C1/C2/C3/C5/C6/C7) go through this package so they stay asset-agnostic.
//
// The underlying contract is used AS-IS (no modification). This package wraps its
// existing entrypoints via ContractCall / ContractStateGet.
//
// ---------------------------------------------------------------------------
// EDITIONED-NFT MODE (kind "1") IS REJECTED AT INIT — DO NOT RE-ENABLE PIECEMEAL
// ---------------------------------------------------------------------------
// Plan Profile 3 ("NFT-backed") is DEFERRED (§19.1) and the prerequisites it
// depends on are NOT built. Wiring magi_nft-contract calls into Transfer/PullFrom/
// Mint alone would produce a deployment that inits cleanly, emits, and then
// silently strands most claimants. Concretely, against the canonical
// magi_nft-contract interface:
//
//  1. R4 (plan §18.1) mandates a fractional accumulator with cross-epoch carry —
//     per-bucket in C2 and per-claimant in C3/C5/C7 — because editions are whole
//     uint64 units. NONE of that exists: C2.recordAllocations does a bare
//     bps Div (remainder → dust only), and C3/C5.claim + C7.claim do a bare
//     funded*share/total Div and then `sdk.Abort("payout rounds to zero")`.
//     With edition-scale supply every small holder's claim aborts permanently and
//     their slice is stranded in the distributor. R4 also requires init to reject
//     configs where per-epoch emission < #buckets; that check does not exist.
//  2. C2 cannot even read the NFT's supply through the calls it makes. Its init
//     does ContractCall(token,"getInfo") and expects a global "maxSupply";
//     the NFT's getInfo returns only {name,symbol,baseUri,trackMinted} — supply is
//     PER-ID and needs maxSupply({"id":..}). distributeEpoch calls
//     totalSupply with an EMPTY payload; the NFT aborts "Payload required".
//     So C2 in NFT mode is undeployable today, and only by accident.
//  3. Amount width: framework amounts are big.Int; every NFT amount field is a
//     uint64 JSON number. Nothing clamps or range-checks the conversion.
//  4. Trust surface: magi_nft is a multi-id COLLECTION. D4/R8 require full
//     changeOwner(NFT)→C2, which hands C2 mint/burn/pause/setURI/soulbound
//     authority over every id in that collection, not just the value-asset id.
//     The delegated-operator alternative is explicitly disallowed by R8 because it
//     leaves the human owner's own unbounded mint (dilution) and global pause in
//     place. R16 additionally requires rejecting soulbound ids: the NFT refuses
//     safeTransferFrom when isSoulbound(id) && from != owner, so editions pulled
//     into C1 custody could never be returned. No such guard exists either.
//  5. The DEX does not trade editions. `vsc-eco/dex-contracts` pools fungible
//     balances, so an editioned NFT cannot be an AMM asset there and no pool can
//     exist for it. That breaks the C5 bucket outright rather than merely making it
//     inaccurate: an LP-reward distributor for an asset with no liquidity pool has
//     no providers to pay, and the reporter's `lp` mode has no add_liq/rem_liq
//     events to replay. This one is not fixable inside this repo — it needs the DEX
//     to support the asset first — so it bounds when NFT mode could be revisited
//     at all, independently of the arithmetic above.
//
// Until R4/R16 are implemented across C2/C3/C5/C7, C2's supply reads are made
// kind-aware, and the DEX can pool the asset, the only safe behaviour is to fail
// closed: RequireFungible rejects kind "1" at init in every contract, and the
// movement functions below keep an unconditional abort as a second line of defence.
package adapter

import (
	"magi_token/sdk"
	"math/big"
)

type Kind uint8

const (
	Fungible     Kind = 0
	EditionedNFT Kind = 1
)

// Asset identifies the value asset for a deployment.
type Asset struct {
	Kind     Kind
	Contract string // token or NFT contract id
	TokenId  string // NFT: the single editioned id; unused for Fungible
}

// unsupported is the single abort used everywhere the editioned-NFT path would
// have been taken. See the package doc for why it is not implemented.
func unsupported() {
	sdk.Abort("adapter: editioned-NFT value asset (kind=1) is not supported — " +
		"NFT mode requires the R4 integer-carry accumulator in C2/C3/C5/C7, " +
		"kind-aware supply reads in C2, and R16 soulbound rejection, none of which " +
		"are implemented; deploy with kind=0 (fungible) only")
}

// RequireFungible validates the init-time `kind` config field. It is the single
// gate every contract's init calls so a kind=1 deployment is impossible rather
// than merely broken at first transfer. Returns the canonical stored value.
//
// Only "0" is accepted. "1" gets the explicit NFT-unsupported abort; anything
// else (including an absent/empty field) is rejected outright, so a typo cannot
// silently fall through to fungible behaviour.
func RequireFungible(kind string) string {
	switch kind {
	case "0":
		return "0"
	case "1":
		unsupported()
	}
	sdk.Abort(`adapter: kind must be "0" (fungible); kind "1" (editioned NFT) is not supported`)
	return ""
}

// RequireNoTokenId rejects a non-empty `tokenId` at init. tokenId only ever meant
// "the single editioned NFT id"; accepting it in fungible mode would let an
// operator believe they had configured NFT mode when they had not.
func RequireNoTokenId(tokenId string) {
	if tokenId != "" {
		sdk.Abort(`adapter: tokenId is only meaningful for kind="1" (editioned NFT), which is not supported — omit it`)
	}
}

// ---- reads ---------------------------------------------------------------

// ---- movement ------------------------------------------------------------

// Transfer sends `amount` from THIS contract to `to`.
// NOTE ON THE ZERO CASE — deliberately different from PullFrom, which aborts.
//
// Transfer is the PAYOUT direction and its callers legitimately compute zero: a claim
// pays funded*share/totalShares, which truncates to 0 for a small enough share, and
// that rounding is documented as favouring the pot. Aborting there would make a claim
// fail rather than pay dust, and would leave the claimant unable to mark the epoch
// claimed. PullFrom is the FUNDING direction, where a zero pull always means a
// miscalculation upstream and should be loud.
func Transfer(a Asset, to string, amount *big.Int) {
	if amount.Sign() <= 0 {
		return
	}
	switch a.Kind {
	case Fungible:
		sdk.ContractCall(a.Contract, "transfer",
			`{"to":"`+safe(to)+`","amount":"`+amount.String()+`"}`, nil)
	case EditionedNFT:
		unsupported()
	default:
		sdk.Abort("adapter: unknown kind")
	}
}

// Approve lets `spender` pull up to `amount` from THIS contract.
//
// The mirror image of PullFrom, and the only way one contract can hand tokens to
// another contract's custody: a plain Transfer moves the balance but tells the
// receiver nothing, so it cannot credit anyone. Approve-then-call lets the receiver
// PULL exactly what it was told about, which is what makes the amount verifiable
// rather than asserted.
//
// Grant exactly what is about to be pulled and no more. A standing allowance is a
// standing right to drain this contract up to it.
func Approve(a Asset, spender string, amount *big.Int) {
	if amount.Sign() <= 0 {
		return
	}
	switch a.Kind {
	case Fungible:
		sdk.ContractCall(a.Contract, "approve",
			`{"spender":"`+safe(spender)+`","amount":"`+amount.String()+`"}`, nil)
	case EditionedNFT:
		unsupported()
	default:
		sdk.Abort("adapter: unknown kind")
	}
}

// PullFrom pulls `amount` from `acct` into THIS contract. `acct` must have first
// approved this contract (token approve → transferFrom).
func PullFrom(a Asset, acct string, amount *big.Int) {
	if amount.Sign() <= 0 {
		sdk.Abort("adapter: pull amount must be > 0")
	}
	switch a.Kind {
	case Fungible:
		// token.transferFrom: caller (this contract) is the spender; `to` = self.
		sdk.ContractCall(a.Contract, "transferFrom",
			`{"from":"`+safe(acct)+`","to":"`+self()+`","amount":"`+amount.String()+`"}`, nil)
	case EditionedNFT:
		unsupported()
	default:
		sdk.Abort("adapter: unknown kind")
	}
}

// ---- helpers -------------------------------------------------------------

// self returns THIS contract's ledger identity as the token sees it: the token
// keys balances by msg.caller, which for a contract caller is "contract:<id>"
// (not the bare id). PullFrom must credit the same key Transfer later spends from.
func self() string {
	id := sdk.GetEnvKey("contract.id")
	if id == nil {
		sdk.Abort("adapter: no contract.id")
	}
	return "contract:" + *id
}

// safe rejects the state-key delimiter and JSON-breaking chars in an id. Ids are
// already validated upstream, but we defend the payload we build by concatenation.
func safe(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c == '|' {
			sdk.Abort("adapter: illegal char in id")
		}
	}
	return s
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

// NOTE: Mint, BalanceOf and parseAmountField were removed when C2 stopped minting. Emission now draws
// from an approved pool via PullFrom, so nothing in the framework mints, and no
// caller needed a balance read. They were left behind as exported-but-unreachable
// API — which reads as "this is supported" to the next person. parseAmountField went
// with them: it was BalanceOf's parser, it returned 0 for an absent or unparseable
// field (a silent zero on the value path), and it matched keys by bare substring
// rather than requiring KEY position as the contracts' own f()/pickField do. Two
// parsers with two safety levels on one wire format is a trap; there is now one.
// Reinstate any of them only with a caller, and with the strict parser.

// BalanceOfSelf reads this contract's own balance of the asset.
//
// Reinstated deliberately and under the conditions the removal note set out: it has a
// caller (C6.sweepResidual needs to know what is actually held before deciding what
// is residual), and it parses STRICTLY.
//
// "Strictly" is the whole point. The deleted parser returned 0 for an absent or
// unparseable field — a silent zero on the value path, which here would compute a
// residual of 0 and make a legitimate sweep abort as "nothing to sweep". Worse, it
// matched keys by bare substring, so `"balance"` occurring inside a VALUE could be
// read as the field. This one requires the match to be in key position and ABORTS
// when the field is missing or malformed, because a balance we could not read is not
// a balance of zero.
func BalanceOfSelf(a Asset) *big.Int {
	if a.Kind != Fungible {
		unsupported()
	}
	res := sdk.ContractCall(a.Contract, "balanceOf", `{"account":"`+self()+`"}`, nil)
	if res == nil {
		sdk.Abort("adapter: balanceOf returned nothing")
	}
	raw := keyField(*res, "balance")
	if raw == "" {
		sdk.Abort("adapter: balanceOf response has no `balance` field")
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		sdk.Abort("adapter: balanceOf `balance` is not an integer")
	}
	if n.Sign() < 0 {
		sdk.Abort("adapter: balanceOf returned a negative balance")
	}
	return n
}

// keyField extracts a flat JSON field, matching `name` ONLY in key position — the
// match must be followed by `"` then `:`. This is the same rule the contracts' own
// f() uses (M4); the adapter previously used a weaker bare-substring match.
func keyField(res, name string) string {
	needle := `"` + name + `"`
	for i := 0; i+len(needle) <= len(res); i++ {
		if res[i:i+len(needle)] != needle {
			continue
		}
		j := i + len(needle)
		for j < len(res) && (res[j] == ' ' || res[j] == '\t') {
			j++
		}
		if j >= len(res) || res[j] != ':' {
			continue // this was a VALUE that happens to equal the name
		}
		j++
		for j < len(res) && (res[j] == ' ' || res[j] == '\t') {
			j++
		}
		quoted := j < len(res) && res[j] == '"'
		if quoted {
			j++
		}
		k := j
		for k < len(res) {
			c := res[k]
			if quoted && c == '"' {
				break
			}
			if !quoted && (c == ',' || c == '}') {
				break
			}
			k++
		}
		return res[j:k]
	}
	return ""
}
