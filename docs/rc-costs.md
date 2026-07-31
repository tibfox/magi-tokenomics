# RC cost per function

Measured, not estimated. These are the **real metered costs** the chain applies —
`RcUsed = ceil(gas / CYCLE_GAS_PER_RC)`, floor 100 — captured by running every
callable function against the real wasm engine.

Reproduce with:

```sh
GOTOOLCHAIN=go1.25.3 go test ./itest/ -run TestRC_ProfileAllFunctions -count=1 -v
GOTOOLCHAIN=go1.25.3 go test ./itest/ -run TestRC_DistributeEpochCatchUpCost -count=1 -v
```

## The budget you are spending against

**RC = the account's VSC-ledger HBD balance + a 10,000 free allowance.**

The 10,000 is a standing allowance for Hive accounts, *not* a per-execution budget
and not a per-day quota. An account holding no HBD on the VSC ledger has exactly
10,000 RC to work with; depositing HBD raises it one-for-one. So "fits the free
tier" below means *an unfunded account can do this*; anything larger needs a
deposit.

Measured on a devnet run: depositing 100 HBD gave an account ~110,000 RC.

---

## Fixed-cost functions

| contract | action | RC | free tier |
|---|---|---:|---|
| C0 token | `init` | 642 | ok |
| C0 token | `mint` | 273 | ok |
| C0 token | `transfer` (to contract) | 340 | ok |
| C0 token | `transfer` (to account) | 214 | ok |
| C0 token | `approve` | 243 | ok |
| C0 token | `changeOwner` | 863 | ok |
| C1 staking | `init` | 939 | ok |
| C1 staking | `stake` (first, creates history) | 1,179 | ok |
| C1 staking | `stake` (subsequent) | 1,018 | ok |
| C1 staking | `stakeFor` | 1,066 | ok |
| C1 staking | `unstake` | 603 | ok |
| C1 staking | `claimUnstaked` | 501 | ok |
| C2 emission | `init` (3 buckets) | 5,725 | ok |
| C2 emission | `queueTokenOp` | 163 | ok |
| C2 emission | `cancelTokenOp` | 100 | ok |
| C2 emission | `executeTokenOp` (calls token.pause) | 333 | ok |
| C3 / C5 | `init` | 2,683 | ok |
| C3 / C5 | `pullFunding` (cross-contract) | 889 | ok |
| C3 / C5 | `finalizeEpoch` | 343–347 | ok |
| C3 / C5 | `claim` | 497–529 | ok |
| C6 migration | `init` | 1,175 | ok |
| C7 yield | `init` | 3,061 | ok |
| C7 yield | `pullFunding` | 880 | ok |
| C7 yield | `claim` (reads C1 history) | 1,069 | ok |

Read-only queries (`stakeOf`, `stakeAtHeight`, `scheduleInfo`, `owedOf`, `shareOf`,
`fundedOf`) all land at the **100 RC floor**.

Every fixed-cost call fits the free tier comfortably. The whole deployment sequence
— seven `init`s plus the token handover — totals roughly **18,000 RC**, so a
deployer needs a funded account (about 10 HBD deposited is ample).

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
entries costs ~4,900 RC — inside the free tier, but only just, and the 4096-byte
payload cap usually binds first anyway.

**Sizing a real epoch:** 500 earners at 60/page = 9 pages ≈ **44,000 RC**, so a
reporter serving a busy tribe needs a funded account, not the free tier.

### `distributeEpoch` — scales with epochs caught up × buckets

| epochs | buckets | RC | free tier |
|---:|---:|---:|---|
| 1 | 1 | 1,279 | ok |
| 5 | 1 | 5,242 | ok |
| 10 | 1 | 10,196 | **exceeds** |
| 25 | 1 | 25,134 | **exceeds** |
| 50 | 1 | 50,004 | **exceeds** |
| 1 | 3 | 1,718 | ok |
| 10 | 3 | 14,587 | **exceeds** |
| 50 | 3 | 72,122 | **exceeds** |

About **~995 RC per epoch** with one bucket and **~1,437** with three, over a fixed
base of ~280. This is the cost that catches operators out: a keeper that stops for a
fortnight and then pokes once has to catch up every missed epoch in a single
transaction.

> **Re-measured 2026-07-29 for the allowance model, and the numbers went UP** — about
> 19% per epoch with one bucket. C2 used to `mint`; it now does a cross-contract
> `transferFrom` per epoch plus two reads (`allowance`, `balanceOf`) per poke. The
> previous figures in this table (~770 RC/epoch/bucket) were measured under the
> minting model and are no longer accurate.
>
> The measurement itself had also silently broken: `rc_measure_test.go` never minted
> a pool, so every poke returned `{"distributed":"0","starved":true}` and the table
> was recording a 271-RC no-op. If these numbers ever look suspiciously flat, check
> the `ret=` column shows a non-zero `distributed` before trusting them.

**This is what `maxCatch` is for.** It caps how many epochs one poke will process
(1..1000, default 50). The free tier now covers only **~9 epochs of catch-up with one
bucket and ~6 with three** — so either keep the keeper running, fund it, or set
`maxCatch` low enough that a poke always fits your keeper's RC and simply poke
repeatedly until caught up.

**`submit.rc_limit` must cover a FULL page**, and the reporter now refuses a config
where it does not. The two settings validate fine in isolation and are incoherent
together: pagination emits a short page only at the very end, so if a full page
exceeds the limit then every page but the last reverts — every time — while the
cheap calls (poke, pull, finalize) all succeed. At 60 entries that is ~7,140 RC with
headroom.

### `airdropBatch` — scales with recipients

| recipients | RC | free tier |
|---:|---:|---|
| 1 | 499 | ok |
| 10 | 3,848 | ok |
| 25 | 9,289 | ok (barely) |
| 50 | 18,383 | **exceeds** |

About **~370 RC per recipient**. **Keep batches at or below 25 recipients** to stay
inside the free tier; ~10 is a comfortable default. A migration of 1,000 holders is
~40 batches and ~370,000 RC total, so budget the deposit accordingly.

---

## Practical guidance

| role | typical per-epoch cost | needs funding? |
|---|---|---|
| keeper (`distributeEpoch` + 3 `pullFunding`) | ~4,100 RC | free tier is fine **if** it never falls behind |
| reporter (pages + `finalizeEpoch`) | ~5,200 RC for 60 earners; ~44,000 for 500 | **yes**, past a small tribe |
| claimant | ~500–1,100 RC per claim | free tier is fine |
| deployer (one-off) | ~18,000 RC | **yes** |
| migration (per 25-holder batch) | ~9,300 RC | yes for anything beyond a few batches |

Three rules follow from the numbers:

1. **Fund the reporter and the deployer.** Everything else fits the free tier.
2. **Don't let the keeper fall behind**, or cap `maxCatch` so a poke always fits.
   Emission is not lost by poking repeatedly — `distributeEpoch` is idempotent and
   catches up in bounded chunks.
3. **Keep airdrop batches ≤ 25.** Batches are idempotent per `batchId`, so splitting
   a migration into many small batches costs nothing but transactions.

## Caveats

- Measured in-process against the real wasm engine and the real metering. Devnet and
  mainnet apply the same formula, but absolute gas can shift with a runtime or
  contract change — **re-run the profile after changing contract code**.
- Costs are per *successful* execution. A call that aborts still consumes the RC it
  burned before aborting, which is why a thinly-funded account fails in confusing
  ways rather than cleanly.
- `submitShares` figures are for a single page. Attest mode multiplies the cost by
  the number of attesting machines, since each submits the same page independently.
