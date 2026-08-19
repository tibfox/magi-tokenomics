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
> | 10 | 259 B | 4,292 | **904** |
> | 30 | 779 B | 9,070 | **1,190** |
> | 60 | 1,559 B | 15,309 | **1,956** |
>
> Those figures include the `pagesum|<ch>|<ep>` accumulator that `finalizeEpoch`
> holds the declared `totalShares` to. It costs **~85 RC on a full page** (1,871 →
> 1,956, under 5%) and about 340 RC once on an epoch's first page, which sets the
> key's high-water mark. That buys back the invariant the merkle book gave up: a
> declared denominator higher than the pages published used to strand the difference
> where no call could reach it — half an epoch, from one wrong number.
>
> **8.2× cheaper at a full page**, because state writes were ~92% of the bill
> (~311 RC per account against ~1 RC per byte of log). Reproduce with
> `TestRC_RealisticSharePageCost` and `TestSplitProbe`-style measurement.
>
> What an epoch costs now:
>
>     9 pages x 1,956         ~=  17,600
>     + submitRoot            ~=     600     one call, whatever the earner count
>     + poke and pull         ~=   2,500
>                                --------
>                                ~20,700 RC  ~= 21 HBD on the reporter's ledger
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

### Attest mode — the payload record dominates, and it is priced by the byte

Attest mode (`reporter_mode = 2`) is far more expensive than single or cosigned, and
the reason is not the voting: it is that `auth` pins the attested payload **byte for
byte** until the round commits. The host prices contract state per byte —
`WRITE_IO_GAS_RC_COST = 19`, `READ_IO_GAS_RC_COST = 1`
(`modules/common/params/params.go`) — so holding a page is the single largest cost in
the mode.

Measured by `TestRC_AttestPayloadCostScalesWithBytes`, at the entry shape a live pool
emits:

| entries | payload | hold (1st attest) | apply + release (2nd) |
|---:|---:|---:|---:|
| 1 | 81 B | 1,699 | 1,358 |
| 10 | 315 B | 6,145 | 2,044 |
| 30 | 835 B | 16,025 | 3,607 |
| 60 | 1,615 B | **30,846** | 5,950 |

The marginal cost is **19.0 RC per payload byte**, exactly the host's write rate — the
test asserts this stays true, so if the pricing model ever changes, the suite says so
rather than this page going quietly stale.

**Two consequences worth planning around:**

**A full page costs ~30,800 RC to attest, 3× the free tier.** The same page in single
mode costs ~1,871 RC. That is a **16× penalty for the mode**, paid by whichever
authority attests first. An attester running on the free tier alone cannot hold a
60-entry page. Either fund the attesters or use smaller pages — 10 entries costs 6,145
and does fit.

**But it is a high-water-mark cost, not a per-round one.** The host charges the write
rate only for bytes that push a contract above its all-time peak size
(`ContractSession.IncSize`: `newWriteGas = max(0, CurrentSize-MaxSize)`); bytes reusing
space under the peak are charged at the read rate, 19× less. Since the leak fix
releases the record on commit, later rounds rewrite into freed space. Measured by
`TestRC_AttestHighWaterMarkMakesLaterRoundsCheap` over four consecutive pages:

    page 1   30,823 RC     <- sets the high-water mark
    page 2    6,870 RC
    page 3    6,888 RC
    page 4    6,888 RC     <- flat, so the space IS being reclaimed

**What this means for replacing `auth.Hash`.** The 4,096-byte record exists only
because `auth.Hash` is a 128-bit double-FNV that is not collision-resistant, so the
code pins the exact bytes and rejects any mismatch. A real digest would take the record
to 32 bytes. On RC that is worth:

- **~29,100 RC on the first attest** (30,846 → ~1,700), an 18× cut, and it takes the
  mode back inside the free tier
- **~1,580 RC per round after that** (6,888 → ~5,300), about 23%

