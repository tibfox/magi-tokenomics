# MAGI Tokenomics Framework

Reusable, deploy-per-project contract framework for community token economics on
MAGI/VSC: scheduled emission split across reward pools, with content, LP and staking
payouts that each holder claims for themselves. Any project deploys its own instances
and configures the economics entirely through `init` — **no numbers are hardcoded**.

Hive-Engine's tribes (SCOT/outposts) were the reference point for *what problem to
solve*, not a specification. This is an independent implementation and it does not
reproduce their behaviour — do not assume a rule carries over because it held there.
Where a number or a policy matters to you, read it here.

New here? Read [`docs/how-it-works.md`](docs/how-it-works.md) first — it explains the
whole system in plain language. This file is the build-and-deploy reference.

## The pieces

**Three contracts of ours, plus the token.** Each deploy and each update costs a real
fee, so roles that share a balance and a history share a contract.

| | contract | what it does |
|---|---|---|
| C0 | *(external)* `magi_token-contract` | the token itself. **Unmodified** — the framework drives it through its existing allowance interface, it does not fork it. |
| C1 | `c1-staking` | **staking + staking yield + the launch airdrop.** Staking keeps height checkpoints so any epoch's stake is provable after the fact; yield pays stakers pro-rata straight from that history, with no reporter; the airdrop imports a launch snapshot and can credit stake directly. |
| C2 | `c2-emission` | draws each epoch's emission from an approved pool and splits it across named buckets. Needs **no** authority over the token. |
| C3 | `c3-distributor` | reward **channels** — content, LP, or anything else a tenant adds. Each channel has its own funding bucket, share book, challenge window and reporter authority, so one deployed contract serves them all. |
| — | `reporter/` | off-chain service that turns Hive activity (or DEX liquidity history) into share lists ([README](reporter/README.md)) |
| — | `auth/`, `adapter/`, `events/` | shared modules: multi-party authorisation, value-asset abstraction, contract log emission |
| — | `indexer/` | drop-in mappings so magi-mongo-indexer picks up any deployment ([README](indexer/README.md)) |

**Why yield and the airdrop live in C1.** Yield never read anything except C1 — stake
at two heights, and the exact `Σ min(aᵢ,bᵢ)` denominator from its drawdown
accumulator. Merged, those are local reads instead of three cross-contract calls per
claim. The airdrop runs a handful of times at launch and is then dead, which is
exactly why it did not warrant a fee of its own.

That does mean **three pools share one balance**, so C1 holds to a stated envelope:

```
balance >= total_staked + (yield funded but unclaimed)
         + (unstaked, queued, still in cooldown) + airdrop float
```

Only the airdrop may spend the unobligated remainder, and it checks before paying.
Yield pays from its own funded pool; principal is only ever returned to the staker who
put it in.

