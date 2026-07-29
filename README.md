# MAGI Tokenomics Framework

Reusable, deploy-per-project contract framework recreating Hive-Engine outpost/SCOT
tokenomics on MAGI/VSC. Any project deploys its own instances and configures the
economics entirely through `init` — **no numbers are hardcoded**.

New here? Read [`docs/how-it-works.md`](docs/how-it-works.md) first — it explains the
whole system in plain language. This file is the build-and-deploy reference.

## The pieces

| | contract | what it does |
|---|---|---|
| C0 | *(external)* `magi_token-contract` | the token itself. **Unmodified** — the framework drives it through its existing allowance interface, it does not fork it. |
| C1 | `c1-staking` | staking with height checkpoints, so any epoch's stake can be proven after the fact |
| C2 | `c2-emission` | draws each epoch's emission from an approved pool and splits it across named buckets. Needs **no** authority over the token. |
| C3 | `c3-distributor` | content/author rewards — accepts share lists, pays claims |
| C5 | `c5-lp` | LP rewards — same mechanism as C3, separate instance |
| C6 | `c6-migration` | one-off snapshot import / airdrop |
| C7 | `c7-yield` | staking yield — **trustless**, reads C1 directly, needs no reporter |
| — | `reporter/` | off-chain service that turns Hive activity into share lists ([README](reporter/README.md)) |
| — | `auth/`, `adapter/` | shared modules: multi-party authorisation, value-asset abstraction |

`sdk/` and `runtime/` are copied verbatim from `magi_token-contract` and **must never
be edited**. The Go module is named `magi_token` so those copied imports resolve.

## Build

```bash
cd /home/dockeruser/okinoko/magi-tokenomics
for c in c1-staking c2-emission c3-distributor c5-lp c6-migration c7-yield; do
  GOTOOLCHAIN=go1.25.3 tinygo build -gc=custom -scheduler=none -panic=trap \
    -no-debug -target=wasm-unknown -o $c/artifacts/main.wasm ./$c/contract
done
GOTOOLCHAIN=go1.25.3 go build -o reporter/bin/reporter ./reporter/cmd/reporter
```

`GOTOOLCHAIN=go1.25.3` is required — tinygo 0.39 does not support the go1.26 host.

## Test

```bash
GOTOOLCHAIN=go1.25.3 go test ./itest/ -count=1 -p 1     # 65 contract tests, real wasm engine
GOTOOLCHAIN=go1.25.3 go test ./reporter/... -count=1    # 75 reporter tests, no network
```

