# reporter — off-chain share reporter

The contracts in this repo cannot read Hive. `C3` (content rewards) and `C5` (LP
rewards) only know how to accept a list of `account:shares` pairs and pay claims
against it. This service is the piece that watches Hive, computes those shares, and
pushes them on chain.

There is no oracle: the reporter is a plain service that a tenant runs, holding an
account that the distributor's `init` named as its reporter authority.

```
reporter epoch    # where are we? verifies config against on-chain state
reporter compute  # what would be reported? prints the canonical shares
reporter plan     # what would be sent? prints the exact custom_json bodies
reporter run      # send it (DRY-RUN unless -broadcast)
```

## Quick start

```sh
go build -o reporter ./reporter/cmd/reporter
./reporter init-config > reporter.json     # then edit contract ids, tag, curves
./reporter epoch   -config reporter.json   # sanity: config vs chain
./reporter compute -config reporter.json   # see the shares
./reporter run     -config reporter.json   # dry run, nothing signed
export REPORTER_ACTIVE_WIF=5J...           # the reporter account's ACTIVE key
./reporter run     -config reporter.json -broadcast
```

## Packages

| package     | what it does                                                        |
|-------------|---------------------------------------------------------------------|
| `sharecore` | deterministic share math. Integer-only, no I/O. The important part.  |
| `hivesrc`   | reads Hive: epoch windows, posts, votes → `sharecore` input          |
| `vscapi`    | reads VSC contract state (incl. historical C1 stake)                 |
| `submit`    | builds the ordered call plan + durable progress                      |
| `broadcast` | renders and signs `vsc.call` custom_json                             |

## Config reference

Every field, since they are otherwise scattered across the sections below. Unknown
keys are a load error, not a silent default — a typo'd key never takes effect
quietly.

| field | default | notes |
|---|---|---|
| `hive.api` | `["https://api.hive.blog"]` | first is used for reads; **all** are handed to the broadcaster, so one node being down does not stall an epoch |
| `hive.chain_id` | `""` = Hive **mainnet** | which chain signatures are made over. **Not** `vsc.net_id`. Wrong value → the signature recovers to a different key and the node says `missing required active authority`, which reads as a permissions bug |
| `vsc.api` | *required* | node GraphQL, for contract state reads |
| `vsc.net_id` | *required* | `vsc-mainnet` / `vsc-testnet` |
| `indexer.api` | *required for* `kind=lp` | Hasura GraphQL endpoint |
| `indexer.secret` | `""` | `x-hasura-admin-secret`; empty when public |
| `indexer.pool` | *required for* `kind=lp` | pool contract id as `indexer_contract_id` |
| `indexer.page_size` | `1000` | rows per GraphQL page |
| `indexer.allow_stale` | `false` | disables the freshness gate. See the warning under **Two source kinds** before setting it |
| `contracts.distributor` | *required* | C3 (content) or C5 (LP) |
| `contracts.funder` | `""` | C2. Required when `submit.keeper` |
| `contracts.stake` | `""` | C1. Required when `source.weight=token_stake` |
| `epoch.genesis` | `0` | **must equal** the distributor's `cfg_genesis` |
| `epoch.len` | *required* | **must equal** the distributor's `cfg_epochLen` |
| `epoch.lookback` | `20` | how many closed epochs back to search for the oldest unfinalized one. Raise after downtime — a backlog longer than this can never be worked off |

`reporter epoch` verifies `genesis`/`len` against on-chain state before anything is
submitted. A mismatch would report on the wrong block range and then finalize it,
which is unrecoverable.

