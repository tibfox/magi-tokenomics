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

| contract | action | RC | was |
|---|---|---:|---:|
| C0 token | `init` | 642 | — |
| C0 token | `mint` | 273 | — |
| C0 token | `transfer` (to contract) | 340 | — |
| C0 token | `transfer` (to account) | 214 | — |
| C0 token | `approve` | 243 | — |
| C0 token | `changeOwner` | 863 | — |
| C1 | `init` (staking + yield + airdrop) | 2,197 | 1,851 |
| C1 | `stake` (first, creates history) | 1,309 | 1,175 |
| C1 | `stake` (subsequent) | 1,179 | 1,046 |
| C1 | `stakeFor` | 1,226 | 1,093 |
| C1 | `unstake` | 785 | 628 |
| C1 | `claimUnstaked` (1 matured entry) | 610 | 505 |
| C1 | `claimUnstaked` (5 matured entries) | 821 | — |
| C1 | `pullFunding` (yield) | 1,140 | 853 |
| C1 | `claimYield` | 1,014 | 870 |
| C2 emission | `init` (3 buckets) | 9,288 | 8,730 |
| C2 emission | `distributeEpoch` (1 epoch, 3 buckets) | 1,990 | 1,537 |
| C2 emission | `queueTokenOp` | 311 | 163 |
| C2 emission | `cancelTokenOp` | 240 | 100 |
| C2 emission | `executeTokenOp` (calls token.pause) | 482 | 333 |
| distributor | `init` | 2,623 | 2,330 |
| distributor | `addChannel` | 832–1,097 | 661–911 |
| distributor | `pullFunding` (cross-contract) | 1,103–1,225 | 821–923 |
| distributor | `finalizeEpoch` | 554–630 | 343–347 |
| distributor | `claim` (liquid, with proof) | 959 | 678–764 |
| distributor | `claim` (part staked) | ~3,100 | 2,967 |
| distributor | `submitRoot` | ~600 | — |

Read-only queries (`stakeOf`, `scheduleInfo`, `owedOf`, `fundedOf`, `minStakeSum`) land
at the **100 RC floor**; `stakeAtHeight` 101 and `shareOf` 126.

**A staked claim costs 3.6× a liquid one** — 2,967 against 828 in the same fixture.
The extra 2,139 RC buys an `approve` and a cross-contract `stakeFor` on top of the
plain transfer, and the claimant pays it. That is the number to keep an eye on,
because a claim is the one call an ORDINARY holder makes: someone who has earned a
reward may hold no HBD at all and be running on the 10,000 free tier alone. At 2,967
there is room, and `TestRC_StakedClaimCost` fails if it ever stops fitting.

**The `was` column is event emission.** The contracts now log every state transition
for the indexer (see [`indexer/README.md`](../indexer/README.md)), and a log costs gas
in proportion to its bytes. The flat additions are 100–350 RC on writing entrypoints
and nothing on queries, which do not log. Two paths were deliberately shaped to keep
that cost flat rather than per-item — see `submitShares` and `airdropBatch` below.

**Two C1 costs drift over a deployment's life, and only one of them is bounded.**

`ckpt|` (a global checkpoint of `total_staked`, appended on *every* stake mutation by
*any* account) and `hist|acct|` are append-only and never pruned. `stakeAtHeight`,
`claimYield`, `minStakeSum` and `sweepEmptyEpoch` binary-search them at one host state
read per probe, so those costs grow with **log₂(entries)** — about one extra probe per
doubling. The figures above are measured on short arrays; a deployment three years in
pays a few probes more per claim. Size for that rather than for week one.

What is bounded is same-block growth: several mutations in one block now collapse to a
single entry, since the search can only reach the last of them anyway. That matters
most where it used to be worst — a 25-recipient `airdropStaked` batch appended 25
global checkpoints and now appends one, measured at **13,549 RC against 15,836
before** — and it holds generally, because any two accounts staking in the same block
used to add two entries where one was reachable. The cost is one state read per
mutation that has a predecessor, which is why a repeat `stake` is ~37 RC dearer and a
first stake is unchanged.

**`claimUnstaked` is the one that got cheaper per unit.** Each matured entry used to
carry its own token transfer to the same recipient; they are now summed into one. Five
entries cost 821 RC — about **53 RC per additional entry**, against the 214 RC a
transfer to a plain account costs on its own. A full 20-entry claim is now roughly
1,600 RC rather than roughly 4,600.