So the RC case is real but it is mostly a case about the *first* attest and about page
size, not about steady-state throughput. It is still the strongest argument for the
change, since it is what makes attest mode usable by an unfunded attester at full page
size. The cost is getting a collision-resistant digest into a tinygo wasm contract.

---

## Practical guidance

| role | typical per-epoch cost | capacity to hold |
|---|---|---|
| keeper (`distributeEpoch` + 3 `pullFunding`) | ~5,400 RC | small, and a catch-up is now ~2.4× cheaper — but it still lands in one transaction |
| reporter (pages + `finalizeEpoch`) | ~9,000 RC for 60 earners; ~70,000 for 500 | size to a full epoch's pages, sent together |
| claimant | ~600–1,100 RC per claim | negligible, one call at a time |
| deployer (one-off) | ~18,000 RC | the whole sequence at once; ~10 HBD is ample |
| migration (per 25-holder batch) | ~9,600 RC | the run, not the batch — back-to-back batches do not thaw in between |

### Sizing a real deployment: how much HBD each role must hold

The tables above are per call. This is the number an operator actually needs: given a
community size and an epoch cadence, what balance does the reporter need to hold.

**Two facts do the work.** Capacity is the account's VSC-ledger HBD balance in millis
plus 10,000 for a `hive:` account — confirmed on a real chain by the scale run, whose
reporter logged `ledger hbd=290000 -> RC ~300000`. And a spend does not deduct, it
FREEZES, thawing linearly back over ~5 days.

That second fact is what catches people. With epochs longer than the thaw window each
spend has fully returned before the next one, so capacity need only cover one epoch.
With DAILY epochs it does not: the spend from *d* days ago still has `1 − d/5` frozen,
so at steady state the frozen total settles at

    E × (1 + 0.8 + 0.6 + 0.4 + 0.2) = 3E

**Reporter capacity, derived from the measured per-page cost** (1,956 RC at 60 entries,
plus ~600 submitRoot, ~2,500 poke and pull, ~600 finalize):

| earners | pages | RC per epoch | daily epochs need | weekly epochs need |
|---:|---:|---:|---:|---:|
| 50 | 1 | 5,656 | 16,968 | 5,656 |
| 200 | 4 | 11,524 | 34,572 | 11,524 |
| 500 | 9 | 21,304 | 63,912 | 21,304 |
| 1,000 | 17 | 36,952 | 110,856 | 36,952 |
| 2,000 | 34 | 70,204 | 210,612 | 70,204 |
| 5,000 | 84 | 168,004 | 504,012 | 168,004 |

Capacity is millis, so **subtract the 10,000 free tier and read the rest as HBD/1000**:
a 500-earner tribe on daily epochs needs ~63,912 − 10,000 ≈ **54 HBD** on the
reporter's ledger; on weekly epochs, ~11 HBD. A 2,000-earner tribe on daily epochs
needs ~200 HBD.

Three things worth taking from the shape of that table:

- **Cadence costs more than size.** Moving 500 earners from weekly to daily epochs
  triples the requirement; growing from 500 to 1,000 earners only adds ~70%. A tribe
  under RC pressure should lengthen its epoch before it trims its earner list.
- **`min_share_bps` is the other lever, and it is cheaper than it looks.** Pages scale
  with earners, so dropping a long tail of dust earners removes whole pages. Going
  from 500 to 200 earners saves 5 pages — nearly half the epoch's cost — and the value
  redistributes to those who remain rather than being stranded.
- **The keeper and claimants need nothing.** A keeper poke plus pulls is ~5,400 RC and
  a claim ~600–1,100, both inside the free tier. Only the reporter and the deployer
  need a funded ledger; an earner claiming a reward never does.

Sanity-check against the real chain: the scale run's reporter held 290,000 millis
against a 502-earner epoch measured here at ~21,300 RC, i.e. roughly 14× headroom —
which is why it never came close to the ceiling even submitting all 9 pages
back-to-back.

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