| field | default | notes |
|---|---|---|
| `source.kind` | `content` | `content` reads Hive posts/votes → C3; `lp` replays liquidity events → C5 |
| `source.tag` | *required for* `kind=content` | the Hive tag/community that counts |
| `source.limit` | `1000` | max posts fetched per epoch |
| `source.attribution` | `cashout` | `cashout` scores a post in the epoch its Hive payout lands in (every vote counted once); `created` is prompter but discards votes cast after the snapshot |
| `source.weight` | `hive_rshares` | or `token_stake` (then `contracts.stake` is required) |
| `source.exclude` | `[]` | accounts whose posts are skipped entirely |
| `shares.author_reward_bps` | `0` | author/curator split, `0..10000` |
| `shares.author_curve` | `"1/1"` | `"num/den"` rational exponent |
| `shares.curation_curve` | `"1/1"` | `"1/2"` = sqrt = **early** voters win; `>1` rewards late voters. Opposite cultures — pick deliberately |
| `shares.muted` | `[]` | accounts that earn nothing |
| `page.max_entries` | `60` | entries per `submitShares` call |
| `page.max_bytes` | `3800` | must be `<= 4096`, the auth module's payload cap |
| `submit.account` | `""` | reporter Hive account, with or without `hive:` |
| `submit.wif_env` | `REPORTER_ACTIVE_WIF` | env var holding the ACTIVE key. **No key ever lives in the config**, so the file can be committed and shared across an Attest quorum |
| `submit.rc_limit` | `60000` | must fit a **full** page — validated at load, see below |
| `submit.progress_file` | `reporter-progress.json` | durable resume state |
| `submit.keeper` | `false` | also poke `C2.distributeEpoch` |
| `submit.pull_funding` | `false` | also call `pullFunding` |
| `submit.finalize` | `false` | also call `finalizeEpoch` |
| `submit.confirm_tries` | `6` | re-reads of the chain before finalizing. `0` selects the default — the gate **cannot** be turned off here |
| `submit.confirm_interval_sec` | `15` | seconds between those re-reads. `0` selects the default |

Two of these are checked **against each other** at load, because they validate
perfectly in isolation and are ruinous together. Pagination only emits a short page
at the very end, so if a full page exceeds `rc_limit` then *every page but the last*
reverts, every time — while the cheap calls (poke, pull, finalize) all succeed. The
budget is ~95 RC per entry over a ~200 base ([rc-costs.md](../docs/rc-costs.md)).

## Why the math is integer-only

`sharecore` uses `math/big` throughout and never `float64`. Two reasons:

1. **The challenge window.** `C3.finalizeEpoch` opens a window in which a guardian
   can cancel a bad report. That is only meaningful if a third party can recompute
   the same numbers from the same input.
2. **Attest mode.** The `auth` module's Attest mode requires N reporter accounts to
   push **byte-identical** payloads before a page applies. float64 rounding varies
   with expression order, so a single float would break the quorum.

Curve exponents are therefore rationals evaluated with an integer nth-root
(`PowRational` → `IntNthRoot`), not `math.Pow`.

### Curation curve direction

`curation_curve` is `"num/den"` and the direction is a deliberate policy choice
with inverted incentives:

| setting  | shape   | effect                                                 |
|----------|---------|--------------------------------------------------------|
| `"1/2"`  | concave | **early** voters earn more (classic Steem/Hive curation) |
| `"1/1"`  | linear  | order-neutral                                          |
| `"2/1"`  | convex  | **late** voters earn more (pile-on)                    |

## The four on-chain steps

## Signing

`submit.account` and `submit.wif_env` name the reporter's Hive account and the
environment variable holding its ACTIVE key. The key is never read from the config
file — a file holding a live active key tends to end up in a backup or a git repo.

**`hive.chain_id` selects which HIVE chain signatures are made over.** Leave it empty
for mainnet. It is NOT `vsc.net_id`: that selects the VSC network, while the chain id
selects the Hive chain underneath it, and a VSC network can run on a Hive chain that
is not mainnet. Getting this wrong does not fail as a chain mismatch — the signature
recovers to a different key and the node reports `missing required active authority`,
which reads as a permissions problem and sends you looking in the wrong place.

## Two source kinds

`source.kind` selects where shares come from. The rest of the pipeline —
canonicalisation, pagination, submission, Attest quorum — is identical either way.

| kind | reads | feeds |
|---|---|---|
| `content` (default) | Hive posts and votes | C3 |
| `lp` | the indexer's liquidity event log | C5 |

`reporter init-config lp` writes a ready LP config. It needs an `indexer` block:

```json
"indexer": {
  "api":       "https://indexer.example.com/v1/graphql",
  "secret":    "",
  "pool":      "vsc1...POOL",
  "page_size": 1000
}
```

**Why LP does not read the DEX directly.** `vsc-eco/dex-contracts` keeps LP balances
as current state (`lp-{address}`, total at `tlp`) with no height checkpoints. An epoch
is priced after it ends, so live state cannot answer "who held LP during epoch 41",
and paying against it would be flash-liquidity gameable — add just before the
snapshot, remove just after. So the reporter replays `add_liq`/`rem_liq` events:

```
LP(provider, H) = SUM(lp_minted where height <= H) - SUM(lp_burned where height <= H)
```

and credits **`min(LP(start), LP(end))`**, mirroring what C7 does for stake:
liquidity must be present at BOTH epoch boundaries to earn anything.

**Freshness is gated by default.** A lagging indexer does not error — it returns fewer
rows — so scoring an epoch it has not reached underpays providers irreversibly, with
nothing in any log. Before scoring, the reporter reads
`indexer_health.latest_block_height` and refuses unless the indexer is provably past
the epoch's end block.