**What the merge changed** (unchanged by this work). Yield reads the stake history in
the same contract rather than across a boundary, so `claimYield` is cheaper than the
cross-contract version was. `C1.init` is large because one init configures three roles,
and `C2.init` because each bucket also writes a name→target index — that index is what
lets several buckets pay one distributor. `distributor.init` is small because the
per-channel policy moved out of it into `addChannel`.

No single fixed-cost call is large. The whole deployment sequence — four `init`s, two
`addChannel`s, `adoptSchedule` and the token handover — totals roughly **18,000 RC**,
and it happens back to back with no time for anything to thaw, so the deployer needs
capacity for the lot at once. About 10 HBD deposited is ample.

---

## Variable-cost functions — these are the ones to size

### `submitShares` — scales with entries per page

| entries | RC | RC/entry | was |
|---:|---:|---:|---:|
| 1 | 558 | — | 697 |
| 10 | 1,901 | ~133 | 1,376 |
| 30 | 4,595 | ~133 | 3,310 |
| 60 | 8,369 | ~133 | 5,942 |

Roughly **~133 RC per entry over a ~425 fixed base**, up from ~91 over ~465. The
default page size of 60 entries costs ~8,400 RC; the 4096-byte payload cap usually
binds before RC does.

> ### ★ THE SHARE BOOK IS A MERKLE ROOT NOW, AND THAT CHANGES THESE NUMBERS
>
> The table above is what a page cost when the contract wrote one state entry per
> account. It no longer does: the distributor stores a 32-byte commitment and the
> leaves are LOGGED for the indexer. Measured against the shape a live pool emits
> (`hive:u123:521226084116000`):
>
> | entries | payload | RC before | RC now |
> |---:|---:|---:|---:|
> | 10 | 259 B | 4,292 | **566** |
> | 30 | 779 B | 9,070 | **1,088** |
> | 60 | 1,559 B | 15,309 | **1,871** |
>
> **8.2× cheaper at a full page**, because state writes were ~92% of the bill
> (~311 RC per account against ~1 RC per byte of log). Reproduce with
> `TestRC_RealisticSharePageCost` and `TestSplitProbe`-style measurement.
>
> What an epoch costs now:
>
>     9 pages x 1,871         ~=  16,800
>     + submitRoot            ~=     600     one call, whatever the earner count
>     + poke and pull         ~=   2,500
>                                --------
>                                ~20,000 RC  ~= 20 HBD on the reporter's ledger
>
> Against ~127,500 before. **The claimant pays ~130 RC more** for proof
> verification — 959 against 830 — which is the cost moving from the operator to
> the person collecting, and it still fits the 10,000 free tier with room.
>
> **Size from the number of EARNERS still**, but the slope is far shallower: a
> page is now dominated by its log bytes rather than by per-account writes.

The base is the number to keep honest — it is what the reporter's coherence check is
sized from, and a stale one under-covers small pages rather than large ones. That check
now lives in `reporter/submit/rccost.go` and is bound to a real measurement by
`TestRC_ReporterRcLimitFormulaCoversAFullPage`, which fails if a page outgrows what the
constants predict *or* if the headroom multiplier stops meaning anything. Both have
happened before, the second silently.

> **Why the page emits one log and not one per entry.** The obvious shape — a `share`
> event per accepted entry — was built and measured first: it took the marginal rate to
> **229 RC/entry** and a 60-entry page to **14,127 RC**, past the free tier, turning a
> 500-earner epoch into ~127,000 RC. `submitShares` is the most-repeated privileged
> call in the system and the reporter is its heaviest consumer, so the page-level form
> won: one `shares` log carrying the submitted entries verbatim, plus a `skip` log for
> anything dropped. Applied = submitted − skipped, so the per-account share book is
> still reconstructible exactly — for a quarter of the added cost.

**Sizing a real epoch:** 500 earners at 60/page = 9 pages ≈ **70,000 RC** (was
~44,000), sent within minutes of each other. A reporter serving a tribe that size needs
capacity for the whole epoch at once, and enough headroom that the next epoch is not
waiting on the thaw — so roughly 65 HBD or more on the ledger.

### `distributeEpoch` — scales with epochs caught up × buckets