**The third term is the one that was missing.** `unstake` drops `total_staked`
immediately — it must, or an account on its way out keeps earning weight — but moves
no tokens: they serve the cooldown in custody. Until that was counted, the envelope
reported a staker's queued principal as free float for the whole cooldown, and
`sweepUnobligated` would hand it to the treasury (measured: `{"swept":"1000"}`, then
the staker's own `claimUnstaked` failed on "Insufficient balance"). Tracked now as
`unstake_outstanding`, incremented at `unstake` and released at `claimUnstaked`.

`sdk/` and `runtime/` are copied verbatim from `magi_token-contract` and **must never
be edited**. The Go module is named `magi_token` so those copied imports resolve.

## Build

```bash
cd /home/dockeruser/okinoko/magi-tokenomics
for c in c1-staking c2-emission c3-distributor; do
  GOTOOLCHAIN=go1.25.3 tinygo build -gc=custom -scheduler=none -panic=trap \
    -no-debug -target=wasm-unknown -o $c/artifacts/main.wasm ./$c/contract
done
GOTOOLCHAIN=go1.25.3 go build -o reporter/bin/reporter ./reporter/cmd/reporter
```

`GOTOOLCHAIN=go1.25.3` is required — tinygo 0.39 does not support the go1.26 host.

## Test

```bash
GOTOOLCHAIN=go1.25.3 go test ./itest/ -count=1 -p 1     # 195 contract tests, real wasm engine
GOTOOLCHAIN=go1.25.3 go test ./reporter/... -count=1    # 174 reporter tests, no network
GOTOOLCHAIN=go1.25.3 go test ./indexer/... -count=1     # 13 indexer/proofsvc tests
```

The indexer needs its own line: it lives at `./indexer/proofsvc`, **not** under
`./reporter/...`, so the reporter command does not run it. `go test ./...` is not a
substitute — the contract packages are `//go:build gc.custom` and only compile under
tinygo, so a plain `./...` reports them as `[setup failed]` and buries the real result.

Devnet (docker multi-node, in the go-vsc-node clone — see [Devnet tests](#devnet-tests)):

```bash
# from a go-vsc-node checkout, after copying in testdata/devnet/*.go
go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/   # whole system + reporter, ~40 min
```

---

# Deployment

## Order matters — five constraints that are easy to get wrong

1. **Deploy first, deposit second.** Each deploy costs 10 HBD of the deploying
   account's L1 balance. Depositing to the VSC ledger first leaves nothing to pay
   the deploy fee (`get_hbd_balance() >= -delta` assert). RC capacity comes from that
   same ledger balance, so deposit *after* deploying to cover the init calls — see
   [`docs/rc-costs.md`](docs/rc-costs.md).
2. **A contract must be initialised by the account that deployed it.** The deployer
   becomes `contract.owner`, and every `init` aborts unless `msg.caller == owner`.
3. **Fund C1's airdrop float and stake into C1 BEFORE initialising C2.** Two reasons:
   - the airdrop pays out of C1's *own* balance, so the float must be transferred in
     while the deployer still owns the token — i.e. before `changeOwner` hands it to C2.
   - C2's `genesis` defaults to the block it is initialised at, and yield credits
     `min(stakeAt(epochStart), stakeAt(epochEnd))`, so stake that arrives *after*
     C2's init is zero at both boundaries of epoch 0 — that epoch's yield bucket
     ends up **funded but permanently unclaimable**.
4. **Call `C1.adoptSchedule` after `C2.init`.** It arms two things at once: the
   per-epoch drawdown accumulator behind the exact yield denominator, and the bucket
   C1 pulls yield funding from. Neither can be configured at C1's own init, because
   the deploy order puts C1 first (constraint 3) while C2's `genesis` and buckets do
   not exist yet. Owner-only and once. `pullFunding` refuses until it has run.
5. **Register a channel per reward stream** with `C3.addChannel`, after `C2.init` —
   verifying a channel's bucket means calling the funder. Channels are append-only,
   with one exception: `setPolicy`. An **attest-mode channel must declare a
   `policy`** digest (`reporter policy-digest`), because that is the mode where two
   reporters can disagree — see [Reporter policy](#reporter-policy-two-honest-reporters-must-never-disagree).

## Recommended sequence

```
deploy 4 contracts                      # token + C1 + C2 + C3. 10 HBD each.
deposit HBD                             # for RC capacity
token.init                              # deployer owns it at this point
token.mint  {amount}                    # credits the OWNER (mint has no `to` field)
token.transfer -> contract:<C1>         # fund the airdrop float
token.transfer -> <source>              # move the emission pool to its holder
C1.init                                 # staking + yield + airdrop, in one
C1.airdropBatch                         # seed holders (optionally straight into stake)
  holders: token.approve -> C1 ; C1.stake
<source>: token.approve -> contract:<C2>  # let C2 draw the pool
C2.init  {"source": "<source>"}         # <- sets `genesis`; the clock starts here
C1.adoptSchedule {"funder":"<C2>","bucket":"yield"}   # accumulator + yield funding
C3.init                                 # adopts C2's schedule automatically
C3.addChannel {"channel":"content","policy":"<reporter policy-digest>", ...}
C3.addChannel {"channel":"lp","policy":"<reporter policy-digest>", ...}
  # policy is REQUIRED for reporterMode 2 (attest), optional elsewhere

# token ownership is now OPTIONAL: C2 does not need it. Hand it over only if you
# want C2's timelocked guardian pause/changeOwner passthrough; otherwise renounce
# it or give it to a DAO.
```

After this, each epoch runs: `C2.distributeEpoch` → `C3.pullFunding` →
`submitShares` pages → `finalizeEpoch` → holders `claim`, once **per channel**. The
reporter does all of that for the channel it is configured against; C1's yield needs
only `pullFunding`, then stakers claim.

> **The LP channel is fed from the indexer, not the DEX.** Run a second reporter with
> `source.kind: "lp"` and an `indexer` block (`reporter init-config lp`). It replays
> `add_liq`/`rem_liq` events to reconstruct each provider's LP balance at a height,
> and credits `min(LP(epochStart), LP(epochEnd))`.
>
> The DEX cannot be read directly for this: it stores balances as current state
> (`lp-{address}`, total at `tlp`) with **no height checkpoints**, so a finished epoch
> cannot be priced from it, and paying against live balances would be
> flash-liquidity gameable — add just before the snapshot, remove just after. The
> `min(start, end)` rule mirrors the yield anti-flash-stake rule for the same reason.
>
> LP rewards therefore inherit a trust assumption on the indexer operator. It is not
> a new one: the reporter was already trusted to submit an honest list, the events
> derive from on-chain transactions, and Hasura is publicly queryable — so anyone can
> recompute independently and a guardian can still veto in the challenge window.
> Details in [`reporter/README.md`](reporter/README.md).

### C3 — the distributor

```json
{"token":"vsc1...", "kind":"0", "tokenId":"",
 "funder":"vsc1C2", "treasury":"hive:treasury",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}
```

| field | meaning |
|---|---|
| `funder` | the C2 instance channels pull their slices from |
| `treasury` | fixed destination for `sweepUnallocated`. Pinned at init so a sweep cannot be redirected. |
| `guardian*` | who may cancel a bad report, contract-wide |

`genesis` and `epochLen` are **not** configured here — they are read from the funder
at init, and a supplied mismatch aborts.

#### Channels

Everything per-reward-stream is registered afterwards, one call per stream:

```json
{"channel":"content", "bucket":"content", "window":"1200",
 "reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1",
 "role":"content"}
```

| field | meaning |
|---|---|
| `bucket` | the C2 bucket that funds this channel. Verified against the funder at registration, so a typo fails here rather than at the first `pullFunding` — by which time the epoch it should have funded has elapsed. One bucket may fund only one channel. |
| `window` | challenge window in blocks, per channel. Must be > 0 and ≤ 10 epochs. |
| `reporter*` | who may push shares and finalize **this channel**. A content reporter and an LP reporter are different services holding different keys; neither can write the other's book. |
| `role` | optional `content`/`lp` label a reporter cross-checks itself against |

Channels are **append-only** and owner-only. Re-pointing a live channel's bucket or
reporter would rewrite the rules under an epoch already in flight.

Every epoch call — `pullFunding`, `submitShares`, `finalizeEpoch`, `cancelEpoch`,
`claim`, `shareOf`, `sweepUnallocated` — takes a `channel`.

### C1 — staking, yield and the airdrop

```json
{"token":"vsc1...", "kind":"0", "tokenId":"", "cooldown":"57600", "epochLen":"28800",
 "allow":"", "treasury":"hive:treasury", "maxAirdrop":"1000000", "airdropStaked":"0",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}
```

Staking needs only `token`, `cooldown`, `epochLen` and `allow`. **Everything else is
optional, and absent means the capability is absent** rather than half-wired:

| field | enables |
|---|---|
| `treasury` + `guardian*` | `sweepEmptyEpoch` (an epoch nobody could ever claim) and `sweepUnobligated` (leftover airdrop float). Without them both refuse. |
| `maxAirdrop` | the airdrop, capped at that total. Without it `airdropBatch` refuses. |
| `airdropStaked` | `"1"` credits stake **directly** instead of transferring: nothing leaves the contract, recipients earn from the first epoch, and they must serve the unstake cooldown before selling. |

`cooldown` must exceed `epochLen` (R15), so a one-block stake cannot capture a whole
epoch of yield. That check is made against an operator-supplied `epochLen` and
re-verified against the real schedule at `adoptSchedule`.

**Then, after `C2.init`:**

```json
adoptSchedule {"funder":"vsc1C2", "bucket":"yield"}
```

Owner-only and once. It arms the drawdown accumulator (below) and the bucket yield is
funded from. `pullFunding` refuses until it has run.

#### Why the yield denominator needs an accumulator

Yield credits each staker `min(stakeAt(epochStart), stakeAt(epochEnd))`, so only stake
held for the **whole** epoch earns — that defeats flash-staking a single block around
either boundary. The matching denominator is therefore `Σ min(aᵢ,bᵢ)`, and no contract
can evaluate that directly: summing a per-account minimum means iterating every staker.

The obvious substitute, `min(Σa, Σb)`, is *larger* whenever stakers move in both
directions during an epoch, and dividing by too large a figure pays out less than
`funded` and strands the difference where nobody can claim it.

So C1 keeps one running number per epoch — the total by which accounts have fallen
below their epoch-start level — giving

```
Σ min(aᵢ,bᵢ) = totalStakedAtHeight(epochStart) − drawdown(epoch)
```

in O(1) per stake change. Updates telescope, so many moves inside one epoch collapse
to the final `max(0, aᵢ−bᵢ)`. The denominator is exact, payouts sum to `funded` less
truncation dust, and two things follow:

- **claims never expire** — content, LP and yield are identical in this respect;
- **`sweepEmptyEpoch` fires only when the denominator is zero** — nobody held stake
  across the epoch, so no claim can ever succeed and the funding would otherwise be
  locked forever. Settled by history, so it needs no maturity wait and cannot strand a
  slow claimant. An epoch with even one staker is never sweepable.

C3's channels do not need any of this: they are *handed* their denominator,
accumulating `totalShares` from the entries as they are submitted.

#### The airdrop, and the balance envelope

`maxAirdrop` caps the total ever distributed. Batches are idempotent per `batchId`.
The contract **must hold the float** — transfer it in before ownership moves to C2.

Because staked principal, funded-but-unclaimed yield and airdrop float now share one
balance, `airdropBatch` may only spend the **unobligated remainder** and checks that
before paying. Without it an oversized batch would pay itself out of stakers'
principal, surfacing only when someone tried to withdraw.

`sweepUnobligated` (owner-only, to the pinned treasury) recovers leftover float, and
reserves whatever airdrop capacity is still unspent so a mid-migration sweep cannot
starve the batches still to come.

## Devnet tests

The docker multi-node tests are in [`testdata/devnet/`](testdata/devnet/) — see that
directory's README for how to run them. They are `package devnet` (they use
go-vsc-node's devnet harness internals), so they must be copied into a go-vsc-node
checkout to run; `testdata/` keeps them versioned here without breaking the build.

| test | covers | measured |
|---|---|---|
| `magi_tokenomics_devnet_test.go` | C0+C2+C3 + 13 outsider attacks | 10 min |
| `magi_cosigned_devnet_test.go` | **Cosigned 2-of-2**: one authority applies nothing, two in ONE tx apply the page | 12 min |
| `magi_realbroadcast_devnet_test.go` | the reporter **signing and broadcasting its own** epoch, root included | 12 min |
| `magi_reporter_devnet_test.go` | the real reporter binary driving C3, claims proved from `reporter proof` | 12 min |
| `magi_refill_devnet_test.go` | **batched minting**: pool drained, refilled, backlog paid in full | 17 min |
| `magi_stake_lp_airdrop_devnet_test.go` | staking+yield+airdrop and an LP channel + outsider attacks | 17 min |
| `magi_scale_devnet_test.go` | **the Hive-Engine parity config at size**: 200 posts, 10k votes, 502 earners, 9 pages | 17 min |
| `magi_lp_multiepoch_devnet_test.go` | **LP rewards**: 3 epochs via the real reporter in `lp` mode, forfeiture proved from the book | 20 min |
| `magi_rogue_reporter_devnet_test.go` | the **trusted** reporter turning malicious; fraud contained, attest quorum held | 23 min |
| `magi_multiepoch_devnet_test.go` | **operation over time**: catch-up, flat emission, stake history, unstake maturity | 34 min |
| `magi_full_devnet_test.go` | **the token + all three contracts + reporter**, then 14 staked-holder + 32 outsider attacks | 39 min |

All eleven pass against the merkle share book. Times are measured, not estimated —
the whole set is roughly 3.5 hours and they cannot run concurrently (each stands up
its own five-node chain and they contend for disk).

They need `reporter/bin/reporter` built first, and take `MAGI_FRAMEWORK_DIR` /
`MAGI_TOKEN_WASM` to locate the artifacts. Each run builds a ~766MB docker image that
is **not** cleaned up automatically — remove `devnet-test-*` images between runs or
the disk fills.

## Security model and how it is verified

The threat model is deliberate: **the deployer is trusted** — they funded the token
and own the contracts. What must hold is that nobody *else* can move anything, and
that the one genuinely trusted runtime role (the reporter) is containable when it
misbehaves.

Every role has been turned against the system on a live devnet chain:

| attacker | attempts | what it establishes |
|---|---|---|
| pure outsider | 32 | every privileged action on the token and all three contracts is refused |
| staked holder | 14 | a real position (stake + shares + a collected claim) buys no extra power — double-claims, epoch aliasing, early withdrawal and sweeps all fail |
| **rogue reporter** | 3 phases | a fraudulent report is *accepted*, then **contained**: guardian veto, funding rolled forward and recovered, Attest quorum unbroken |
| hostile contract | 16 relayed | a contract cannot borrow its caller's authority |
| economic (in-process) | 4 | donation, Sybil split, mid-epoch exit/restake and allowance theft are all unprofitable or impossible |
| posting-key (in-process) | 3 | a posting key cannot satisfy an active-authority role |

### Reward-pool settings

The knobs a pool is configured with. Most live in the reporter's config; the staked
split is the exception, because only the distributor ever holds the tokens.

| setting | where | what it does |
|---|---|---|
| `source.tags` | reporter | up to 5 tags/communities to index. A post matching several is collected **once** |
| `source.excluded_tags` | reporter | drops a post carrying any of them, applied **after** the tag walk |
| `source.limit` | reporter | max posts per epoch |
| `source.exclude` | reporter | **accounts** that earn nothing (not tags) |
| `source.cashout_days` | reporter | how long a post collects votes before it pays. 0 = Hive's 7 |
| `source.ignore_declined_payout` | reporter | pay authors who declined their Hive payout. Default honours the decline |
| `source.disable_downvotes` | reporter | drop negative votes instead of letting them net off |
| `source.weight` | reporter | `hive_rshares` (inherits Hive's stake and mana) or `token_stake` |
| `source.vote_*` / `downvote_*` | reporter | the vote budget, **required** for `token_stake` |
| `shares.author_reward_bps` | reporter | author's cut; the rest goes to curators |
| `shares.author_curve` / `curation_curve` | reporter | exact rationals. Curation `1/2` favours early voters, `1/1` is order-neutral |
| `shares.muted` | reporter | accounts that earn nothing |
| `shares.min_share_bps` | reporter | drop earners below this share of the epoch — **the main cost lever** |
| `shares.app_tax` | reporter | skim from posts published outside a designated app |
| `stakedBps` + `stakeContract` | **C3 init** | how much of each claim arrives as stake |
| `baseAnnual` / `blocksPerYear` / `epochLen` | **C2 init** | emission rate; flat, no decay schedule |

Three of these deserve a warning rather than a row:

**`token_stake` needs its vote budget, which is why those four are required rather
than optional.** A staked balance is not consumed by being used, so with no budget one
account votes every post in an epoch at full weight and the curation curve stops
meaning anything. They are *refused* for `hive_rshares`, where the rshares already
carry Hive's own mana and a second budget would charge the same vote twice. The budget
is replayed per epoch, every voter starting full — SCOT carries mana forever, but
reporter-side state that two Attest machines could disagree about is worse than a
budget that resets.

**The app tax matches on self-declared metadata.** A client writes its own `app` into
`json_metadata`, so anyone posting via the API can claim any app they like. It shapes
the behaviour of ordinary users on ordinary front-ends and does nothing to anyone
motivated to avoid it.

**A staked claim costs 3.6× a liquid one** (2,967 RC against 828), because it adds an
`approve` and a cross-contract `stakeFor`. The claimant pays that, and a claimant may
hold no HBD at all — see [docs/rc-costs.md](docs/rc-costs.md).

### The share book is a merkle commitment

The distributor stores a **32-byte root** for each epoch, not one state entry per
earner. Writing a share book cost ~311 RC per account against ~1 RC per byte of log,
so at 500 earners the per-account writes were ~92% of the bill; a full page went from
**15,309 RC to 1,871**, and an epoch from ~127,500 to ~20,000.

What that costs you, and it is not nothing:

- **`share|<ch>|<ep>|<acct>` no longer exists.** Nothing reads what an account earned
  from chain state alone. `shareOf` returns the commitment, the denominator, the
  funding and the status — deliberately not a per-account figure, because returning
  zero would make a wallet display "you earned nothing" and be believed.
- **A claimant supplies their share and a proof.** `reporter proof -account X`
  recomputes the epoch from Hive and returns both, so a claim needs no indexer when
  one is unavailable — and can check the indexer's copy when one is.
- **The indexer holds the book.** The leaves are still logged, so it can serve
  proofs; the root is what proves its answer was not invented. If its copy does not
  rebuild to the committed root, it must not be served. That service is
  [`indexer/proofsvc`](indexer/proofsvc) — it rebuilds each epoch from the `shares`
  logs, checks the result against the committed root, and refuses rather than hand
  out proofs the contract would reject at the point of payment.

The leaf is `sha256(0x00 || "acct|share")` and binds account and amount together, and
the contract builds it from `msg.caller` rather than a payload field — so a stolen
proof is worthless and an inflated one does not verify.

**One property this removed, and it needed replacing — in both directions.**
`totalShares` used to be computed by the contract from the pages it stored, so
Σclaims ≤ funded held by construction. It is now declared by the reporter.

*Under*-declaring makes every payout too large — across epochs, since the token
balance is shared. An explicit per-epoch `paid` guard refuses at the cause rather than
letting a later claimant's transfer fail for reasons unrelated to them.

*Over*-declaring is the quieter one and overpays nobody: it shrinks every payout and
leaves the difference in `funded|<ch>|<ep>`, which nothing could reach. `cancelEpoch`
refuses a finalized epoch past its window — exactly when a residue becomes visible,
since you only learn what went unclaimed after claims have run — `sweepUnallocated`
only moves `unalloc|`, and nobody can claim twice. **Half an epoch could be lost to one
wrong number.** So `submitShares` accumulates `pagesum|<ch>|<ep>` and `finalizeEpoch`
refuses unless the declared total equals what the pages actually published. That also
catches a root committed with pages missing (leaves never logged, so no proof could
ever be built) and an entry the chain *skipped*, which used to dilute every other
earner by a slice nobody could claim. Cost: ~85 RC on a full page, under 5%.

An epoch stopped there is not stuck — status stays open, so the guardian's stale
rescue still recovers the funding, which beats finalizing into a residue nothing can
move.

### What the reporter can and cannot do

The reporter is the only component that is trusted at runtime, and it is worth being
precise about what that means. It is *supposed* to publish share lists, so **nothing
stops it publishing a false one** — the devnet suite asserts the fraud is accepted,
because containment is not the submission path. It cannot mint, move funds, change
roles, or pay itself. When it lies:

- a guardian cancels inside the challenge window;
- the funding rolls into `unallocated` — neither stolen nor stranded — and the
  guardian recovers it to the **treasury pinned at init**;
- the rogue claims nothing, then or later.

In **Attest** mode (M-of-N) a single rogue additionally cannot reach threshold,
cannot equivocate (one vote per authority per action, so backing a second payload is
refused), and cannot stop an honest majority committing a different payload.

Staking yield has none of this exposure: it reads the stake history directly and pays
pro-rata, so there is nothing to report and nothing to challenge.

### Two rules the contracts enforce, not convention

- **Reporter and guardian must be disjoint**, and so must guardian and veto. `init`
  rejects overlaps — one party able to both publish a fraudulent report and refuse to
  cancel it would make the challenge window meaningless.
- **The treasury is pinned at init.** Sweeps can only ever go there.

Staked funds sit in C1 and no role — owner, guardian or reporter — can touch them.

### Reporter policy: two honest reporters must never disagree

If two reporters score the same epoch differently, that is a bug, not an operational
condition to monitor. The framework treats it that way.

**The arithmetic was already safe.** `sharecore` proves the same input yields
byte-identical output (`TestDeterminism_ShuffledInputSameBytes`, `TestDeterminism_RepeatedRuns`),
reward curves use integer-only `big.Int` arithmetic so no float rounding can make two
machines differ in the last digit, and `hivesrc` sorts canonically *before*
truncating, so listing the same tags in a different order cannot change the result.

**What was not safe was the input.** `verifyChainConfig` compared three things
against the chain — genesis, epochLen, role. Twenty-odd other settings that decide
the numbers were local config compared against nothing: tags and exclusions, muted
accounts, `min_share_bps`, the author and curation curves, `cashout_days`, downvote
and declined-payout handling, the four vote-mana settings, the app tax, pagination.
Two honest reporters differing in one of them produced different books forever and
neither could tell. In attest mode that is not a wrong payout, it is a **deadlock**:
the tally is per payload hash and anti-equivocation gives each authority one vote per
action, so both burn their vote in a different bucket and the page never reaches
threshold.

**Now:** those settings hash to one digest (`reporter policy-digest`), the channel
declares it on chain, and every reporter compares its own before computing anything.
A divergent reporter **refuses to run and names the offending field** rather than
submitting. The contract enforces the same digest at `submitRoot` — the one call that
authorises money, since pages are only logged — as a backstop for a reporter that
skipped its own check.

That leaves exactly one way for two reporters to disagree: one deliberately running
patched code. Which is the case attest mode exists to contain, and does.

**The digest is pinned per epoch, not per channel.** `pullFunding` snapshots the
policy in force into `policy|<ch>|<ep>` when the epoch is funded, so governance can
`setPolicy` without rewriting an epoch already being reported, and any old epoch
stays re-derivable against the exact rules it was scored under. A backlog epoch
therefore needs the config it was funded under — the reporter says so by name rather
than failing at `submitRoot` after computing the whole book.

**One residual gap, stated plainly.** The digest covers config, not data sources.
Endpoints are deliberately excluded so two honest mirrors are not rejected — but the
*indexer* URL is not as safe as the others: magi-mongo-indexer can legitimately
assign the same event different heights across two instances (it falls back between a
transaction's L1 anchored height and its state-output height when the transaction_pool
lookup misses), which lands the event in a different epoch. **LP quorums must share an
indexer.** No digest computed in the reporter can fix that; the fix belongs upstream.

### Reading the adversarial suites

Two failure modes make an attack suite look green while proving nothing, and both are
guarded against explicitly:

- **An attack that aborts for the wrong reason.** The attacker's deposits are polled
  until credited and then asserted, so it runs with RC capacity well past what the
  sweep needs (~110,000 against ~48,000), and every run logs
  `N/N attacks reached the chain`. An attack that dies of insufficient RC proves
  nothing about authority, and neither does a malformed payload that aborts on
  parsing.
- **A vacuous assertion.** The pre-attack state snapshot is rejected if any baseline
  value is empty, since an empty baseline compares equal to anything.

One honest limit: `sweepEmptyEpoch` refuses any epoch that has stakers, and it
checks that before authority — so a devnet run, where stakers exist by construction,
cannot reach its authority gate. That gate is proven in-process instead, on a
genuinely stakerless epoch where a non-guardian moves nothing and the guardian
recovers the full amount (`itest/security_regression_test.go`). The devnet attack is
labelled accordingly rather than claiming more than it shows.

## Status

All three contracts + reporter are complete, audited, and green: **195 contract tests,
174 reporter tests, 13 indexer tests, and twelve devnet suites** — the full-system run, the
adversarial suites, multi-epoch operation, batched refills, LP rewards, the guardian
token-op passthrough, full-scale distribution, and the in-place upgrade path.

Every state transition is emitted as a contract log in the format
`magi-mongo-indexer` ingests, so a deployment is queryable over GraphQL without
touching contract state — see [`indexer/`](indexer/). Watch the two `skip` tables in
particular: they record entries a contract dropped inside a transaction that
succeeded, which is the only loss the chain cannot otherwise show you.

**Not done, stated plainly:**

- **No real deployment yet.** Everything here is devnet-verified only.
- ~~**Scale is verified in-process, not on devnet.**~~ **Now verified on devnet.**
  `TestDevnetMagiScale` runs the Hive-Engine parity configuration at full size over a
  real five-node chain: 200 posts, 10,000 votes, **502 earners across 9 pages**, the
  root committed and every claim proved against it. The chain's `totalShares` equals
  the reporter's exactly (208,552,153,997,506,000), and the staked half of each payout
  lands as real stake through `stakeFor` while the other half goes out liquid. The
  in-process `TestCovDist_FiveHundredEarnersAcrossNinePages` still covers the
  distribution arithmetic — 99,748 of 100,000 distributed, the remainder under one unit
  per earner, truncation dust rather than a leak. `docs/rc-costs.md` has the measured
  per-entry curve.
- ~~**`vsc.update_contract` (the in-place upgrade path) is untested.**~~ **Now
  exercised.** `TestDevnetMagiUpgrade` deploys a stand-in that initialises the way a
  pre-allowance C2 did — a live schedule, no `cfg_source` — swaps the current C2 over
  it with a real `vsc.update_contract`, and asserts the upgraded instance records NO
  epoch. That is the observable form of the abort: had it pulled from the empty
  address and reported success, `cfg_lastEpoch` would be set. The fixture
  (`testdata/fixtures/c2-preallowance`) reproduces the state shape only; it is not a
  copy of the old contract, and the code installed over it is the real one.
- **Cosigned mode 1 is proven at the contract layer, not the key layer.** Verified on
  devnet by `TestDevnetMagiCosigned` — one authority applies nothing, two in a single
  transaction apply the page — but the devnet's
  accounts share an active authority, so ONE signature satisfies a two-account
  `required_auths` list. That proves the CONTRACT's threshold logic — which is what
  mode 1 implements — but not Hive's aggregation of genuinely distinct keys. Deferred to testnet
  deliberately: hivego's transaction type and broadcast are unexported and the
  devnet harness signs with a single WIF, so a two-key transaction cannot be
  assembled without patching an external repo. Testnet has real accounts with
  real distinct keys, which is where this belongs anyway.
- ~~**Attest mode keeps the payload of a vote that never reaches its threshold.**~~
  **Fixed.** There were two leaks, not one. A committing round deleted only the
  WINNING payload, so any rival page stayed forever — which is what happens every
  time a rogue reporter attests a fraudulent page and the honest majority commits a
  different one. And a round that never committed had nothing able to remove it at
  all. `auth` now tracks the hashes an action has seen and releases them together on
  commit, and `c3.releaseStaleAttest` frees an abandoned round after
  `staleAttestBlocks` (~24h). That call is permissionless on purpose: a committed
  action is refused and a live round is protected, so there is nothing to gain by
  it — whereas restricting it to the authorities would let one losing a vote clear
  the tally and re-run it.

  It is storage growth, not a vulnerability: bounded by authorities × distinct actions,
  paid for in the attester's own RC, and it blocks nothing. Note that transaction
  revert does **not** clean it up, because a partial attestation is a SUCCESSFUL
  transaction — that persistence is the mechanism, since an async M-of-N vote has to
  survive until the next reporter arrives in a later transaction. (Revert does cover
  the *failed* paths: a double-attest aborts after the canon write and unwinds it.)

  The blob exists only because `auth.Hash` is a 128-bit double-FNV and is documented as
  not collision-resistant, so the code pins the first attestation's exact bytes and
  rejects any mismatch. A collision-resistant digest would make the byte-compare
  unnecessary and take the record from 4,096 bytes to 32 — which is the real fix, and
  it means pulling a proper hash into a tinygo wasm contract.

  **It is also an RC argument, and that turns out to be the stronger one.** The host
  prices contract state by the byte (`WRITE_IO_GAS_RC_COST = 19`), so holding the
  payload is the dominant cost of attest mode, not the voting: a 60-entry page costs
  **30,846 RC to attest against ~1,871 RC in single mode**, a 16× penalty, and 3× the
  10,000 free tier — so an unfunded attester cannot hold a full page at all. A 32-byte
  record would cut that to ~1,700. The saving is smaller in steady state (~23%),
  because the release added above returns the space below the contract's high-water
  mark, where bytes are charged 19× less; see
  [`docs/rc-costs.md`](docs/rc-costs.md#attest-mode--the-payload-record-dominates-and-it-is-priced-by-the-byte)
  for the measured curve. Still not done, but the reason to do it is now measured
  rather than assumed.
- **`TestDevnetMagiUpgrade` cannot be run from a stock go-vsc-node checkout.** It
  needs `Devnet.UpdateContract`, `ContractUpdateOpts` and `Devnet.PendingUpdates`,
  which live on that repo's `feat/contract-update-timelock` branch, not on main.
  Against a checkout without them the whole `devnet` package fails to compile, so the
  file has to be moved aside to run any of the others. The result above stands — it
  was exercised on a checkout that had them — but it is not reproducible today
  without that branch, and saying so is the difference between a verified claim and
  an unfalsifiable one.
- ~~**Seven audit findings remain open.**~~ **Two remain, both low and neither
  fund-loss, and both are now closed in code.** `submitRoot` had no on-chain progress
  marker, so a resume re-broadcast a call the contract must abort — `chainApplied` now
  reads `root|<ch>|<ep>`, which is safe as a presence check because the resume guard
  has already refused any run whose plan root differs from the committed one. And
  `shares.muted`/`source.exclude` silently did nothing without a `hive:` prefix, since
  both are exact-match sets probed with a domain-prefixed account; `Config.Validate`
  now refuses a bare name rather than normalising it, matching
  `shares.app_tax.beneficiary`. Both guards are mutation-tested, and the consumers are
  pinned in their own packages so the validator cannot outlive the rule it enforces.
- **Three of those seven were refuted, not fixed.** Re-reading the audit journal (see
  below) turned up verdicts that already disposed of them. `executeTokenOp`'s
  single-veto execution is real but is not the veto's problem: the same bare membership
  check already lets any one of N guardians fire a matured op, execution moves nothing
  in the ledger, and `cancelTokenOp` has no height gate, so the coalition's block stays
  live until the instant of execution and can never hold an `unpause`. Bucket-name
  truncation needs a payload that is not valid JSON, and both ingest paths hand the
  contract a JSON marshaller's output. The epoch-boundary gap is real in the code and
  unreachable in the data: Hive block times sit on a three-second slot grid and
  `payout_at = created + 604800s` stays on it, so the uncovered interval holds only
  seconds that no payout can occupy. It becomes representable only when Hive misses the
  boundary slot *and* a scored post lands on precisely that second — order one dropped
  post per 140 years. **Deliberately not fixed:** the one-line correction changes every
  epoch's boundary and so every root, and a fleet that upgraded unevenly would have two
  attesters computing different books — a strictly worse failure than the one being
  closed. It belongs with a policy-version bump, not a cleanup sweep. The roles table
  in [`docs/how-it-works.md`](docs/how-it-works.md) now states the execute semantics.
- ~~**About nineteen findings from the audit were never adjudicated.**~~ **That was
  wrong, and the correction is worth more than the claim.** The run's `journal.jsonl`
  holds 25 completed agents: 6 finders producing 28 raw findings that deduplicate to 19
  distinct issues, and **19 verifiers whose verdicts did land**. After dedup exactly one
  issue had neither a verdict nor a fix — the `muted`/`exclude` prefix above, which was
  then verified directly against the source rather than taken from the journal. What
  the classifier cost was the synthesis step, not the adjudication, so the findings
  looked unadjudicated only because nothing had collated them.
- Per-tenant config values and the governance DAO are out of scope here.

### Two suites worth knowing about

**`magi_cosigned_devnet_test.go`** — a 2-of-2 C3 rejects a single authority and applies
the page when both sign one transaction, so all three auth modes run on a chain. It
builds the operation inline via `hivego` rather than through the harness's single-auth
`CallContract`.

**`magi_realbroadcast_devnet_test.go`** — the only suite where the harness does not
broadcast. `reporter run -broadcast` runs against a live devnet, so the reporter builds
the envelope, signs with an active key and submits every call itself. It covers the
whole signing path that no in-harness test can reach, because the harness supplies the
transaction envelope those tests never exercise.

It runs in **LP mode** specifically: the reporter uses ONE endpoint list for both reads
and broadcasts, so a fixture endpoint cannot also accept transactions. LP reads come
from the indexer and touch Hive only for the head block, which the devnet's real node
answers on the same endpoint it accepts broadcasts on. Content mode has no such split.

**RC budgeting:** [`docs/rc-costs.md`](docs/rc-costs.md) — measured RC cost of every
function, the scaling curves for `submitShares` / `distributeEpoch` / `airdropBatch`,
and which roles need a funded account (reporter and deployer do; claimants and a
healthy keeper do not).

**Emission is flat** — the same amount every epoch, at `baseAnnual * epochLen /
blocksPerYear`. There is no decaying or halving schedule; if you want one, customize
the contract.