**It is measured per pool, and that distinction is load-bearing.** `indexer_health`
reports `MAX(block_height)` across *every* contract the indexer tracks, but ingestion
advances a **separate cursor per contract**, and DEX pools are discovered at runtime
from a `pool_init` event rather than configured statically — so a pool begins at
cursor 0 and backfills while long-tracked contracts already sit at head. On the live
testnet indexer the global figure is ~5,023,590 while that pool's own logs stop at
~2,937,340: two million blocks of false confidence. The gate therefore reads
`contract_logs` filtered to the pool, and uses the global number only as a diagnostic
hint in the error message.

That proof is **one-sided**, and it matters that you know which side. Even scoped to
the pool it is the last log *written*, not the position *scanned*. So
`height >= epochEnd` proves sufficiency and passes; `height < epochEnd` is ambiguous —
either the indexer is behind on this pool, or the pool has simply been idle, which
means the data is complete. Nothing exposed distinguishes those, so the gate passes
only on proof and refuses otherwise. A pool with no logs at all is treated as
unproven, not empty: an undiscovered pool looks identical to an idle one.

On a low-traffic chain that will refuse epochs whose data is in fact complete. That is
what `indexer.allow_stale` is for. It defaults to **false**: an operator who wants
throughput should have to say so, rather than discover silent underpayment later.

**The indexer is configurable per operator, on purpose** — so nobody is forced onto
someone else's, and so the aggregation stays in the reporter. Do not "optimise" it by
moving the aggregation into SQL: a view that differs between deployments turns a
quorum into a silent byte-mismatch.

**But reporters sharing an Attest quorum should point at the SAME indexer.** An
earlier version of this document claimed different indexers were safe, on the grounds
that the reporter pins its arithmetic to explicit heights. That was wrong — the
heights are the indexer's, not ours. Its ingestion falls back between a transaction's
L1 anchored height and the state-output height when the `transaction_pool` lookup
misses, and that height is part of the per-event dedupe key. Two instances can
therefore assign different heights to the same event, place it either side of an epoch
boundary, and produce payloads that never merge to a threshold. Same indexer, same
answer.

This does mean LP rewards inherit a trust assumption on the indexer operator. It is
not a *new* one — the reporter was already trusted to submit an honest list, the
events derive from on-chain transactions, and Hasura is publicly queryable, so anyone
can recompute independently and a guardian can still veto during the challenge window.

### Verifying against a real indexer

Every other LP test uses a fake server, which cannot catch a renamed column or a
different table prefix — a fake written alongside the queries always agrees with
them. One opt-in test closes that, read-only, two SELECTs and no chain writes:

```bash
LPSRC_LIVE_INDEXER=https://api-testnet.okinoko.io/hasura/v1/graphql \
LPSRC_LIVE_POOL=vsc1Brm1QpGF8WXvRCvwgbpB6fiHtTBJzyZUC9 \
go test ./reporter/lpsrc/ -run TestLive -v
```

Run it whenever the indexer's schema might have moved. It asks for a single instant
(`Start == End`) rather than a span: over a span, `min()` correctly reports nobody
held LP at both ends of all history, which looks like a schema failure but is the
anti-flash rule working.

## Finalize is gated on confirmed pages

`finalizeEpoch` is irreversible: once an epoch closes, `submitShares` aborts, so any
account missing from the report can never be added and the accounts that did land
split the whole epoch between them.

Broadcasting is not executing. `Send` returns a Hive L1 txid, and there is no L2
tx-receipt query anywhere in this codebase — only state reads — so a share page can be
accepted by Hive and still revert inside the contract (RC exhaustion, a rejected
payload, an Attest threshold not yet met). Before sending finalize, the reporter
therefore re-reads `ssdone|<ep>|<page>` — which the contract writes only on the
committed path, making it a true receipt — and refuses unless every page is applied
and the epoch is funded.

What it does when pages are missing depends on why, because the same symptom is a
fault in one mode and routine in another:

| situation | behaviour |
|---|---|
| pages broadcast in this run | defer to the next run, exit 0 — they may still be landing |
| Attest mode, quorum not yet reached | defer, exit 0 — normal for every attester but the last |
| pages missing that were NOT sent this run | **hard error** — they reverted |
| chain unreadable | **hard error** — never treated as confirmation |
| epoch already finalized/cancelled | stop cleanly, no second finalize |

`submit.confirm_tries` × `submit.confirm_interval_sec` bound the wait (default 6 × 15s).

