// C1 — Staking, staking yield, and the launch airdrop.
//
// Three roles in one contract because each deploy costs a real fee and these three
// share a balance and a history anyway:
//
//	STAKING   stakes the value asset via allowance and keeps per-account stake plus a
//	          height-checkpointed running total, so stakeAtHeight/totalStakedAtHeight
//	          are consistent by construction (invariant #3). stakeFor conserves
//	          Σstake == custody (R7).
//	YIELD     pays stakers pro-rata from a C2 bucket, with no reporter — it reads the
//	          stake history directly. Merged here because it never read anything else.
//	AIRDROP   a launch-time snapshot import, optionally crediting stake directly.
//	          Runs a handful of times and is then dead, which is precisely why it does
//	          not warrant a contract of its own.
//
// THE ONE THING TO PRESERVE. Three pools now share one balance, and the custody rule
// is no longer the simple Σstake == balance:
//
//	balance >= total_staked + (yield funded but unclaimed) + whatever the airdrop
//	           has left
//
// Only the airdrop may spend the unobligated remainder, and it checks that before
// paying (see the airdrop section). Yield pays from its own funded pool; principal is
// only ever returned to the staker who put it in. If those envelopes ever leak into
// each other, rewards get paid out of somebody's principal.
//
// Single-tenant (D3). Amounts are decimal strings. Checkpoint/history entries are
// appended in block order, so heights are non-decreasing by index → binary-searchable.
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

// ---- config / state keys -------------------------------------------------

const (
	kInit     = "init"
	kToken    = "cfg_token"
	kKind     = "cfg_kind"
	kTokenId  = "cfg_tokenid"
	kCooldown = "cfg_cooldown"
	kTotal    = "total_staked"
	kCkptN    = "ckpt_n"
	kEpochLen = "cfg_epochLen"
	kGenesis  = "cfg_genesis"
	kFunder   = "cfg_funder"
	kBucket   = "cfg_bucket"   // the C2 bucket that funds yield here
	kTreasury = "cfg_treasury" // pinned sweep destination
	kMaxAir   = "cfg_maxAirdrop"
	kAirStake = "cfg_airdropStaked" // "1" = airdrop credits stake instead of transferring
	kAirTotal = "airdrop_total"
	// kYieldOutstanding is yield pulled from C2 but not yet claimed or swept. Tracked
	// incrementally because deriving it would mean summing every epoch ever funded.
	kYieldOutstanding = "yield_outstanding"
	// kUnstakeOutstanding is principal that has left total_staked but not yet left the
	// contract: unstaked, queued, serving its cooldown.
	//
	// `unstake` has to drop total_staked immediately or an account on its way out keeps
	// earning weight, but it moves no tokens — they sit in custody until claimUnstaked.
	// Without this term the envelope counts that money as free float for the whole
	// cooldown, which init requires to exceed a full epoch, and both spenders of float
	// (airdropBatch, sweepUnobligated) will send a staker's principal elsewhere. That
	// is not hypothetical: sweepUnobligated returned {"swept":"1000"} against a queued
	// withdrawal and the staker's own claimUnstaked then failed on "Insufficient
	// balance". See itest/unstake_envelope_test.go.
	//
	// Tracked incrementally for the same reason as yield above: deriving it would mean
	// walking every account's us| queue.
	kUnstakeOutstanding = "unstake_outstanding"
	maxClaim            = 20 // bound RC per claimUnstaked call
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
	// A POSTING key must not configure a deployment.
	//
	// The runtime derives msg.caller from RequiredPostingAuths[0] when a transaction
	// carries no active auth (auth/auth.go:90), so comparing msg.caller to the owner
	// is satisfied by a posting-only transaction from the owner's account. Posting
	// authority is the half Hive users delegate to front-ends and think of as safe.
	// The fund-moving owner entrypoints already demand active auth; configuring the
	// deployment is not a lesser power, and init pins the token, treasury, guardian set and stakeFor allowlist, once.
	//
	// UnlessContract, so a DAO or multisig CONTRACT owner still works: a contract
	// caller has no key at all and can never appear in required_auths, and exempting
	// it cannot reintroduce the posting-key risk it is guarding against.
	auth.RequireActiveUnlessContract(*caller, reqAuths())
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
	// CROSS-CHECK the epochLen against the emission schedule when a funder is given.
	//
	// R15 ("cooldown must exceed an epoch, so a one-block stake cannot capture a full
	// epoch of C7 yield") is only as good as the epochLen it was checked against, and
	// that value is supplied by the caller. C7 already solves exactly this: it pulls
	// scheduleInfo from its funder and rejects a mismatch. C1 had no such check, so a
	// typo'd epochLen silently produced a WEAKER cooldown guarantee than the operator
	// believed — R15 would pass against a fictional epoch length.
	//
	// `funder` is optional so a standalone C1 (no emission contract, e.g. staking
	// before C2 exists) still deploys. When present the check is exact.
	if fu := field(payload, "funder"); fu != "" {
		validateAddr(fu)
		sch := sdk.ContractCall(fu, "scheduleInfo", "", nil)
		se := pickField(sch, "epochLen")
		if se == "" {
			sdk.Abort("funder scheduleInfo unavailable — init the emission contract (C2) first")
		}
		if se != field(payload, "epochLen") {
			sdk.Abort("epochLen mismatch with funder — R15's cooldown check would be made against the wrong epoch")
		}
		sdk.StateSetObject(kFunder, fu)
		// A funder at init means C2 already exists, so its genesis is knowable NOW and
		// the separate adoptSchedule step is unnecessary. Adopting here keeps the
		// drawdown accumulator armed from C1's very first block in that deploy order.
		if sg := pickField(sch, "genesis"); sg != "" {
			if _, err := strconv.ParseUint(sg, 10, 64); err != nil {
				sdk.Abort("funder genesis is not a uint")
			}
			sdk.StateSetObject(kGenesis, sg)
		}
	}
	// Store it either way, so the value R15 was checked against is auditable after
	// deploy rather than parsed once and discarded.
	sdk.StateSetObject(kEpochLen, field(payload, "epochLen"))
	// stakeFor allowlist (immutable)
	for _, a := range splitComma(field(payload, "allow")) {
		if a != "" {
			validateAddr(a)
			sdk.StateSetObject("allow|"+a, "1")
		}
	}
	// ---- optional capabilities -------------------------------------------
	//
	// Yield and the airdrop are configured, not switched on. A deployment that wants
	// staking alone supplies none of the below and gets exactly that: pullFunding has
	// no bucket to pull, sweepEmptyEpoch has no guardian to authorise it, and
	// airdropBatch has no cap to work within, so each refuses rather than sitting
	// there as a half-wired feature.

	// Treasury: where sweepEmptyEpoch sends an epoch nobody can claim. Pinned now so
	// a sweep can never be redirected later.
	if tre := field(payload, "treasury"); tre != "" {
		validateAddr(tre)
		if !isLedgerAddr(tre) {
			sdk.Abort("treasury must start with hive:/contract:/did:/system:")
		}
		if tre == selfAddr() {
			sdk.Abort("treasury cannot be this contract")
		}
		// A guardian-controlled treasury turns a sweep into a drain.
		for _, g := range splitComma(field(payload, "guardianAuth")) {
			if g != "" && g == tre {
				sdk.Abort("treasury must not be a guardian authority")
			}
		}
		sdk.StateSetObject(kTreasury, tre)
	}
	if gm := field(payload, "guardianMode"); gm != "" {
		sdk.StateSetObject("cfg_gMode", gm)
		sdk.StateSetObject("cfg_gAuth", field(payload, "guardianAuth"))
		sdk.StateSetObject("cfg_gThr", field(payload, "guardianThreshold"))
		auth.Validate(guardianCfg())
		if getStr(kTreasury) == "" {
			sdk.Abort("guardian configured without a treasury — a sweep would have nowhere to go")
		}
	}
	// Airdrop cap. Absent means the airdrop entrypoint is unavailable.
	if ma := field(payload, "maxAirdrop"); ma != "" {
		if parseBig(ma).Sign() <= 0 {
			sdk.Abort("maxAirdrop must be > 0 when set")
		}
		sdk.StateSetObject(kMaxAir, ma)
		// Credit stake directly instead of transferring liquid tokens. Recipients then
		// earn yield from the first epoch and must serve the unstake cooldown before
		// they can sell, which is usually the point of doing it this way.
		if field(payload, "airdropStaked") == "1" {
			sdk.StateSetObject(kAirStake, "1")
		}
	}

	setBig(kTotal, new(big.Int))
	setBig(kYieldOutstanding, new(big.Int))
	setBig(kUnstakeOutstanding, new(big.Int))
	sdk.StateSetObject(kCkptN, "0")
	sdk.StateSetObject(kInit, "1")
	// Discovery anchor for the indexer — see the note on c2_init. Emitted last so it
	// only ever describes a deployment that fully initialised.
	events.New("c1_init").
		Str("token", tok).
		Str("owner", *owner).
		Str("cooldown", cd).
		Str("epoch_len", field(payload, "epochLen")).
		Str("funder", getStr(kFunder)).
		Str("genesis", getStr(kGenesis)).
		Str("treasury", getStr(kTreasury)).
		Str("max_airdrop", getStr(kMaxAir)).
		Bool("airdrop_staked", getStr(kAirStake) == "1").
		Str("allow", field(payload, "allow")).
		Emit()
	return ok()
}

