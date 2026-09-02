// C2 — Emission Controller (plan §19.2, §21/F1 pull model).
//
// DRAWS a flat per-epoch emission from an approved pool (token.transferFrom against
// cfg_source) and records it against named buckets. It does NOT mint, and it does NOT
// need to own the token — it is only an approved spender, so token ownership can be
// renounced or handed to a DAO independently.
//
// distributeEpoch only pulls-to-self + records per-epoch bucket allocations (NO
// external calls to bucket targets) so a hostile/paused target cannot freeze emission
// (H-1). Each target PULLS its slice via claimBucket. Exhaustion PAUSES rather than
// ends the schedule: a poke that cannot fund the next epoch writes no state, and a
// refill resumes and pays the backlog. The guardian passthrough (pauseToken/
// changeTokenOwner) is auth-gated (all modes) + timelocked (R6/D6) and applies only
// if the token was in fact handed to C2.
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
	// A POSTING key must not configure a deployment.
	//
	// The runtime derives msg.caller from RequiredPostingAuths[0] when a transaction
	// carries no active auth (auth/auth.go:90), so comparing msg.caller to the owner
	// is satisfied by a posting-only transaction from the owner's account. Posting
	// authority is the half Hive users delegate to front-ends and think of as safe.
	// The fund-moving owner entrypoints already demand active auth; configuring the
	// deployment is not a lesser power, and init pins the emission schedule, the bucket table and both authority sets, once.
	//
	// UnlessContract, so a DAO or multisig CONTRACT owner still works: a contract
	// caller has no key at all and can never appear in required_auths, and exempting
	// it cannot reintroduce the posting-key risk it is guarding against.
	auth.RequireActiveUnlessContract(*caller, reqAuths())
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

	// buckets
	selfId := selfAddr() // ledger identity (MED-2)
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
		// Names must be UNIQUE: allocations are recorded per bucket NAME, so two
		// buckets sharing one would silently merge into a single owed record and the
		// second would never be separately claimable. Names are also what lets two
		// buckets point at the SAME contract — which is how one distributor instance
		// serves several reward channels.
		if present("bkt|" + name) {
			sdk.Abort("duplicate bucket name — names identify allocations and must be unique")
		}
		set("bkt|"+name, target)
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

	// Read the token's maxSupply. Under the pool model this NO LONGER BOUNDS EMISSION
	// — the approved pool does — so it is kept only as an init-time sanity check that
	// the configured token is a real, initialised token, and as an audit record of
	// what it was at deploy time. Nothing reads it afterwards.
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
	// C2 cannot be its own pool. The token refuses to approve self ("Cannot approve
	// self") and refuses a transferFrom where from == to, so the allowance would read
	// 0 forever and EVERY poke would return {"distributed":"0","starved":true} —
	// indistinguishable from a legitimately empty pool. Init is one-shot and the
	// economic config is immutable, so the deployment would be permanently dead with
	// no diagnosis. Every other address in this framework already gets a
	// self-reference check (bucket targets here, treasuries in C3/C5/C7); this was
	// the one field that did not.
	if src == selfId {
		sdk.Abort("source cannot be C2 itself: the token cannot approve self, so the pool would never fund")
	}
	set("cfg_source", src)
	set("cfg_maxSupply", pickField(info, "maxSupply"))
	if parseBig(getStr("cfg_maxSupply")).Sign() <= 0 {
		sdk.Abort("token maxSupply must be > 0 (getInfo) — M-A")
	}

	// NB: do NOT write cfg_lastEpoch here — its ABSENCE is the "no epoch yet"
	// sentinel so the first poke starts at epoch 0 (H1).
	set(kInit, "1")
	// Discovery anchor: this is a deploy-per-project framework, so every tenant is a
	// new contract id and a static address list in the indexer's mappings would need
	// editing per deployment. `c2_init` is what the mapping's discoverEvent matches,
	// the same way the DEX pools are discovered by pool_init.
	events.New("c2_init").
		Str("token", tok).
		Str("source", src).
		Str("owner", *owner).
		U64("genesis", gv).
		U64("epoch_len", el).
		Big("base_annual", parseBig(getStr("cfg_baseAnnual"))).
		U64("blocks_per_year", by).
		Str("dust_bucket", dust).
		U64("bucket_count", n).
		Str("buckets", f(payload, "buckets")).
		Big("emission_per_epoch", emissionForEpoch(0)).
		Emit()
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
		// Quoted, like every other return from this function. These two early-outs used
		// to emit a bare JSON NUMBER while every other path emitted a string, so one
		// field had two types depending on which branch produced it —
		// coverage_c2_test.go still tests for both spellings, which is how a consumer
		// discovers this: by tripping over it and working around it.
		return str(`{"distributed":"0"}`) // first epoch not elapsed yet
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
		return str(`{"distributed":"0"}`)
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
	// An instance initialised BEFORE the allowance model has no cfg_source, and init
	// can never be re-run. Without this the pull would silently target the empty
	// address and emission would simply stop, with every poke still reporting
	// success — the failure mode this codebase keeps producing. Fail loudly instead,
	// naming the cause, so it is diagnosable rather than mysterious.
	if source == "" {
		sdk.Abort("cfg_source is unset: this C2 predates the allowance model and cannot draw a pool — redeploy")
	}
	tok := getStr("cfg_token")
	allowed := parseBig(pickField(sdk.ContractCall(tok, "allowance",
		`{"owner":"`+source+`","spender":"`+self()+`"}`, nil), "allowance"))
	held := parseBig(pickField(sdk.ContractCall(tok, "balanceOf",
		`{"account":"`+source+`"}`, nil), "balance"))
	available := allowed
	if held.Cmp(available) < 0 {
		available = held
	}
	// Fully-elapsed epoch indices are 0..current-1 (H1).
	maxCatch := getU("cfg_maxCatch")
	if maxCatch == 0 {
		maxCatch = defaultMaxCatch
	}

	// The schedule parameters and the bucket table are immutable after init, so
	// they are read ONCE here rather than re-read inside the loop. A 50-epoch,
	// 3-bucket catch-up used to re-read epochLen/baseAnnual/blocksPerYear per epoch
	// (through emissionForEpoch) plus bucket_n, dustBucket and every bucket|i
	// definition per epoch — roughly 400 state reads of values that cannot change,
	// where 8 do.
	ecfg := loadEmissionCfg()

	// PLAN the catch-up before moving anything. Walk forward accumulating each
	// epoch's emission while the pool still covers the RUNNING TOTAL, which is the
	// same all-or-nothing test the per-epoch loop applied, just evaluated ahead of
	// the transfer:
	//
	// All-or-nothing per epoch. A partially funded epoch would still be marked done
	// and never revisited, so a later top-up could not repair it — and with a pool
	// refilled in batches that boundary recurs at EVERY batch, not just once at the
	// end. Waiting instead means every epoch is eventually paid in full. The cost is
	// that a remainder smaller than one epoch never emits; size the final approve to
	// land on a whole multiple if that matters.
	//
	// Accumulating rather than multiplying keeps this correct if a non-flat schedule
	// is ever reintroduced: forEpoch still decides each epoch's amount.
	plan := uint64(0)
	totalPull := new(big.Int)
	starved := false
	for ep := next; ep < current && plan < maxCatch; ep++ {
		want := new(big.Int).Add(totalPull, ecfg.forEpoch(ep))
		if want.Cmp(available) > 0 {
			starved = true
			break
		}
		totalPull = want
		plan++
	}

	if plan == 0 {
		out := `{"distributed":"0"`
		if starved {
			// A poke that could not fund even ONE epoch still has to say so. Without
			// this the pool running dry is INVISIBLE downstream: no `emit` rows appear,
			// and an operator watching the indexer cannot tell "the pool is empty" from
			// "the keeper died" — the two failures with completely different fixes.
			//
			// The idle path above (next >= current, nothing to do) stays silent on
			// purpose; nothing happened there. This one is an incident.
			out += `,"starved":true`
			events.New("poke").
				U64("from_epoch", next).
				U64("last_epoch", next).
				U64("epochs", 0).
				Big("pulled", new(big.Int)).
				Bool("starved", true).
				Emit()
		}
		return str(out + `}`)
	}

	// ONE transferFrom for the whole catch-up instead of one per epoch. Source,
	// destination and asset are identical on every iteration, so N cross-contract
	// calls only ever differed by amount — and that call is the dominant cost of a
	// poke (a 50-epoch catch-up measured 72,122 RC, of which the repeated pulls were
	// roughly 17,000 by the token's own per-transfer figure).
	//
	// Aborts if the source revoked the allowance or moved the tokens between our
	// snapshot and now; the whole poke then reverts and can simply be retried.
	if totalPull.Sign() > 0 {
		adapter.PullFrom(asset(), source, totalPull)
	}

	buckets, dust := loadBuckets()
	for i := uint64(0); i < plan; i++ {
		ep := next + i
		emission := ecfg.forEpoch(ep)
		if emission.Sign() > 0 {
			recordAllocations(ep, emission, buckets, dust)
		}
		events.New("emit").
			U64("epoch", ep).
			Big("emission", emission).
			Emit()
	}
	last = next + plan - 1
	setU("cfg_lastEpoch_v", last)
	set("cfg_lastEpoch", "1")

	events.New("poke").
		U64("from_epoch", next).
		U64("last_epoch", last).
		U64("epochs", plan).
		Big("pulled", totalPull).
		Bool("starved", starved).
		Emit()

	out := `{"distributed":"` + strconv.FormatUint(plan, 10) + `"`
	if starved {
		// NOT terminal — a refill resumes from here and pays the backlog.
		out += `,"starved":true`
	}
	return str(out + `}`)
}