An epoch with **no** earners is refused before anything is broadcast: funding it would
move C2's slice into a distributor that can never finalize, recoverable only by a
guardian cancel that pays the treasury rather than any earner.

## The epoch sequence

An epoch pays out only if all four happen, in this order:

> **Call order.** Pages are submitted BEFORE `pullFunding`. `pullFunding` stamps the
> anchor for the guardian's stale-rescue deadline, so pulling first spends that whole
> window paginating — worst for a backlog epoch, where one refill can fund many epochs
> whose deadlines then coincide. `submitShares` needs no funding, so the clock starts
> as late as possible. `finalizeEpoch` still requires `funded>0` and stays last.

1. `C2.distributeEpoch` — keeper poke; DRAWS the epoch's emission from the approved
   pool (`token.transferFrom`; C2 does not mint) and records bucket `owed`.
   Permissionless and idempotent. Set `submit.keeper` to have the reporter do it, or
   leave it to a separate keeper. If the pool cannot cover a whole epoch the poke is
   a harmless no-op returning `{"distributed":"0","starved":true}` — top the pool up
   and the next poke resumes and pays the backlog.
2. `C3.pullFunding` — moves this epoch's slice from C2 into C3, recording
   `funded[epoch]`.
3. `C3.submitShares` × N pages — the report.
4. `C3.finalizeEpoch` — freezes the epoch and opens the challenge window.

The order is enforced by the contracts, not just convention: `finalizeEpoch`
aborts unless `funded > 0` **and** `totalShares > 0`, and `submitShares` aborts
once the epoch is finalized. So funding must precede shares and finalize must be
last.

## Idempotency and resume

`run` decides what still needs doing from **chain state**, not from its progress
file:

| step            | on-chain marker         |
|-----------------|-------------------------|
| pullFunding     | `funded\|<ep>`          |
| submitShares P  | `ssdone\|<ep>\|<P>`     |
| finalizeEpoch   | `status\|<ep>`          |

The progress file is an audit trail and is reported when it disagrees with the
chain. This is deliberate: a `pullFunding` that was broadcast but claimed 0 (because
the C2 poke had not landed yet) would otherwise be recorded as done and never
retried, leaving the epoch permanently unfundable. Every step is safe to repeat.

`run` stops at the first failure rather than continuing, because the calls are
ordered — pressing on would finalize an epoch that is missing pages.

## Epoch selection

The default target is the **oldest closed-but-unfinalized** epoch within a
20-epoch lookback, not the newest. That is what makes Attest mode work: N machines
running minutes apart across an epoch boundary would otherwise target different
epochs and never reach quorum. It also means downtime is caught up in order rather
than skipped.

Override with `-epoch N`. An epoch that has not closed yet is refused.

## When a post is scored (attribution)

`source.attribution` decides which epoch a post's rewards land in. This is the
most consequential setting in the file.

**`"cashout"` (default).** A post is scored in the epoch its Hive **payout** falls
in — 7 days after posting, when voting has closed. Every vote is counted exactly
once. Rewards lag one payout period behind posting, exactly as they do natively on
Hive and in SCOT.

**`"created"`.** A post is scored in the epoch it was posted in, using whatever
votes exist at the epoch boundary. Rewards are immediate, but the trade is real and
worth stating plainly: voting stays open for 7 days, so a post made in the last
minute of an epoch is scored with almost none of its votes while one made at the
start gets a full epoch's worth, and every vote cast after the boundary is counted
by nobody, ever. Offered for tenants who want promptness; it is not the default.

### The vote cutoff

Under either mode the vote set is frozen at a cutoff — the post's `payout_at` under
`cashout`, the epoch end under `created` — and votes after it are dropped.

This is not a detail. Hive keeps recording votes after a post pays out: a real post
that paid out on 2023-03-16 still took a vote on 2023-03-20. Without the cutoff,
recomputing an epoch later would see a larger vote set and produce different
numbers, which would **void the challenge window** (no verifier could reproduce the
report) and **break Attest quorum** (two machines running days apart would never
agree). The cutoff is what makes a report reproducible forever.

## Operational constraints

These are properties of Hive and the VSC node, all verified against live
infrastructure — not assumptions.

- **There is NO reporting deadline.** Vote detail is kept indefinitely: a post
  created 2023-03-09 and paid out 2023-03-16 still returns all 287 votes with
  `time`/`percent`/`rshares` 1234 days later. (An earlier version of this package
  claimed a 7-day pruning window and refused to score paid-out posts. That was
  wrong — the "Post ... does not exist" error came from querying with a permlink
  that had been truncated in a debug print.)