// stake: {"amount"} — caller must have approved this contract on the token.
//
//go:wasmexport stake
func Stake(payload *string) *string {
	assertInit()
	c := mustCaller()
	// Fund-moving and caller-relative: require an ACTIVE authority. Without this a
	// Hive POSTING key — routinely delegated to third-party front-ends — moves the
	// signer's stake, because the runtime derives msg.caller from
	// RequiredPostingAuths[0] when no active auth is present. C1 custody is the only
	// layer that can stop that: the token contract authorizes transfers on msg.caller
	// alone. Contract callers are exempt (see auth.RequireActiveUnlessContract) or
	// contract-held stake could never be withdrawn.
	auth.RequireActiveUnlessContract(c, reqAuths())
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
	if !present("allow|" + c) {
		sdk.Abort("caller not in stakeFor allowlist")
	}
	// UnlessContract, not RequireActive. The check exists to stop a posting key
	// standing in for an active one, and a CONTRACT caller has no key at all — the
	// runtime supplies "contract:<id>", which can never appear in required_auths.
	// Insisting on it here would make stakeFor unreachable from another contract,
	// which is precisely how a distributor pays a reward straight into stake. The
	// allowlist above is what authorises a contract, and it is immutable after init.
	auth.RequireActiveUnlessContract(c, reqAuths()) // CRIT-2
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
	// Fund-moving and caller-relative: require an ACTIVE authority. Without this a
	// Hive POSTING key — routinely delegated to third-party front-ends — moves the
	// signer's stake, because the runtime derives msg.caller from
	// RequiredPostingAuths[0] when no active auth is present. C1 custody is the only
	// layer that can stop that: the token contract authorizes transfers on msg.caller
	// alone. Contract callers are exempt (see auth.RequireActiveUnlessContract) or
	// contract-held stake could never be withdrawn.
	auth.RequireActiveUnlessContract(c, reqAuths())
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
	noteDrawdown(c, cur, newStake, h) // BEFORE appendHist — see the function doc
	appendHist(c, h, newStake)
	appendCkpt(h, total)
	// queue withdrawal. The amount leaves total_staked above but NOT the contract, so
	// it moves from one obligation to another rather than becoming free float.
	setBig(kUnstakeOutstanding, new(big.Int).Add(getBig(kUnstakeOutstanding), amt))
	tail := idx("us_tail|" + c)
	ready := h + cooldown()
	sdk.StateSetObject("us|"+c+"|"+strconv.FormatUint(tail, 10),
		amt.String()+":"+strconv.FormatUint(ready, 10))
	sdk.StateSetObject("us_tail|"+c, strconv.FormatUint(tail+1, 10))
	events.New("unstake").
		Str("acct", c).
		Big("amount", amt).
		Big("new_stake", newStake).
		Big("new_total", total).
		U64("height", h).
		U64("ready_height", ready).
		U64("queue_idx", tail).
		Emit()
	return ok()
}

