# RC cost per function

Measured, not estimated. These are the **real metered costs** the chain applies —
`RcUsed = ceil(gas / CYCLE_GAS_PER_RC)`, floor 100 — captured by running every
callable function against the real wasm engine.

Reproduce with:

```sh
GOTOOLCHAIN=go1.25.3 go test ./itest/ -run TestRC_ProfileAllFunctions -count=1 -v
GOTOOLCHAIN=go1.25.3 go test ./itest/ -run TestRC_DistributeEpochCatchUpCost -count=1 -v
```

## What you are spending against

**RC is a regenerating capacity, not a balance you burn down.** Three things decide
what an account can do (`modules/rc-system` in go-vsc-node):

```
capacity   = the account's VSC-ledger HBD balance   (+10,000 for hive: accounts)
spending   freezes RC, which then thaws linearly back to zero over ~5 days
available  = capacity − currently frozen
```

Two consequences worth having straight before reading any number below:

- **Capacity is set by your HBD balance.** Depositing raises it one-for-one — 100 HBD
  measured at ~110,000 RC on devnet. Holding more HBD does not "buy" transactions; it
  raises the ceiling you can be mid-flight against.
- **It refills on its own.** A spend is returned over roughly five days, so the
  sustainable throughput of an account is about *capacity per five days*. An operation
  that costs more RC than you have capacity for cannot be made to fit by waiting; one
  that merely costs a lot can be repeated as the frozen amount thaws.

So the question a cost below answers is **"what capacity does this need?"**, not "does
it fit in an allowance". Size the account for the largest single call it must make,
then check the per-day total against capacity ÷ 5.

---

## Fixed-cost functions

| contract | action | RC |
|---|---|---:|
| C0 token | `init` | 642 |
| C0 token | `mint` | 273 |
| C0 token | `transfer` (to contract) | 340 |
| C0 token | `transfer` (to account) | 214 |
| C0 token | `approve` | 243 |
| C0 token | `changeOwner` | 863 |
| C1 staking | `init` | 970 |
| C1 staking | `stake` (first, creates history) | 1,204 |
| C1 staking | `stake` (subsequent) | 1,043 |
| C1 staking | `stakeFor` | 1,090 |
| C1 staking | `unstake` | 628 |
| C1 staking | `claimUnstaked` | 502 |
| C2 emission | `init` (3 buckets) | 5,725 |
| C2 emission | `queueTokenOp` | 163 |
| C2 emission | `cancelTokenOp` | 100 |
| C2 emission | `executeTokenOp` (calls token.pause) | 333 |
| C3 / C5 | `init` | 2,683 |
| C3 / C5 | `pullFunding` (cross-contract) | 889 |
| C3 / C5 | `finalizeEpoch` | 343–347 |
| C3 / C5 | `claim` | 497–529 |
| C6 migration | `init` | 1,175 |
| C7 yield | `init` | 3,102 |
| C7 yield | `pullFunding` | 822 |
| C7 yield | `claim` (reads C1 history) | 980 |

Read-only queries (`stakeOf`, `stakeAtHeight`, `scheduleInfo`, `owedOf`, `shareOf`,
`fundedOf`, `minStakeSum`) all land at the **100 RC floor**.

**What C1's drawdown accumulator costs.** Every `stake`/`unstake` also files that
account's position against its epoch-start level, so C7 can divide by the exact
`Σ min(aᵢ,bᵢ)` (see the C7 section of the [README](../README.md)). That is one extra
history lookup and one state write: **+25 RC**, flat, on a ~600–1,200 RC operation.

The cost sits with staking rather than with claiming. `C7.claim` makes a single
cross-contract call for the denominator, which is cheaper than deriving one from two
`totalStakedAtHeight` reads — so the exact denominator is not merely affordable, it is
net cheaper for the operation users perform most.

No single fixed-cost call is large. The whole deployment sequence — seven `init`s
plus the token handover — totals roughly **18,000 RC**, and it happens back to back
with no time for anything to thaw, so the deployer needs capacity for the lot at
once. About 10 HBD deposited is ample.

---

## Variable-cost functions — these are the ones to size

### `submitShares` — scales with entries per page

| entries | RC | RC/entry |
|---:|---:|---:|
| 1 | 278 | — |
| 10 | 1,131 | ~95 |
| 30 | 2,745 | ~90 |
| 60 | 4,897 | ~82 |

Roughly **~80–95 RC per entry** plus ~200 fixed. The default page size of 60
entries costs ~4,900 RC; the 4096-byte payload cap usually binds before RC does.