// bucketDef is one bucket's immutable share of an epoch, read once per poke.
type bucketDef struct {
	name string
	wbps int
}

// loadBuckets reads the bucket table and the dust bucket name. Both are fixed at
// init, so a catch-up reads them once instead of once per epoch.
func loadBuckets() ([]bucketDef, string) {
	n := getU("bucket_n")
	out := make([]bucketDef, 0, n)
	for i := uint64(0); i < n; i++ {
		name, _, w := split3(getStr("bucket|" + strconv.FormatUint(i, 10)))
		wb, _ := strconv.Atoi(w)
		out = append(out, bucketDef{name: name, wbps: wb})
	}
	return out, getStr("cfg_dustBucket")
}

// recordAllocations splits `minted` across buckets (remainder→dustBucket) and
// records per-(target,epoch) owed amounts. No external calls (pull model).
// Takes the bucket table rather than reading it, so a catch-up loads it once.
func recordAllocations(epoch uint64, minted *big.Int, buckets []bucketDef, dust string) {
	distributed := new(big.Int)
	for _, b := range buckets {
		slice := new(big.Int).Mul(minted, big.NewInt(int64(b.wbps)))
		slice.Div(slice, big.NewInt(10000))
		distributed.Add(distributed, slice)
		addOwed(b.name, epoch, slice)
		if slice.Sign() > 0 {
			events.New("alloc").
				Str("bucket", b.name).
				U64("epoch", epoch).
				Big("amount", slice).
				Bool("is_dust", false).
				Emit()
		}
	}
	// remainder → dust bucket (init guarantees it names a configured bucket)
	rem := new(big.Int).Sub(minted, distributed)
	if rem.Sign() > 0 {
		addOwed(dust, epoch, rem)
		// Emitted separately from the dust bucket's own weighted slice above: they
		// land in the same owed record, and an indexer summing `alloc` rows per
		// (bucket, epoch) must see both parts or its total will not match `owed`.
		events.New("alloc").
			Str("bucket", dust).
			U64("epoch", epoch).
			Big("amount", rem).
			Bool("is_dust", true).
			Emit()
	}
}