// claimUnstaked: withdraw matured unstake entries (FIFO, bounded per call).
//
//go:wasmexport claimUnstaked
func ClaimUnstaked(_ *string) *string {
	assertInit()
	c := mustCaller()
	// Fund-moving and caller-relative: require an ACTIVE authority. Without this a
	// Hive POSTING key — routinely delegated to third-party front-ends — moves the
	// signer's stake, because the runtime derives msg.caller from
	// RequiredPostingAuths[0] when no active auth is present. C1 custody is the only
	// layer that can stop that: the token contract authorizes transfers on msg.caller
	// alone. Contract callers are exempt (see auth.RequireActiveUnlessContract) or
	// contract-held stake could never be withdrawn.
	auth.RequireActiveUnlessContract(c, reqAuths())
	h := blockHeight()
	head := idx("us_head|" + c)
	tail := idx("us_tail|" + c)
	paid := new(big.Int)
	n := 0
	// maxClaim bounds ITERATIONS, not payments. It used to bound `n`, which only
	// increments when an entry is actually paid — so the defensive branch below for a
	// missing queue entry advanced the head and continued without counting, and a run
	// of empty slots would have iterated unbounded in one call. No gap can form (an
	// entry is deleted only as it is paid, and the head advances past it in the same
	// transaction), so this was a bound held by an invariant rather than by the guard
	// that appears to enforce it. Now the guard holds it.
	for steps := 0; head < tail && steps < maxClaim; steps++ {
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
		paid.Add(paid, amt)
	}
	sdk.StateSetObject("us_head|"+c, strconv.FormatUint(head, 10))
	// ONE transfer for the whole batch rather than one per matured entry. Every
	// iteration paid the SAME recipient the SAME asset and differed only in amount,
	// so up to maxClaim(20) cross-contract calls were doing one call's work — about
	// 4,000 RC on a full claim at the token's measured per-transfer cost.
	//
	// CEI is unaffected, and in fact strengthened: every delete and the head advance
	// now complete before anything external is touched, rather than interleaving.
	if paid.Sign() > 0 {
		// Release the obligation BEFORE the transfer, with the deletes and the head
		// advance above — the money is about to stop being owed because it is about to
		// be handed over. Leaving it counted would ratchet the reserve up forever and
		// make genuinely free float permanently unsweepable.
		setBig(kUnstakeOutstanding, new(big.Int).Sub(getBig(kUnstakeOutstanding), paid))
		adapter.Transfer(asset(), c, paid)
		events.New("unstake_claim").
			Str("acct", c).
			Big("amount", paid).
			Int("entries", n).
			U64("new_head", head).
			Emit()
	}
	return str(`{"claimed":"` + paid.String() + `"}`)
}

// ---- queries -------------------------------------------------------------

// scheduleInfo reports the epochLen this instance's R15 cooldown check was made
// against, so a contract that inits LATER can verify it. C1 cannot do that check
// itself in the normal deploy order: stake must exist before C2's genesis (or epoch
// 0's yield is unclaimable), so C1 is initialised BEFORE C2 and has nothing to ask.
//
//go:wasmexport scheduleInfo
func ScheduleInfo(_ *string) *string {
	// Deliberately NOT assertInit. This is a read-only query whose CALLER is another
	// contract's init, and an abort inside a nested call reverts the caller's whole
	// transaction — so aborting here would turn "C1 is not initialised yet" into "C7
	// cannot be deployed", inventing a deploy-order requirement rather than reporting
	// a fact. Empty fields say "nothing to check", which is what an uninitialised
	// instance actually means.
	// `genesis` is empty until adoptSchedule runs. C7 reads it to confirm this C1 is
	// actually accumulating drawdowns before it agrees to pay against them.
	return str(`{"epochLen":"` + getStr(kEpochLen) + `","cooldown":"` + getStr(kCooldown) +
		`","genesis":"` + getStr(kGenesis) + `"}`)
}

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

// adoptSchedule: {"funder"} — learn the emission schedule from C2, ONCE.
//
// C1 cannot take `genesis` at init. The deploy order is C1 → C2 → C7 (stake has to
// exist before C2's genesis or epoch 0's yield is funded-but-unclaimable), and C2's
// genesis defaults to its own deploy block — a value that does not exist yet while
// C1 is being initialised. So the schedule is adopted afterwards, from the contract
// that defines it, rather than copied by hand into two places.
//
// OWNER-ONLY, and that is load-bearing rather than ceremonial: `genesis` decides
// which epoch bucket every drawdown lands in. A caller who could point C1 at a
// fabricated C2 would shift the boundaries, shrink the drawdown for a real epoch,
// and inflate C7's denominator against the payouts it has already made. Adoption is
// also once-only for the same reason — re-anchoring live epochs would retroactively
// invalidate every accumulator already written.
//
//go:wasmexport adoptSchedule
func AdoptSchedule(payload *string) *string {
	assertInit()
	owner, caller := sdk.GetEnvKey("contract.owner"), sdk.GetEnvKey("msg.caller")
	if owner == nil || caller == nil || *owner != *caller {
		sdk.Abort("only contract owner can adopt the schedule")
	}
	// A POSTING key must not configure a deployment.
	//
	// The runtime derives msg.caller from RequiredPostingAuths[0] when a transaction
	// carries no active auth (auth/auth.go:90), so comparing msg.caller to the owner
	// is satisfied by a posting-only transaction from the owner's account. Posting
	// authority is the half Hive users delegate to front-ends and think of as safe.
	// The fund-moving owner entrypoints already demand active auth; configuring the
	// deployment is not a lesser power, and adoptSchedule is one-shot, so reaching it spends the only chance to configure yield.
	//
	// UnlessContract, so a DAO or multisig CONTRACT owner still works: a contract
	// caller has no key at all and can never appear in required_auths, and exempting
	// it cannot reintroduce the posting-key risk it is guarding against.
	auth.RequireActiveUnlessContract(*caller, reqAuths())
	if present(kGenesis) {
		sdk.Abort("schedule already adopted (immutable: re-anchoring would invalidate every drawdown already recorded)")
	}
	fu := field(payload, "funder")
	if fu == "" {
		sdk.Abort("funder required")
	}
	validateAddr(fu)
	sch := sdk.ContractCall(fu, "scheduleInfo", "", nil)
	sg, se := pickField(sch, "genesis"), pickField(sch, "epochLen")
	if sg == "" || se == "" {
		sdk.Abort("funder scheduleInfo unavailable — init the emission contract (C2) first")
	}
	// The epochLen R15's cooldown check was made against must be the one the funder
	// actually runs, or the drawdown buckets and C7's snapshots straddle each other.
	if se != getStr(kEpochLen) {
		sdk.Abort("funder epochLen disagrees with this contract's — R15's cooldown check was made against the wrong epoch length")
	}
	if _, err := strconv.ParseUint(sg, 10, 64); err != nil {
		sdk.Abort("funder genesis is not a uint")
	}
	sdk.StateSetObject(kGenesis, sg)
	sdk.StateSetObject(kFunder, fu)
	// The yield bucket is adopted here for the same reason as the genesis: it names a
	// bucket on a contract that did not exist when this one was initialised. Verified
	// against the funder immediately — a name C2 does not know, or one paying somebody
	// else, would otherwise deploy cleanly and fail at the first pullFunding, after
	// the epoch it should have funded had already elapsed.
	if bucket := field(payload, "bucket"); bucket != "" {
		bt := pickField(sdk.ContractCall(fu, "bucketTarget",
			`{"bucket":"`+bucket+`"}`, nil), "target")
		if bt == "" {
			sdk.Abort("the funder has no bucket by that name")
		}
		if bt != selfAddr() {
			sdk.Abort("the funder's bucket of that name pays a different contract")
		}
		sdk.StateSetObject(kBucket, bucket)
	}
	events.New("schedule_adopted").
		Str("funder", fu).
		Str("genesis", sg).
		Str("epoch_len", se).
		Str("bucket", getStr(kBucket)).
		Emit()
	return ok()
}

