# MAGI Tokenomics Framework

Reusable, deploy-per-project contract framework recreating Hive-Engine outpost/SCOT
tokenomics on MAGI/VSC. Any project deploys its own instances and configures the
economics entirely through `init` — **no numbers are hardcoded**.

New here? Read [`docs/how-it-works.md`](docs/how-it-works.md) first — it explains the
whole system in plain language. This file is the build-and-deploy reference.

## The pieces

| | contract | what it does |
|---|---|---|
| C0 | *(external)* `magi_token-contract` | the token itself. **Unmodified** — the framework takes ownership of it, it does not fork it. |
| C1 | `c1-staking` | staking with height checkpoints, so any epoch's stake can be proven after the fact |
| C2 | `c2-emission` | mints each epoch's emission and splits it across named buckets. Becomes the token's owner. |
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
go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/   # all 7 + reporter, ~23 min
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
C6.init ; C6.airdropBatch               # seed holders
C1.init                                 # staking must be live before anyone stakes
  holders: token.approve -> C1 ; C1.stake
token.changeOwner -> contract:<C2>      # hand the token over
C2.init                                 # <- sets `genesis`; the emission clock starts here
C3.init / C5.init / C7.init             # they adopt C2's schedule automatically
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
 "genesis":"", "epochLen":"28800",
 "baseAnnual":"1000000", "blocksPerYear":"10512000",
 "buckets":"content:contract:vsc1C3:5000,lp:contract:vsc1C5:3000,yield:contract:vsc1C7:2000",
 "dustBucket":"content", "maxCatch":"50", "timelock":"5760",
 "guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1",
 "vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1"}
```

| field | meaning |
|---|---|
| `genesis` | first block of epoch 0. **Omit it** to start at the init block — that is the normal case. A genesis in the past is rejected (it would force a huge catch-up). |
| `baseAnnual` / `blocksPerYear` | emission per epoch = `baseAnnual * epochLen / blocksPerYear`. **Flat** — the same every epoch, forever. |
| `buckets` | `name:target:weightBps` triples, comma-separated. Weights must sum to **10000**. Targets can be any contract or address — a distributor, a DAO, or a plain treasury. |
| `dustBucket` | which bucket receives the remainder from integer division. Must name a configured bucket. |
| `maxCatch` | max epochs one `distributeEpoch` poke will process (1..1000). Bounds the RC cost of a single call after downtime. |
| `timelock` | blocks a queued guardian operation waits before it may execute |

Emission stops only when `maxSupply` headroom runs out: the final epoch mints the
remainder and latches `terminal`, after which pokes are permanent no-ops. There is
no time-based cap — see [`docs/halving-schedule.md`](docs/halving-schedule.md) if you
need a decaying schedule.

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

| test | covers |
|---|---|
| `magi_tokenomics_devnet_test.go` | C0+C2+C3 + outsider attacks |
| `magi_c5c6c7_devnet_test.go` | C1+C2+C5+C6+C7 + outsider attacks |
| `magi_reporter_devnet_test.go` | the real reporter binary driving C3 |
| `magi_full_devnet_test.go` | **all 7 contracts + reporter in one run** |

They need `reporter/bin/reporter` built first, and take `MAGI_FRAMEWORK_DIR` /
`MAGI_TOKEN_WASM` to locate the artifacts. Each run builds a ~766MB docker image that
is **not** cleaned up automatically — remove `devnet-test-*` images between runs or
the disk fills.

## Status

All 6 contracts + reporter are complete, audited (crit/high/med resolved), and green:
65 contract tests, 75 reporter tests, and all four devnet suites including the
full-system run.

Not done: first real deployment, per-tenant config values, and the governance DAO
(developed separately).

**Archived design:** [`docs/halving-schedule.md`](docs/halving-schedule.md) — the
decaying/halving emission schedule, removed from C2 as out of scope, with the exact
removed code and a verified restore checklist.