// addOwed records an allocation against the bucket NAME rather than its target
// address. Keying by address collapses two buckets that share a target into one
// record, which is exactly the configuration a multi-channel distributor uses — one
// contract funded by a `content` bucket and an `lp` bucket at the same id.
func addOwed(name string, epoch uint64, amt *big.Int) {
	if amt.Sign() == 0 {
		return
	}
	k := "owed|" + name + "|" + strconv.FormatUint(epoch, 10)
	setBig(k, new(big.Int).Add(getBig(k), amt))
}

// claimBucket: a bucket target pulls its slice for a given epoch.
// {"epoch":"N","bucket":"name"} — msg.caller must be the target configured for that
// bucket. The name is required because one contract may be the target of SEVERAL
// buckets (a distributor serving both a content and an LP channel), so the caller
// alone does not identify which allocation is being pulled.
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
	bucket := f(payload, "bucket")
	if bucket == "" {
		sdk.Abort("bucket required")
	}
	// The caller must be the address this bucket was configured to pay. Without this
	// any contract could name someone else's bucket and drain its allocation.
	if getStr("bkt|"+bucket) != c {
		sdk.Abort("caller is not the target of that bucket")
	}
	k := "owed|" + bucket + "|" + strconv.FormatUint(ep, 10)
	amt := getBig(k)
	if amt.Sign() <= 0 {
		sdk.Abort("nothing owed for bucket/epoch")
	}
	// CEI: zero the owed record before the external transfer
	sdk.StateDeleteObject(k)
	adapter.Transfer(asset(), c, amt)
	events.New("bucket_claim").
		Str("bucket", bucket).
		U64("epoch", ep).
		Big("amount", amt).
		Str("target", c).
		Emit()
	return str(`{"claimed":"` + amt.String() + `"}`)
}