// minStakeSum: {"epoch"} → Σᵢ min(stakeᵢ(start), stakeᵢ(end)) — the EXACT denominator
// C7 must divide an epoch's yield by. See the drawdown accumulator's doc for why
// this cannot be derived from the running total alone.
//
// Reports "" when no schedule has been adopted, so C7 can refuse loudly at init
// rather than quietly pay against a denominator that over-counts.
//
//go:wasmexport minStakeSum
func MinStakeSum(payload *string) *string {
	assertInit()
	el := idx(kEpochLen)
	if el == 0 || !present(kGenesis) {
		return str(`{"total":""}`)
	}
	ep, err := strconv.ParseUint(field(payload, "epoch"), 10, 64)
	if err != nil {
		sdk.Abort("epoch required")
	}
	return str(`{"total":"` + minStakeSumAt(ep).String() + `"}`)
}

// epochBounds returns the first and last block of `ep`, and is the ONLY place this
// contract turns an epoch index into heights.
//
// It exists because the wrap guard used to live in exactly one place — the read-only
// minStakeSum query — while claimYield, sweepEmptyEpoch and minStakeSumAt did the same
// `g + ep*el` arithmetic on the same caller-supplied index with no check at all. The
// guard was on the path that moves nothing and absent from the three that move tokens.
//
// A wrapped end height is small, so `end >= blockHeight()` passes and an epoch that
// does not exist looks fully elapsed; ckptAt/histAt then binary-search to a REAL
// checkpoint at that wrapped height and return a confident, wrong number. Nothing could
// reach it — every caller is gated behind funded > 0 for that epoch, and funding only
// exists for epochs C2 actually distributed — but that is a precondition in another
// contract, not a check where the arithmetic happens.
//
// The bound is stricter than the old one because it covers what the callers actually
// compute: g + (ep+1)*el - 1, not just g + ep*el. It can only refuse epoch indices near
// 2^64, which no elapsed-block count can produce.
func epochBounds(ep uint64) (uint64, uint64) {
	g, el := idx(kGenesis), idx(kEpochLen)
	if el == 0 {
		sdk.Abort("no schedule adopted")
	}
	if ep+1 < ep || ep+1 > (^uint64(0)-g)/el {
		sdk.Abort("epoch out of range")
	}
	start := g + ep*el
	return start, g + (ep+1)*el - 1
}

// minStakeSumAt is the exact Σᵢ min(aᵢ,bᵢ) for a CLOSED epoch: the epoch-start total
// less everything that dropped below its starting level. Shared by the query above
// and by claimYield, so an external verifier and the payout can never disagree.
func minStakeSumAt(ep uint64) *big.Int {
	start, end := epochBounds(ep)
	// Refuse while the epoch is still open: the drawdown is still moving, so an
	// answer now could exceed the final one and over-pay whoever claimed early.
	if end >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	v := new(big.Int).Sub(ckptAt(start), getBig("dd|"+strconv.FormatUint(ep, 10)))
	if v.Sign() < 0 {
		v = new(big.Int) // unreachable: drawdown ≤ Σa by construction
	}
	return v
}

// ---- core: credit + checkpoints ------------------------------------------

func credit(acct string, amt *big.Int) {
	adapter.PullFrom(asset(), acct, amt) // pull from acct (must have approved us)
	applyCredit(acct, amt, "stake")
}

func creditFor(from, acct string, amt *big.Int) {
	adapter.PullFrom(asset(), from, amt) // pull from the allowlisted caller
	applyCredit(acct, amt, "stakeFor")
}

// applyCredit records a stake increase. `via` names the entrypoint that caused it
// — stake, stakeFor or airdrop — because all three land here and an indexer
// reading only the resulting balance cannot tell a purchase from a migration
// credit, which are very different things to a tenant reading their own numbers.
func applyCredit(acct string, amt *big.Int, via string) {
	old := getBig("stake|" + acct)
	newStake := new(big.Int).Add(old, amt)
	setBig("stake|"+acct, newStake)
	total := new(big.Int).Add(getBig(kTotal), amt)
	setBig(kTotal, total)
	h := blockHeight()
	noteDrawdown(acct, old, newStake, h) // BEFORE appendHist — see the function doc
	appendHist(acct, h, newStake)
	appendCkpt(h, total)
	events.New("stake").
		Str("acct", acct).
		Str("via", via).
		Big("amount", amt).
		Big("new_stake", newStake).
		Big("new_total", total).
		U64("height", h).
		Emit()
}