Devnet (docker multi-node, in the go-vsc-node clone — see [Devnet tests](#devnet-tests)):

```bash
# from a go-vsc-node checkout, after copying in testdata/devnet/*.go
go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/   # all 7 + reporter, ~30 min
```

---

# Deployment

## Order matters — three constraints that are easy to get wrong

1. **Deploy first, deposit second.** Each deploy costs 10 HBD of the deploying
   account's L1 balance. Depositing to the VSC ledger first leaves nothing to pay
   the deploy fee (`get_hbd_balance() >= -delta` assert). RC = ledger HBD + a
   10,000 free allowance, so deposit *after* deploying to cover the init calls.
2. **A contract must be initialised by the account that deployed it.** The deployer
   becomes `contract.owner`, and every `init` aborts unless `msg.caller == owner`.
3. **Fund C6 and stake into C1 BEFORE initialising C2.** Two separate reasons:
   - C6 pays airdrops out of *its own* balance, so it must be topped up while the
     deployer still owns the token — i.e. before `changeOwner` hands it to C2.
   - C2's `genesis` defaults to the block it is initialised at. C7 credits
     `min(stakeAt(epochStart), stakeAt(epochEnd))`, so stake that arrives *after*
     C2's init is zero at both boundaries of epoch 0 — that epoch's yield bucket
     ends up **funded but permanently unclaimable**.

## Recommended sequence

```
deploy all contracts                    # 10 HBD each, from the account that will init
deposit HBD                             # for RC
token.init                              # deployer owns it at this point
token.mint  {amount}                    # credits the OWNER (mint has no `to` field)
token.transfer -> contract:<C6>         # fund the airdrop float
token.transfer -> <source>              # move the emission pool to its holder
C6.init ; C6.airdropBatch               # seed holders
C1.init                                 # staking must be live before anyone stakes
  holders: token.approve -> C1 ; C1.stake
<source>: token.approve -> contract:<C2>  # let C2 draw the pool
C2.init  {"source": "<source>"}         # <- sets `genesis`; the clock starts here
C3.init / C5.init / C7.init             # they adopt C2's schedule automatically

# token ownership is now OPTIONAL: C2 does not need it. Hand it over only if you
# want C2's timelocked guardian pause/changeOwner passthrough; otherwise renounce
# it or give it to a DAO.
```

After this, each epoch runs: `C2.distributeEpoch` → `<dist>.pullFunding` →
`submitShares` pages → `finalizeEpoch` → holders `claim`. The reporter does all of
that for C3/C5; C7 needs only `pullFunding` and then holders claim.

## Init reference

Every contract takes `token` and `kind`. **`kind` must be `"0"`** (fungible);
editioned-NFT mode fails closed — see [NFT mode](#nft-mode-not-available). `tokenId`
must be empty. Amounts and heights are **decimal strings**, not numbers.

### Shared: authority blocks

`auth` appears wherever a role is configured (`guardian`, `veto`, `reporter`). Each
role takes three fields, e.g. `guardianMode` / `guardianAuth` / `guardianThreshold`:

| mode | meaning | `Auth` format |
|---|---|---|
| `"0"` Single | one account acts alone | `hive:alice` |
| `"1"` Cosigned | M-of-N, all signatures in **one** transaction | `hive:a,hive:b,hive:c` |
| `"2"` Attest | M-of-N, submitted **separately**; identical payloads accumulate until the threshold is met | `hive:a,hive:b,hive:c` |

`Threshold` is M. Attest is what lets several machines run the reporter
independently — determinism guarantees they produce byte-identical payloads.

> Guardian and veto authorities must be **disjoint**, and so must reporter and
> guardian. `init` rejects overlaps: one party that could both push a fraudulent
> report and refuse to cancel it defeats the challenge window.

### C1 — staking

```json
{"token":"vsc1...", "kind":"0", "tokenId":"",
 "cooldown":"86400", "epochLen":"28800", "allow":""}
```

| field | meaning |
|---|---|
| `cooldown` | blocks an unstake waits before it can be withdrawn. **Must exceed `epochLen`**, so a staker cannot exit an epoch they are still earning for. |
| `epochLen` | epoch length in blocks (3s/block: 1 day ≈ 28,800) |
| `allow` | comma-separated allowlist for `stakeFor` (staking on someone else's behalf). Empty = nobody. **Immutable after init.** |

### C2 — emission

```json
{"token":"vsc1...", "kind":"0", "tokenId":"",
 "source":"hive:treasury",
 "genesis":"", "epochLen":"28800",
 "baseAnnual":"1000000", "blocksPerYear":"10512000",
 "buckets":"content:contract:vsc1C3:5000,lp:contract:vsc1C5:3000,yield:contract:vsc1C7:2000",
 "dustBucket":"content", "maxCatch":"50", "timelock":"5760",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",
 "vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1"}
```

| field | meaning |
|---|---|
| `source` | the account holding the emission pool, which must `approve` this C2 to spend it. Defaults to the deploying owner. **C2 does not mint** — it draws each epoch from this allowance. |
| `genesis` | first block of epoch 0. **Omit it** to start at the init block — that is the normal case. A genesis in the past is rejected (it would force a huge catch-up). |
| `baseAnnual` / `blocksPerYear` | emission per epoch = `baseAnnual * epochLen / blocksPerYear`. **Flat** — the same every epoch, forever. |
| `buckets` | `name:target:weightBps` triples, comma-separated. Weights must sum to **10000**. Targets can be any contract or address — a distributor, a DAO, or a plain treasury. |
| `dustBucket` | which bucket receives the remainder from integer division. Must name a configured bucket. |
| `maxCatch` | max epochs one `distributeEpoch` poke will process (1..1000). Bounds the RC cost of a single call after downtime — see [`docs/rc-costs.md`](docs/rc-costs.md); with 3 buckets the free tier covers only ~8 epochs of catch-up. |
| `timelock` | blocks a queued guardian operation waits before it may execute |

**C2 pulls, it does not mint.** Each epoch it draws the full `emission` from `source`
using `token.transferFrom` — the same allowance mechanism C1 uses for staking —
bounded by `available = min(allowance(source→C2), balanceOf(source))`.

**Exhaustion pauses emission; it does not end it.** An epoch is funded in full or not
at all. When `available` cannot cover the next epoch the poke changes no state and
returns `{"distributed":"0","starved":true}`. Top the pool up and the next poke
resumes from exactly where it stopped, paying the epochs that elapsed meanwhile
(oldest first, `maxCatch` per poke). Nothing latches.

That is what makes **batched minting** work — see
[Minting the pool in batches](#minting-the-pool-in-batches) below.

Two consequences worth understanding before you deploy:

- **C2 never needs to own the token.** It holds no `mint`, `pause` or `changeOwner`
  authority, so token ownership can be renounced or given to a DAO independently.
  This removes what was the framework's single largest trust concession.
- **`totalSupply` no longer tracks emission.** The whole pool exists from the moment
  it is minted, so an explorer or market-cap tracker cannot read emission progress
  from supply — watch the `source` account's balance falling instead.
- **The schedule becomes revocable.** The pool holder can `decreaseAllowance` or move
  the tokens and emission stops. Under the old minting model the schedule was
  enforced by code. If you need the stronger guarantee, hold the pool in a timelocked
  multisig, or keep the source separate from any operational account.

Keep the `source` **separate from any participant account** — if the pool holder is
also a staker or earner, its balance mixes the undrawn pool with its own rewards and
accounting becomes hard to reason about.

See [`docs/halving-schedule.md`](docs/halving-schedule.md) for a decaying schedule,
which the pool model makes straightforward.

#### Minting the pool in batches

You do not have to mint the whole supply up front. Minting in tranches (25% at a
time, say) limits how much is pre-minted and revocable at any moment, at the cost of
having to top up before each tranche runs out.

```bash
# batch 1 — before handing ownership anywhere, since mint is owner-only
token.mint             {"amount":"25000000"}
token.approve          {"spender":"contract:<C2>","amount":"25000000"}

# ... epochs run until the pool cannot cover one, then pokes report starved ...

# batch 2 — increaseAllowance, NOT approve
token.mint             {"amount":"25000000"}
token.increaseAllowance {"spender":"contract:<C2>","amount":"25000000"}
```

Three things to get right:

- **Use `increaseAllowance`, not `approve`.** `approve` **overwrites** the allowance,
  so re-approving would silently discard whatever the previous batch had left
  unspent. `increaseAllowance` adds to it atomically.
- **Do not hand token ownership to C2 if you intend to mint again.** `mint` is
  owner-only, and C2's guardian passthrough permits only `pause`/`unpause`/
  `changeOwner` — *not* `mint`. After `changeOwner(C2)` the only route to batch 2 is
  to queue a `changeOwner` back through the timelock, mint, and hand it over again.
  Under the allowance model C2 needs no ownership at all, so the simplest answer is
  not to transfer it.
- **A gap is paid retroactively.** If the pool sits empty for a while, the refill
  causes every skipped epoch to be funded on subsequent pokes. Emission stays a
  strict function of elapsed time, but a long outage means a large catch-up drawing
  against the fresh batch — size the top-up with the backlog in mind.

A remainder smaller than one epoch is never paid out as a short epoch; it waits in
the pool for the next top-up. If you are minting a genuinely final batch and want the
pool to land empty, size it to a whole multiple of the per-epoch emission.

### C3 / C5 — distributors (content, LP)

```json
{"token":"vsc1...", "kind":"0", "tokenId":"",
 "funder":"vsc1C2", "window":"1200", "treasury":"hive:treasury",
 "reporterMode":"0","reporterAuth":"hive:reporter","reporterThreshold":"1",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}
```

| field | meaning |
|---|---|
| `funder` | the C2 instance this pulls its slice from |
| `window` | challenge window in blocks. After `finalizeEpoch`, the guardian has this long to cancel a bad report before claims open. Must be > 0. |
| `treasury` | fixed destination for `sweepUnallocated`. Pinned at init so a sweep cannot be redirected. |
| `reporter*` | who may push shares and finalize |

`genesis` and `epochLen` are **not** configured here — C3/C5 read them from the
funder at init and abort if C2 is not initialised yet. If you do supply them they are
cross-checked, and a mismatch aborts.

### C6 — migration / airdrop

```json
{"token":"vsc1...", "kind":"0", "tokenId":"", "maxAirdrop":"1000000"}
```

`maxAirdrop` caps the total this contract can ever distribute. Batches are
idempotent per `batchId`, so a retried batch cannot double-pay. **C6 must hold the
tokens it airdrops** — transfer them in before ownership moves to C2.

### C7 — staking yield (trustless)

```json
{"token":"vsc1...", "kind":"0", "tokenId":"",
 "funder":"vsc1C2", "stakeSource":"vsc1C1", "treasury":"hive:treasury",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}
```

No reporter — C7 reads stake from `stakeSource` itself and pays strictly pro-rata,
so nobody can influence the split. It adopts `genesis`/`epochLen` from the funder.

It credits `min(stakeAt(epochStart), stakeAt(epochEnd))`, so only stake held for the
**whole** epoch earns. That is deliberate: it defeats flash-staking a single block
around the snapshot.

## NFT mode (not available)

Every `init` rejects `kind:"1"` and any non-empty `tokenId`. Editioned-NFT mode is
unimplemented and fails closed rather than silently misbehaving: without a
fractional-carry accumulator, every pro-rata slice that truncates below one whole
edition strands the claimant permanently. See `adapter/adapter.go` for the full
rationale and what must be built first.

## Devnet tests

The docker multi-node tests are in [`testdata/devnet/`](testdata/devnet/) — see that
directory's README for how to run them. They are `package devnet` (they use
go-vsc-node's devnet harness internals), so they must be copied into a go-vsc-node
checkout to run; `testdata/` keeps them versioned here without breaking the build.

| test | covers | ~time |
|---|---|---|
| `magi_tokenomics_devnet_test.go` | C0+C2+C3 + 13 outsider attacks | 10 min |
| `magi_c5c6c7_devnet_test.go` | C1+C2+C5+C6+C7 + 11 outsider attacks | 18 min |
| `magi_reporter_devnet_test.go` | the real reporter binary driving C3 | 12 min |
| `magi_full_devnet_test.go` | **all 7 contracts + reporter**, then 14 staked-holder + 34 outsider attacks | 30 min |
| `magi_rogue_reporter_devnet_test.go` | the **trusted** reporter turning malicious | 22 min |
| `magi_multiepoch_devnet_test.go` | **operation over time**: catch-up, flat emission, stake history, unstake maturity | 30 min |
| `magi_refill_devnet_test.go` | **batched minting**: pool drained, refilled, backlog paid in full | 17 min |

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
| pure outsider | 34 | every privileged action on all 7 contracts is refused |
| staked holder | 14 | a real position (stake + shares + a collected claim) buys no extra power — double-claims, epoch aliasing, early withdrawal and sweeps all fail |
| **rogue reporter** | 3 phases | a fraudulent report is *accepted*, then **contained**: guardian veto, funding rolled forward and recovered, Attest quorum unbroken |
| hostile contract | 16 relayed | a contract cannot borrow its caller's authority |
| economic (in-process) | 4 | donation, Sybil split, mid-epoch exit/restake and allowance theft are all unprofitable or impossible |
| posting-key (in-process) | 3 | a posting key cannot satisfy an active-authority role |

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

C7 (staking yield) has none of this exposure: it reads C1 directly and pays strictly
pro-rata, so there is nothing to report and nothing to challenge.

### Two rules the contracts enforce, not convention

- **Reporter and guardian must be disjoint**, and so must guardian and veto. `init`
  rejects overlaps — one party able to both publish a fraudulent report and refuse to
  cancel it would make the challenge window meaningless.
- **The treasury is pinned at init.** Sweeps can only ever go there.

Staked funds sit in C1 and no role — owner, guardian or reporter — can touch them.

### Reading the adversarial suites

Two failure modes make an attack suite look green while proving nothing, and both are
guarded against explicitly:

- **An attack that aborts for the wrong reason.** The attacker is funded well past
  the 10,000-RC free tier (deposits polled until credited, then asserted — a recent
  run had the attacker at RC ~110,000 against ~48,000 needed), and every run logs
  `N/N attacks reached the chain`. A malformed payload that aborts on parsing proves
  the parser works, not the authority gate.
- **A vacuous assertion.** The pre-attack state snapshot is rejected if any baseline
  value is empty, since an empty baseline compares equal to anything.

One honest limit: C7's `sweepResidual` has a 1000-block maturity, so a devnet run
can never reach its authority check. Its guardian gate is proven in-process instead
(`itest/security_regression_test.go`), and the devnet attack is labelled accordingly.

## Status

All 6 contracts + reporter are complete, audited (crit/high/med resolved), and green:
65 contract tests, 75 reporter tests, and all five devnet suites — including the
full-system run and the adversarial suites below.

Not done: first real deployment, per-tenant config values, and the governance DAO
(developed separately).

**RC budgeting:** [`docs/rc-costs.md`](docs/rc-costs.md) — measured RC cost of every
function, the scaling curves for `submitShares` / `distributeEpoch` / `airdropBatch`,
and which roles need a funded account (reporter and deployer do; claimants and a
healthy keeper do not).

**Archived design:** [`docs/halving-schedule.md`](docs/halving-schedule.md) — the
decaying/halving emission schedule, removed from C2 as out of scope, with the exact
removed code and a verified restore checklist.