// ---- queries -------------------------------------------------------------

//go:wasmexport scheduleInfo
func ScheduleInfo(_ *string) *string {
	assertInit()
	return str(`{"genesis":"` + getStr("cfg_genesis") + `","epochLen":"` + getStr("cfg_epochLen") + `"}`)
}

// bucketTarget: {"bucket"} -> the address that bucket pays, "" if no such bucket.
//
// Exists so a distributor can verify at ITS init that the bucket funding it actually
// points back at it. Without that check a typo'd bucket name deploys cleanly and
// fails much later at the first pullFunding, by which time the epoch it was supposed
// to fund has already been scored.
//
//go:wasmexport bucketTarget
func BucketTarget(payload *string) *string {
	assertInit()
	return str(`{"target":"` + getStr("bkt|"+f(payload, "bucket")) + `"}`)
}

//go:wasmexport owedOf
func OwedOf(payload *string) *string {
	assertInit()
	return str(`{"owed":"` + getBig("owed|"+f(payload, "bucket")+"|"+f(payload, "epoch")).String() + `"}`)
}

//go:wasmexport emissionForEpochQ
func EmissionForEpochQ(payload *string) *string {
	assertInit()
	return str(`{"emission":"` + emissionForEpoch(pu(f(payload, "epoch"))).String() + `"}`)
}

// ---- emission math (R13: single big.Int division) ------------------------

// emissionCfg is the schedule, read once. Every field is immutable after init.
type emissionCfg struct {
	epochLen      uint64
	baseAnnual    *big.Int
	blocksPerYear uint64
}

func loadEmissionCfg() emissionCfg {
	return emissionCfg{
		epochLen:      getU("cfg_epochLen"),
		baseAnnual:    parseBig(getStr("cfg_baseAnnual")),
		blocksPerYear: getU("cfg_blocksPerYear"),
	}
}

// forEpoch is FLAT: every epoch emits the same amount.
//
// A decaying (halving) schedule was removed as out of scope. Emission pauses
// whenever the approved pool cannot cover a whole epoch, and resumes on refill;
// it has no permanent end state. The epoch index is therefore unused, but is
// kept in the signature so a future schedule can be reintroduced without
// touching callers — distributeEpoch accumulates per-epoch amounts rather than
// multiplying one of them, so a non-flat schedule needs no change there either.
func (c emissionCfg) forEpoch(_ uint64) *big.Int {
	// emission = baseAnnual * epochLen / blocksPerYear
	em := new(big.Int).Mul(c.baseAnnual, new(big.Int).SetUint64(c.epochLen))
	em.Div(em, new(big.Int).SetUint64(c.blocksPerYear))
	return em
}

// emissionForEpoch is the one-shot form, for init's sanity check and the query.
// The catch-up loop uses loadEmissionCfg directly so it reads state once.
func emissionForEpoch(ep uint64) *big.Int { return loadEmissionCfg().forEpoch(ep) }

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

func self() string { return selfAddr() }

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