// SAME-HEIGHT ENTRIES COLLAPSE. Several mutations inside one block used to append
// several entries all carrying that block's height, and `searchVal` — which returns
// the RIGHTMOST entry with height <= target — can only ever reach the last of them.
// The earlier ones were paid for and then unreachable.
//
// It shows worst on `airdropBatch` with `airdropStaked=1`, where every recipient runs
// through applyCredit in ONE transaction: a 25-holder batch appended 25 global
// checkpoints where 1 was needed, and a 1,000-holder migration 1,000 — permanently,
// since these arrays are never pruned and every later stakeAtHeight / claimYield /
// minStakeSum binary-searches them at one host read per probe.
//
// Overwriting the last entry instead is exactly equivalent: the collapsed entry holds
// the same value the search would have returned. The trade is one read for one write
// per collision, and the read is skipped entirely when there is no previous entry —
// which is every recipient of a migration, since each is a new account with no history.
func appendCkpt(h uint64, total *big.Int) {
	n := idx(kCkptN)
	hs := strconv.FormatUint(h, 10)
	rec := hs + ":" + total.String()
	if n > 0 {
		k := "ckpt|" + strconv.FormatUint(n-1, 10)
		if prev := sdk.StateGetObject(k); prev != nil && *prev != "" {
			if ph, _ := splitColon(*prev); ph == hs {
				sdk.StateSetObject(k, rec)
				return
			}
		}
	}
	sdk.StateSetObject("ckpt|"+strconv.FormatUint(n, 10), rec)
	sdk.StateSetObject(kCkptN, strconv.FormatUint(n+1, 10))
}

func appendHist(acct string, h uint64, stake *big.Int) {
	nk := "hist_n|" + acct
	n := idx(nk)
	hs := strconv.FormatUint(h, 10)
	rec := hs + ":" + stake.String()
	if n > 0 {
		k := "hist|" + acct + "|" + strconv.FormatUint(n-1, 10)
		if prev := sdk.StateGetObject(k); prev != nil && *prev != "" {
			if ph, _ := splitColon(*prev); ph == hs {
				sdk.StateSetObject(k, rec)
				return
			}
		}
	}
	sdk.StateSetObject("hist|"+acct+"|"+strconv.FormatUint(n, 10), rec)
	sdk.StateSetObject(nk, strconv.FormatUint(n+1, 10))
}

// histAt: largest-index entry with height<=h (binary search; heights non-decreasing).
func histAt(acct string, h uint64) *big.Int {
	n := idx("hist_n|" + acct)
	return searchVal(func(i uint64) *string { return sdk.StateGetObject("hist|" + acct + "|" + strconv.FormatUint(i, 10)) }, n, h)
}

// ---- the epoch drawdown accumulator (C7's exact denominator) --------------
//
// WHY THIS EXISTS. C7 pays each staker on min(stake at epoch start, stake at epoch
// end), so that only stake held for the WHOLE epoch earns and neither joining late
// nor leaving early is rewarded. That rule is right, but it leaves C7 without a
// denominator it can compute. The correct one is
//
//	S = Σᵢ min(aᵢ, bᵢ)     aᵢ = stakeᵢ(epoch start), bᵢ = stakeᵢ(epoch end)
//
// and no contract can evaluate it directly: summing a PER-ACCOUNT minimum means
// iterating every staker, which a contract cannot do. C7 therefore used to divide by
// min(Σa, Σb) — the only figure it could see — which is ≥ S whenever stakers move in
// BOTH directions during one epoch. Payouts then summed to less than `funded`, and
// the shortfall was money no account could ever claim. Recovering it needed a
// guardian sweep; the sweep could not be allowed to take a slow claimant's share, so
// claims had to CLOSE first. That is the whole origin of C7's old ~10-day claim
// deadline: not a policy anyone wanted, just a consequence of a denominator that
// over-counted.
//
// The gap is attributable entirely to accounts that ENDED the epoch below where they
// started, because min(aᵢ,bᵢ) = aᵢ - max(0, aᵢ-bᵢ). So
//
//	S = Σaᵢ - Σ max(0, aᵢ-bᵢ) = totalStakedAtHeight(start) - drawdown(epoch)
//
// and `drawdown` is ONE running number per epoch, maintainable in O(1) on each stake
// change. C7 divides by the exact S, nothing is left unclaimable beyond truncation
// dust, there is nothing worth sweeping, and claims never have to close.
//
// TELESCOPING. For an account's mutations at h₁<…<h_k inside one epoch, each update
// adds (new contribution − old contribution), so the series collapses to the final
// max(0, aᵢ−bᵢ) no matter how often it moved or in which direction.
//
// SAFE WHEN OFF. A standalone C1 with no adopted schedule tracks nothing, and
// drawdown reads as 0 — which yields S = Σa, the old over-counting denominator. That
// under-pays slightly and can never over-pay, so the degraded mode stays solvent.
//
// ORDER MATTERS: call this BEFORE appendHist. It reads aᵢ through histAt(acct,
// start), and at h == start the account's own new entry would redefine aᵢ mid-update.
func noteDrawdown(acct string, oldS, newS *big.Int, h uint64) {
	el := idx(kEpochLen)
	if el == 0 || !present(kGenesis) {
		return // no schedule adopted — nothing to track (see SAFE WHEN OFF above)
	}
	g := idx(kGenesis)
	if h < g {
		return // before epoch 0 exists
	}
	ep := (h - g) / el
	start := g + ep*el
	// A mutation ON the epoch's first block DEFINES aᵢ rather than moving away from
	// it, so its drawdown is 0 by construction — as was any earlier mutation in that
	// same block. Skipping keeps the telescoping baseline at 0.
	if h == start {
		return
	}
	a := histAt(acct, start)
	oldC, newC := drawdownOf(a, oldS), drawdownOf(a, newS)
	if oldC.Cmp(newC) == 0 {
		return
	}
	k := "dd|" + strconv.FormatUint(ep, 10)
	d := new(big.Int).Add(getBig(k), new(big.Int).Sub(newC, oldC))
	if d.Sign() < 0 {
		d = new(big.Int) // defensive: a sum of non-negatives can never be negative
	}
	setBig(k, d)
	// The accumulator is the one piece of C1's state that CANNOT be recomputed from
	// the outside: it telescopes across every mutation in the epoch, so an observer
	// replaying stake events alone would have to re-derive each account's
	// epoch-start baseline to get the same number. Emitting the delta and the
	// running value makes claimYield's denominator auditable.
	events.New("drawdown").
		U64("epoch", ep).
		Str("acct", acct).
		Big("delta", new(big.Int).Sub(newC, oldC)).
		Big("new_dd", d).
		Emit()
}