**Sizing a real epoch:** 500 earners at 60/page = 9 pages ≈ **44,000 RC**, sent within
minutes of each other. A reporter serving a tribe that size needs capacity for the
whole epoch at once, and enough headroom that the next epoch is not waiting on the
thaw — so roughly 40 HBD or more on the ledger.

### `distributeEpoch` — scales with epochs caught up × buckets

| epochs | buckets | RC |
|---:|---:|---:|
| 1 | 1 | 1,279 |
| 5 | 1 | 5,242 |
| 10 | 1 | 10,196 |
| 25 | 1 | 25,134 |
| 50 | 1 | 50,004 |
| 1 | 3 | 1,718 |
| 10 | 3 | 14,587 |
| 50 | 3 | 72,122 |

About **~995 RC per epoch** with one bucket and **~1,437** with three, over a fixed
base of ~280. This is the cost that catches operators out: a keeper that stops for a
fortnight and then pokes once has to catch up every missed epoch in a single
transaction.

> **Why a poke is not cheap.** C2 does not mint. Each epoch costs a cross-contract
> `transferFrom` against the pool, plus two reads (`allowance`, `balanceOf`) per poke.
>
> **Trusting these numbers:** `rc_measure_test.go` only measures real emission if the
> fixture funds a pool first — an unfunded one returns `{"distributed":"0",
> "starved":true}` and the table records a ~271-RC no-op instead. The test asserts
> `distributed` now, but if these figures ever look suspiciously flat, check the
> `ret=` column shows a non-zero `distributed` before believing them.

**This is what `maxCatch` is for.** It caps how many epochs one poke will process
(1..1000, default 50). Because the whole catch-up lands in ONE transaction, it is
bounded by capacity rather than by throughput — waiting does not help, since the RC
has to be available simultaneously. Either keep the keeper running, give it capacity
for the worst catch-up you would tolerate, or set `maxCatch` low enough that a poke
always fits and simply poke repeatedly until caught up.

**`submit.rc_limit` must cover a FULL page**, and the reporter now refuses a config
where it does not. The two settings validate fine in isolation and are incoherent
together: pagination emits a short page only at the very end, so if a full page
exceeds the limit then every page but the last reverts — every time — while the
cheap calls (poke, pull, finalize) all succeed. At 60 entries that is ~7,140 RC with
headroom.

### `airdropBatch` — scales with recipients

| recipients | RC |
|---:|---:|
| 1 | 499 |
| 10 | 3,848 |
| 25 | 9,289 |
| 50 | 18,383 |

About **~370 RC per recipient**. ~10–25 per batch is a comfortable size: batches are
idempotent per `batchId`, so there is no penalty for splitting. A migration of 1,000
holders is ~370,000 RC in total, which is the number to size the deposit against — a
run of back-to-back batches gives the earlier ones no time to thaw.

---

## Practical guidance

| role | typical per-epoch cost | capacity to hold |
|---|---|---|
| keeper (`distributeEpoch` + 3 `pullFunding`) | ~4,100 RC | small, **provided it never falls behind** — a long catch-up lands in one transaction |
| reporter (pages + `finalizeEpoch`) | ~5,200 RC for 60 earners; ~44,000 for 500 | size to a full epoch's pages, sent together |
| claimant | ~500–1,100 RC per claim | negligible, one call at a time |
| deployer (one-off) | ~18,000 RC | the whole sequence at once; ~10 HBD is ample |
| migration (per 25-holder batch) | ~9,300 RC | the run, not the batch — back-to-back batches do not thaw in between |

Three rules follow from the numbers:

1. **Size for the burst, not the average.** Everything expensive here happens in
   bursts — a deploy sequence, an epoch's worth of pages, a batch run — and a burst
   gets no benefit from the five-day thaw.
2. **Don't let the keeper fall behind**, or cap `maxCatch` so a poke always fits.
   Emission is not lost by poking repeatedly — `distributeEpoch` is idempotent and
   catches up in bounded chunks.
3. **Keep airdrop batches small** (~10–25). Batches are idempotent per `batchId`, so
   splitting a migration costs nothing but transactions.

## Caveats

- Measured in-process against the real wasm engine and the real metering. Devnet and
  mainnet apply the same formula, but absolute gas can shift with a runtime or
  contract change — **re-run the profile after changing contract code**.
- Costs are per *successful* execution. A call that aborts still consumes the RC it
  burned before aborting, which is why a thinly-funded account fails in confusing
  ways rather than cleanly.
- `submitShares` figures are for a single page. Attest mode multiplies the cost by
  the number of attesting machines, since each submits the same page independently.