- **`bridge.get_ranked_posts` caps `limit` at 20.** Asking for more is a hard
  `Assert Exception: limit = N outside valid range [1:20]`, not a silent clamp, so
  every epoch is paged.
- **The community feed is not newest-first.** Pinned posts are hoisted to the top
  of page 1 regardless of age — a real community had four 2024/2025 posts above its
  newest one. Pinned posts are excluded from the "have we walked past the window"
  decision (they are still scored if they fall inside it).
- **Vote weights need `get_active_votes`.** The `active_votes` embedded in
  `get_ranked_posts` carry `rshares` but no `time` and no `percent`, so they cannot
  drive the curation curve (needs vote order), the vote cutoff (needs time), or
  `token_stake` weighting (needs percent). The extra per-post round trip is not
  removable.
- **`rshares` must not go through float64.** Values reach ~1e15 and float64 loses
  integer precision above 2^53 (~9e15). The transport decodes with `UseNumber`.
- **`getStateByKeys` is exact-key only, latest state only** (1..100 keys, no prefix
  scan, no historical view). Historical C1 stake is therefore read by walking C1's
  append-only checkpoint history with the same binary search the contract uses —
  `vscapi.StakeSource` is a deliberate bug-for-bug copy of C1's `searchVal`, and
  `stake_test.go` proves the two agree by differential test.
- **Page sizing.** `submitShares` payloads are capped at 4096 bytes by the `auth`
  module and each entry costs roughly 80-95 RC to apply. Defaults are 60 entries /
  3800 bytes; `page.max_bytes` above 4096 is rejected at config load.

## Vote weight modes

- `hive_rshares` (default) — use Hive's own `rshares`, which already has the voter's
  stake and vote-mana applied. Voting power reflects **HIVE** stake.
- `token_stake` — a vote's weight is the voter's **C1-staked balance** at the epoch's
  end block, scaled by the vote percent. Voting power reflects the **tribe's own
  token**. Requires `contracts.stake`.

## Keys

There is no key in the config file. The active WIF is read from the environment
variable named by `submit.wif_env` (default `REPORTER_ACTIVE_WIF`), so a config can
be committed and shared across an Attest quorum without leaking signing authority.

Signing is always with **active** authority and `required_posting_auths` is always
empty. The framework's `auth.RequireActive` rejects a caller present only in the
posting auths, because the VSC runtime derives `msg.caller` from
`RequiredPostingAuths[0]` when no active auth is present — accepting posting keys
would let a posting key satisfy the reporter role.

## Attest (multi-machine) mode

When the distributor's reporter authority is configured in Attest mode, run this
service on N machines with the **same config** but each machine's own account and
key. Determinism guarantees the pages are byte-identical, so the contract's
threshold is reached without the machines coordinating. Nothing else changes.

## End-to-end verification

Two tests connect the reporter to the contracts, because until they existed each
half was only checked in isolation — contract tests hand-wrote their shares string
and reporter tests stopped at producing a page:

- **`itest/seam_test.go`** (in-process, seconds). Realistic Hive data goes in,
  the real pipeline runs, and `plan.Calls[i].Payload` is handed to the real wasm
  engine verbatim — no payload is written by hand. Asserts the contract's
  `totalShares` equals the reporter's, and that every claim pays exactly
  `funded*share/totalShares`.
- **`tests/devnet/magi_reporter_devnet_test.go`** (docker multi-node, ~30 min).
  Runs the actual `reporter` binary against a local Hive JSON-RPC server serving
  injected post/vote data, then broadcasts its planned payloads to a live chain and
  claims against them.

The seam test immediately paid for itself: `applyEntries` used the permissive
`validateAddr`, so an entry with no ledger domain (`alice:100`) was **counted into
totalShares and then unclaimable forever** — measured at 50% of an epoch's funding
silently stranded, with the remaining claimant diluted. C3/C5 now skip
domain-less share recipients, and C6 does the same for airdrop recipients.

## Tests

```sh
GOTOOLCHAIN=go1.25.3 go test ./reporter/... -count=1
```

74 offline tests, no network and no keys required — the Hive and VSC layers are
behind `Transport` / `StateReader` interfaces.

A live smoke test against real nodes is skipped by default:

```sh
REPORTER_LIVE=1 GOTOOLCHAIN=go1.25.3 go test ./reporter/hivesrc/ -run Live -v
```

It is worth running after any change to `hivesrc`: several of the constraints
listed above were found *only* by running it, and each is now pinned by an offline
regression test in `hivesrc/regression_test.go`.