// drawdownOf = max(0, a-s): how far below its epoch-start level a stake now sits.
func drawdownOf(a, s *big.Int) *big.Int {
	d := new(big.Int).Sub(a, s)
	if d.Sign() < 0 {
		return new(big.Int)
	}
	return d
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

func ok() *string          { return str(`{"success":true}`) }
func str(s string) *string { return &s }

func splitColon(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// splitComma drops empty segments, matching C2/C3/C5/C6/C7. C1's version used to
// keep them, which was harmless only because its single caller happened to guard with
// `if a != ""`. Five identical-looking helpers with one behaving differently is a trap
// for whoever adds the second caller.
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

// field extracts a flat JSON value by key. Matches "name" only in KEY position
// (followed by ':') so a value equal to a field name can't be mis-parsed (M4).
// pickField reads a flat field from a contract-call result, nil-safe. Same shape as
// C2/C3/C7's helper of the same name, and it goes through field(), which matches only
// in KEY position.
func pickField(res *string, name string) string {
	if res == nil {
		return ""
	}
	return field(res, name)
}

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

// ---- STAKING YIELD -------------------------------------------------------
//
// Yield lives here rather than in its own contract because it never did anything
// except read this one: per-account stake at two heights, and the exact
// Σ min(aᵢ,bᵢ) denominator from the drawdown accumulator above. Merged, those become
// local reads instead of three cross-contract calls per claim, and the deployment
// costs one contract fee less.
//
// THE CUSTODY INVARIANT CHANGES SHAPE, so it is restated here and asserted on every
// outbound path. Separately, C1 held only staked principal and the rule was the
// simple `Σstake == balance`. This contract now holds three distinct pools:
//
//	balance  >=  total_staked                     (principal, owed to stakers)
//	           + Σ(y_funded − y_paid)             (yield pulled but not yet claimed)
//	           + whatever remains for the airdrop
//
// obligations() below is the first two terms — everything this contract already owes
// someone. Anything above it is unobligated, and only the airdrop may spend that. A
// bug that let one pool draw on another would pay rewards out of people's principal,
// which is the single worst thing this file could do, so the check is explicit and
// not inferred from balances.

// obligations = principal + yield funded but not yet claimed + principal that is
// unstaked, queued and still serving its cooldown.
//
// That third term is easy to forget precisely because unstake already decremented
// total_staked: the money looks accounted for and is not. It stays owed to the staker
// until claimUnstaked hands it over.
func obligations() *big.Int {
	o := new(big.Int).Set(getBig(kTotal))
	o.Add(o, getBig(kYieldOutstanding))
	return o.Add(o, getBig(kUnstakeOutstanding))
}

// pullFunding — pull this epoch's yield slice from C2 (permissionless).
//
//go:wasmexport pullFunding
func PullFunding(payload *string) *string {
	assertInit()
	if getStr(kBucket) == "" {
		sdk.Abort("no yield bucket adopted — call adoptSchedule({funder,bucket}) after C2 is initialised")
	}
	ep := mustEpoch(payload)
	// FUNDING IS FINAL ONCE ANYONE HAS BEEN PAID.
	//
	// y_funded accumulates (`+= got`), which reads as though several tranches per
	// epoch were expected — but claimYield divides by whatever it happens to hold at
	// the moment of the claim. A second tranche landing after the first claim would
	// permanently underpay everyone who claimed early against everyone who claimed
	// late, out of one pool, with no abort and no imbalance the contract could detect.
	//
	// That could not happen, but only because of how a DIFFERENT contract behaves:
	// C2.claimBucket deletes `owed|bucket|epoch` before transferring and can never
	// re-add it, since epochs only advance through its monotonic cfg_lastEpoch_v — so a
	// second pull aborts inside C2 and reverts. The distributor has the equivalent
	// protection locally (its pullFunding refuses once status is set); this contract
	// had none, leaving the guarantee to a neighbour's delete semantics and to the
	// `vsc.update_contract` upgrade path that the README already flags as untested.
	//
	// y_paid is written by claimYield and by sweepEmptyEpoch, so this also refuses to
	// fund an epoch that was already swept as unclaimable. An epoch nobody has claimed
	// yet is still freely toppable-up: nobody can have been shortchanged.
	if getBig("y_paid|"+ep).Sign() > 0 {
		sdk.Abort("epoch already paid out — funding is final once anyone has claimed")
	}
	res := sdk.ContractCall(getStr(kFunder), "claimBucket",
		`{"epoch":"`+ep+`","bucket":"`+getStr(kBucket)+`"}`, nil)
	got := parseBig(pickField(res, "claimed"))
	k := "y_funded|" + ep
	setBig(k, new(big.Int).Add(getBig(k), got))
	// Track the outstanding pool incrementally: recomputing it would mean summing
	// every epoch ever funded, which grows without bound.
	setBig(kYieldOutstanding, new(big.Int).Add(getBig(kYieldOutstanding), got))
	events.New("yield_funded").
		Str("epoch", ep).
		Big("received", got).
		Big("cumulative", getBig(k)).
		Big("outstanding", getBig(kYieldOutstanding)).
		Emit()
	return str(`{"funded":"` + getBig(k).String() + `"}`)
}

// claimYield — self-serve pro-rata yield for one epoch. Never expires.
//
// Named apart from claimUnstaked deliberately: one returns your own principal after
// its cooldown, the other pays a reward. Calling the wrong one should read wrong.
//
//go:wasmexport claimYield
func ClaimYield(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	c := mustCaller()
	validateAddr(c)
	ck := "y_claimed|" + ep + "|" + c
	if present(ck) {
		sdk.Abort("already claimed")
	}
	funded := getBig("y_funded|" + ep)
	if funded.Sign() <= 0 {
		sdk.Abort("epoch not funded yet")
	}
	set(ck, "1") // CEI: mark before any transfer
	if present("y_swept|" + ep) {
		sdk.Abort("epoch was swept as unclaimable")
	}
	hStart, hEnd := epochBounds(pu(ep))
	if hEnd >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	// min(stake at start, stake at end): only stake held for the WHOLE epoch earns,
	// which is what defeats a one-block flash stake around either boundary.
	stake := histAt(c, hStart)
	if e := histAt(c, hEnd); e.Cmp(stake) < 0 {
		stake = e
	}
	if stake.Sign() <= 0 {
		sdk.Abort("not staked across the whole epoch")
	}
	total := minStakeSumAt(pu(ep))
	if total.Sign() <= 0 {
		sdk.Abort("no stake held across the whole epoch")
	}
	payout := new(big.Int).Mul(funded, stake)
	payout.Div(payout, total)
	if payout.Sign() <= 0 {
		sdk.Abort("payout rounds to zero")
	}
	pk := "y_paid|" + ep
	setBig(pk, new(big.Int).Add(getBig(pk), payout))
	setBig(kYieldOutstanding, new(big.Int).Sub(getBig(kYieldOutstanding), payout))
	adapter.Transfer(asset(), c, payout)
	// stake_used and denominator are both emitted so the payout can be re-derived
	// off-chain: funded*stake/total is the whole rule, and an indexer that only
	// stored the amount could never show a claimant WHY they received it.
	events.New("yield_claim").
		Str("epoch", ep).
		Str("acct", c).
		Big("payout", payout).
		Big("stake_used", stake).
		Big("denominator", total).
		Big("funded", funded).
		Emit()
	return str(`{"claimed":"` + payout.String() + `"}`)
}

// sweepEmptyEpoch — guardian recovers an epoch NOBODY can ever claim.
//
// Fires only when the denominator is zero: no account held stake across that epoch,
// so every possible claim aborts and the funding would otherwise be locked forever.
// That condition is settled by history and cannot change, so no maturity wait is
// needed and no slow claimant can be stranded. An epoch with even one staker is never
// sweepable, at any height.
//
//go:wasmexport sweepEmptyEpoch
func SweepEmptyEpoch(payload *string) *string {
	assertInit()
	ep := mustEpoch(payload)
	if _, end := epochBounds(pu(ep)); end >= blockHeight() {
		sdk.Abort("epoch not fully elapsed")
	}
	if minStakeSumAt(pu(ep)).Sign() != 0 {
		sdk.Abort("epoch has stakers — their yield is claimable forever and is not sweepable")
	}
	funded := getBig("y_funded|" + ep)
	residual := new(big.Int).Sub(funded, getBig("y_paid|"+ep))
	if residual.Sign() <= 0 {
		sdk.Abort("nothing to sweep for epoch")
	}
	ak := "sweepempty:" + ep
	if !auth.Authorize(guardianCfg(), "grd", ak, ak, mustCaller(), reqAuths()) {
		return str(`{"swept":false}`)
	}
	set("y_swept|"+ep, "1")
	setBig("y_paid|"+ep, funded)
	setBig(kYieldOutstanding, new(big.Int).Sub(getBig(kYieldOutstanding), residual))
	adapter.Transfer(asset(), getStr(kTreasury), residual)
	events.New("yield_sweep").
		Str("epoch", ep).
		Big("residual", residual).
		Big("funded", funded).
		Str("to", getStr(kTreasury)).
		Emit()
	return str(`{"swept":"` + residual.String() + `"}`)
}

//go:wasmexport fundedOf
func FundedOf(payload *string) *string {
	assertInit()
	return str(`{"funded":"` + getBig("y_funded|"+mustEpoch(payload)).String() + `"}`)
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
		sdk.Abort("auth threshold must be a positive integer")
	}
	return auth.Config{Mode: mode, Authorities: splitComma(getStr("cfg_gAuth")), Threshold: thr}
}

// ---- MIGRATION AIRDROP ---------------------------------------------------
//
// A launch-time snapshot import. It runs a handful of times and is then dead, which
// is exactly why it does not deserve a contract of its own — but it does hold tokens,
// and it now holds them in the same balance as everybody's staked principal.
//
// So every batch is bounded twice over:
//
//  1. by `maxAirdrop`, the cap chosen at init, and
//  2. by the UNOBLIGATED balance — what is left after principal and unclaimed yield.
//
// The second is the one that matters here. Without it an over-sized airdrop would pay
// itself out of staked principal and the shortfall would only surface later, when a
// staker tried to withdraw and found the custody short. Checking it up front turns
// that into a refused batch.
//
// AIRDROP DIRECTLY INTO STAKE (`airdropStaked`): credits stake instead of
// transferring. Nothing leaves the contract — the float simply becomes principal — so
// recipients start earning yield immediately and cannot dump on day one without
// serving the unstake cooldown first. It routes through applyCredit, so checkpoints
// and the drawdown accumulator stay correct by construction, and the envelope is
// preserved exactly: the balance is unchanged while obligations rise by the amount
// credited, which is the float being reclassified rather than spent.

// unobligated = balance − (principal + unclaimed yield). Only the airdrop may spend it.
func unobligated() *big.Int {
	return new(big.Int).Sub(adapter.BalanceOfSelf(asset()), obligations())
}

// airdropBatch — owner-only, idempotent per batchId.
// {"batchId","entries":"acct:amount,acct:amount"}
//
//go:wasmexport airdropBatch
func AirdropBatch(payload *string) *string {
	assertInit()
	c := mustCaller()
	owner := sdk.GetEnvKey("contract.owner")
	if owner == nil || c != *owner {
		sdk.Abort("only owner")
	}
	auth.RequireActive(c, reqAuths()) // a posting key must not move funds
	if getStr(kMaxAir) == "" {
		sdk.Abort("airdrop not configured — set maxAirdrop at init")
	}
	batch := field(payload, "batchId")
	if batch == "" {
		sdk.Abort("batchId required")
	}
	validateAddr(batch)
	bk := "ad_done|" + batch
	if present(bk) {
		sdk.Abort("batch already applied")
	}
	set(bk, "1") // idempotent: the whole tx reverts if anything below fails

	staked := getStr(kAirStake) == "1"
	total := getBig(kAirTotal)
	sum := new(big.Int) // this batch only, for the envelope check
	type entry struct {
		acct string
		amt  *big.Int
	}
	list := []entry{}
	for _, e := range splitComma(field(payload, "entries")) {
		acct, amtStr := split2(e)
		if acct == "" {
			emitSkip(batch, e, "no account")
			continue
		}
		validateAddr(acct)
		// Recipients are credited on the ledger; a bare "alice" would credit a balance
		// no key can spend. Skipped like any other malformed entry.
		if !isLedgerAddr(acct) {
			emitSkip(batch, e, "not a ledger address")
			continue
		}
		amt := parseBig(amtStr)
		if amt.Sign() <= 0 {
			emitSkip(batch, e, "amount not positive")
			continue
		}
		list = append(list, entry{acct, amt})
		sum.Add(sum, amt)
		total.Add(total, amt)
	}
	if total.Cmp(parseBig(getStr(kMaxAir))) > 0 {
		sdk.Abort("exceeds maxAirdrop cap")
	}
	// THE ENVELOPE. Refuse rather than dip into principal or funded yield.
	if avail := unobligated(); sum.Cmp(avail) > 0 {
		sdk.Abort("batch exceeds the unobligated balance — an airdrop must never be paid " +
			"out of staked principal or funded yield; transfer more float in first")
	}
	for _, e := range list {
		if staked {
			applyCredit(e.acct, e.amt, "airdrop") // float becomes principal; nothing leaves
		} else {
			// No per-recipient event here on purpose. The liquid path IS a token
			// transfer, and the token contract already emits an indexed `transfer` log
			// for each one, so a second log would cost RC to restate what the indexer
			// records anyway. The staked path needs no per-recipient event either: it
			// goes through applyCredit, which emits `stake` with via="airdrop".
			adapter.Transfer(asset(), e.acct, e.amt)
		}
	}
	setBig(kAirTotal, total)
	events.New("airdrop").
		Str("batch_id", batch).
		Int("recipients", len(list)).
		Big("sum", sum).
		Big("cum_total", total).
		Bool("staked", staked).
		Emit()
	out := `{"airdropped_total":"` + total.String() + `"`
	if staked {
		out += `,"staked":true`
	}
	return str(out + `}`)
}

// emitSkip records an entry the batch silently dropped.
//
// This is the single most valuable log in C1. A skipped entry is a no-op inside a
// transaction that SUCCEEDS: the batch is marked applied, the totals look right,
// and the recipient is simply never paid. It leaves no trace in the return value
// and none in state, so before this it was undetectable on chain. Logs survive
// only successful transactions, which is exactly the case here.
func emitSkip(batch, raw, reason string) {
	events.New("skip").
		Str("scope", "airdrop").
		Str("batch_id", batch).
		Str("raw_entry", raw).
		Str("reason", reason).
		Emit()
}

// sweepUnobligated — owner recovers airdrop float that is left over.
//
// The old standalone airdrop contract had this, and dropping it in the merge would
// have stranded every unspent token permanently: float transferred in for a snapshot
// that turned out smaller than budgeted has no other way out.
//
// It can only ever move the UNOBLIGATED remainder — balance less staked principal and
// less yield that is funded but unclaimed — so it is the same envelope airdropBatch
// pays within, and it cannot reach anybody's stake or anybody's unclaimed reward.
// Owner-only with an active authority, to the treasury pinned at init.
//
//go:wasmexport sweepUnobligated
func SweepUnobligated(_ *string) *string {
	assertInit()
	c := mustCaller()
	ownerK := sdk.GetEnvKey("contract.owner")
	if ownerK == nil || c != *ownerK {
		sdk.Abort("only owner")
	}
	auth.RequireActive(c, reqAuths())
	to := getStr(kTreasury)
	if to == "" {
		sdk.Abort("no treasury configured — nothing may be swept without a pinned destination")
	}
	// Reserve whatever airdrop capacity is still unspent. Sweeping it would leave a
	// migration half-done with no float behind its remaining batches — the old
	// standalone airdrop contract protected exactly this, and dropping the reservation
	// would turn a mid-migration sweep into a silent failure of the batches that follow.
	amt := unobligated()
	if cap := getStr(kMaxAir); cap != "" {
		reserved := new(big.Int).Sub(parseBig(cap), getBig(kAirTotal))
		if reserved.Sign() > 0 {
			amt = new(big.Int).Sub(amt, reserved)
		}
	}
	if amt.Sign() <= 0 {
		sdk.Abort("nothing sweepable: the balance is committed to stake, unclaimed yield, " +
			"or the airdrop capacity still to be spent")
	}
	adapter.Transfer(asset(), to, amt)
	events.New("sweep_unobligated").
		Big("amount", amt).
		Str("to", to).
		Emit()
	return str(`{"swept":"` + amt.String() + `"}`)
}

//go:wasmexport airdropTotal
func AirdropTotal(_ *string) *string {
	assertInit()
	return str(`{"total":"` + getBig(kAirTotal).String() +
		`","unobligated":"` + unobligated().String() + `"}`)
}

// ---- shared helpers ------------------------------------------------------

func set(k, v string)    { sdk.StateSetObject(k, v) }
func pu(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }

// mustEpoch validates a canonical uint epoch string. Non-canonical forms ("00", or a
// value past uint64) used to alias onto epoch 0 and drain it.
func mustEpoch(payload *string) string {
	e := field(payload, "epoch")
	if e == "" || len(e) > 19 {
		sdk.Abort("epoch required/too long")
	}
	for i := 0; i < len(e); i++ {
		if e[i] < '0' || e[i] > '9' {
			sdk.Abort("epoch must be numeric")
		}
	}
	if len(e) > 1 && e[0] == '0' {
		sdk.Abort("epoch must be canonical")
	}
	if _, err := strconv.ParseUint(e, 10, 64); err != nil {
		sdk.Abort("epoch out of range")
	}
	return e
}

// split2 splits "a:b" on the LAST colon, so ledger addresses (which contain one) stay
// intact in the left half.
func split2(s string) (string, string) {
	last := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			last = i
		}
	}
	if last < 0 {
		return "", ""
	}
	return s[:last], s[last+1:]
}

func isLedgerAddr(a string) bool {
	return hasPrefix(a, "hive:") || hasPrefix(a, "contract:") ||
		hasPrefix(a, "did:") || hasPrefix(a, "system:")
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

func selfAddr() string {
	if id := sdk.GetEnvKey("contract.id"); id != nil {
		return "contract:" + *id
	}
	return ""
}
