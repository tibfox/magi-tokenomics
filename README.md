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
| C5 | `c5-lp` | LP rewards — same mechanism as C3, separate instance. Fed by the reporter in `source.kind: "lp"` mode, which replays the indexer's liquidity events. |
| C6 | `c6-migration` | one-off snapshot import / airdrop |
| C7 | `c7-yield` | staking yield — **trustless**, reads C1 directly, needs no reporter. Claims **expire** at `max(epochLen×10, 1000)` blocks past funding; C3/C5 claims never do. |
| — | `reporter/` | off-chain service that turns Hive activity (or DEX liquidity history) into share lists ([README](reporter/README.md)) |
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
GOTOOLCHAIN=go1.25.3 go test ./itest/ -count=1 -p 1     # 92 contract tests, real wasm engine
GOTOOLCHAIN=go1.25.3 go test ./reporter/... -count=1    # 120 reporter tests, no network
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
that **for C3 only**; C7 needs just `pullFunding` and then holders claim.

> **C5 is fed from the indexer, not the DEX.** Run a second reporter instance with
> `source.kind: "lp"` and an `indexer` block (`reporter init-config lp`). It replays
> `add_liq`/`rem_liq` events to reconstruct each provider's LP balance at a height,
> and credits `min(LP(epochStart), LP(epochEnd))`.
>
> The DEX cannot be read directly for this: it stores balances as current state
> (`lp-{address}`, total at `tlp`) with **no height checkpoints**, so a finished epoch
> cannot be priced from it, and paying against live balances would be
> flash-liquidity gameable — add just before the snapshot, remove just after. The
> `min(start, end)` rule mirrors C7's anti-flash-stake rule for exactly that reason.
>
> LP rewards therefore inherit a trust assumption on the indexer operator. It is not
> a new one: the reporter was already trusted to submit an honest list, the events
> derive from on-chain transactions, and Hasura is publicly queryable — so anyone can
> recompute independently and a guardian can still veto in the challenge window.
> Details in [`reporter/README.md`](reporter/README.md).

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
{"token":"vsc1...", "kind":"0", "tokenId":"", "maxAirdrop":"1000000",
 "treasury":"hive:treasury"}
```

`maxAirdrop` caps the total this contract can ever distribute. Batches are
idempotent per `batchId`, so a retried batch cannot double-pay. **C6 must hold the
tokens it airdrops** — transfer them in before ownership moves to C2.

`treasury` is **optional** and pins the destination for `sweepResidual`, which moves
only `balance - (maxAirdrop - airdropped)`: the excess that no remaining airdrop
capacity could ever pay. It can never touch tokens a pending batch needs.

Decide deliberately. Omitting it means any excess — tokens above the cap, tokens the
snapshot did not need, and entries the ledger-address filter skipped — is **locked in
the contract permanently**. Setting it gives the owner an exit, which is also a
clawback capability; a launch that wants the stronger "nothing can ever be reclaimed"
promise should leave it unset.

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
| `magi_lp_multiepoch_devnet_test.go` | **LP rewards**: 3 epochs via the real reporter in `lp` mode | 24 min |

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

All 6 contracts + reporter are complete, audited, and green: **92 contract tests, 120
reporter tests, and ten devnet suites** — the full-system run, the adversarial
suites, multi-epoch operation, batched refills, LP rewards, and the guardian token-op
passthrough.

**Not done, stated plainly:**

- **No real deployment yet.** Everything here is devnet-verified only.
- **Scale is verified in-process, not on devnet.** A 500-earner epoch across 9 pages
  is covered by `TestCovDist_FiveHundredEarnersAcrossNinePages`: totalShares
  accumulates exactly, all 500 claims pay, and 99,748 of 100,000 is distributed with
  the remainder under one unit per earner — truncation dust, not a leak. What is still
  untested is that shape over a real multi-node chain, where per-transaction RC and
  block inclusion apply; `docs/rc-costs.md` has the measured per-entry curve.
- **`vsc.update_contract` (the in-place upgrade path) is untested.** C2 aborts loudly
  if upgraded from a pre-allowance deployment rather than silently starving, but that
  abort has itself never been exercised against a real code swap.
- **Cosigned mode 1 is proven at the contract layer, not the key layer.** The devnet's
  accounts share an active authority, so ONE signature satisfies a two-account
  `required_auths` list. That proves the CONTRACT's threshold logic — which is what
  mode 1 implements — but not Hive's aggregation of genuinely distinct keys.
- Per-tenant config values and the governance DAO are out of scope here.

### What closing the last two gaps found

Both had been written off as untestable. Each was closable, and one was hiding real
bugs — which is the argument for not accepting "can't be tested" as a resting state.

**Cosigned auth on devnet** (`magi_cosigned_devnet_test.go`) — a 2-of-2 C3 rejects a
single authority and applies the page when both sign one transaction. It was reachable
because the test file can build the operation inline via `hivego` rather than through
the harness's single-auth `CallContract`. All three auth modes now run on a chain.

**The reporter's real signing path** (`magi_realbroadcast_devnet_test.go`) — runs
`reporter run -broadcast` against a live devnet, so the reporter builds the envelope,
signs with an active key and submits every call itself. This found **two bugs that
made the submission path completely non-functional**:

- `required_posting_auths` was passed as nil, so Hive rejected every transaction with
  `Bad Cast: Invalid cast from null_type to Array`.
- there was no configurable Hive chain id, so every signature was made over mainnet's.
  On any other chain it recovered to the wrong key and the node reported a misleading
  `missing required active authority` — a chain mismatch wearing a permissions error's
  clothing.

Both were invisible beforehand because the devnet harness supplied those fields itself,
so every in-harness test passed while the shipped code could not broadcast at all.

It works in **LP mode** specifically. Content mode needs Hive post data from a fixture
server, and the reporter uses ONE endpoint list for both reads and broadcasts, so a
fixture endpoint cannot also accept transactions. LP mode reads from the indexer and
touches Hive only for the head block — which the devnet's real node answers on the same
endpoint it accepts broadcasts on.

**RC budgeting:** [`docs/rc-costs.md`](docs/rc-costs.md) — measured RC cost of every
function, the scaling curves for `submitShares` / `distributeEpoch` / `airdropBatch`,
and which roles need a funded account (reporter and deployer do; claimants and a
healthy keeper do not).

**Archived design:** [`docs/halving-schedule.md`](docs/halving-schedule.md) — the
decaying/halving emission schedule, removed from C2 as out of scope, with the exact
removed code and a verified restore checklist.