| epochs | buckets | RC | was | change |
|---:|---:|---:|---:|---:|
| 1 | 1 | 1,463 | 1,279 | +14% |
| 5 | 1 | 2,492 | 5,242 | **−52%** |
| 10 | 1 | 3,782 | 10,196 | **−63%** |
| 25 | 1 | 7,722 | 25,134 | **−69%** |
| 50 | 1 | 14,256 | 50,004 | **−71%** |
| 1 | 3 | 1,911 | 1,537 | +24% |
| 10 | 3 | 7,104 | 14,587 | **−51%** |
| 50 | 3 | 30,594 | 72,122 | **−58%** |

About **~261 RC per epoch** with one bucket and **~585** with three, down from ~995 and
~1,437. A single-epoch poke costs slightly more than it did — that is the event
emission — and everything beyond one epoch costs dramatically less.

> **Why the catch-up got cheap.** C2 does not mint: each epoch's emission is pulled
> from the approved pool with a cross-contract `transferFrom`, and that call dominated
> the cost. Source, destination and asset are identical on every iteration, so the poke
> now *plans* the catch-up first — walking forward while the pool still covers the
> running total, which is the same all-or-nothing-per-epoch test evaluated ahead of
> time — and makes **one** pull for the whole run. The schedule parameters and bucket
> table are read once instead of once per epoch, which is the rest of the saving.
>
> All-or-nothing per epoch is unchanged: a partially funded epoch is still never marked
> done, and a starved poke still writes no state and resumes on refill.
>
> **Trusting these numbers:** `rc_measure_test.go` only measures real emission if the
> fixture funds a pool first — an unfunded one returns `{"distributed":"0",
> "starved":true}` and the table records a no-op instead. The test asserts
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
cheap calls (poke, pull, finalize) all succeed. At 60 entries that is ~7,400 RC with
headroom.

### `airdropBatch` — scales with recipients

| recipients | RC | was |
|---:|---:|---:|
| 1 | 897 | 697 |
| 10 | 4,145 | 4,030 |
| 25 | 9,585 | 9,471 |
| 50 | 18,680 | 18,566 |

About **~370 RC per recipient**, unchanged: the per-recipient cost is one token
transfer and events did not touch it. The batch emits ONE `airdrop` summary — a flat
~115 RC however many recipients it carries — because the liquid path's per-recipient
record already exists as the token's own indexed `transfer` log, and the
`airdropStaked` path emits a `stake` event per recipient anyway. Restating either would
be paying RC for a row the indexer already has.

~10–25 per batch is a comfortable size: batches are idempotent per `batchId`, so there
is no penalty for splitting. A migration of 1,000 holders is ~370,000 RC in total,
which is the number to size the deposit against — a run of back-to-back batches gives
the earlier ones no time to thaw.

---

## Practical guidance

| role | typical per-epoch cost | capacity to hold |
|---|---|---|
| keeper (`distributeEpoch` + 3 `pullFunding`) | ~5,400 RC | small, and a catch-up is now ~2.4× cheaper — but it still lands in one transaction |
| reporter (pages + `finalizeEpoch`) | ~9,000 RC for 60 earners; ~70,000 for 500 | size to a full epoch's pages, sent together |
| claimant | ~600–1,100 RC per claim | negligible, one call at a time |
| deployer (one-off) | ~18,000 RC | the whole sequence at once; ~10 HBD is ample |
| migration (per 25-holder batch) | ~9,600 RC | the run, not the batch — back-to-back batches do not thaw in between |

### The minimum deployment is 1.3% under the free tier

A deployer holding **no HBD at all** has exactly 10,000 RC. The smallest useful
deployment — token, emission, distributor, one channel — measures:

| call | RC | running |
|---|---|---|
| `token.init` | 780 | 780 |
| `C2.init` (emission) | 5,188 | 5,968 |
| `C3.init` (distributor) | 2,608 | 8,576 |
| `addChannel` | 1,290 | **9,866 / 10,000** |

It fits, with 134 RC to spare. Two consequences worth stating plainly:

- **It only just fits.** Every byte added to a contract raises its init cost, so
  contract growth is what breaks this, silently and without touching any function
  anyone measures. `TestRCBudget_TokenomicsSetupFitsTheFreeTier` fails the build
  above 10,000 for exactly that reason.
- **Running out of RC is not an error.** The transaction is not rejected; it is
  simply never applied. There is no abort message and nothing in the logs — the
  call just does not happen, and whatever waits on its state waits forever. A
  devnet suite lost a full run to this: three inits landed, `addChannel` did not,
  and it read as a contract bug.

Deploy costs ~10 HBD of L1 balance per contract, so deposit for RC **after**
deploying — depositing first starves the deploy fee.

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
