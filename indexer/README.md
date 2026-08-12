# Indexing this framework

The three contracts emit a log for every state transition, in the format
[`magi-mongo-indexer`](https://github.com/tibfox/magi-mongo-indexer) ingests. Copy
[`magi_tokenomics_mappings.yaml`](magi_tokenomics_mappings.yaml) into that repo's
`internal/config/events/` and restart the indexer — it backfills from MongoDB and then
follows new blocks. Hasura exposes the tables as GraphQL with no further work.

Nothing in the file needs editing per deployment. Each contract announces itself with
an init event and the indexer adopts it, the same way it adopts DEX pools via
`pool_init`. A new tribe launches and its tables fill.

## The format

`magi_token-contract` is the reference and this follows it exactly:

```json
{"type":"stake","attributes":{"acct":"hive:alice","via":"stake","amount":"600", … }}
```

A `type` discriminator the indexer keys its mapping on, everything else under
`attributes`, and **every value a JSON string** — including amounts, epochs and
heights. That last part is not stylistic. The indexer decodes with `encoding/json`
into `map[string]interface{}`, so a bare JSON number becomes a float64 and
`18446744073709551615` would arrive as `18446744073709552000`. The `numeric` columns
in the mapping receive the string and cast it. `events.Big`, `U64`, `Int` and `Bool`
in [`../events/events.go`](../events/events.go) all write strings for this reason;
there is no way to emit a bare number through that package.

`itest/events_test.go` asserts both properties on live logs from a full slice, so a
regression fails the suite rather than surfacing as silently rounded amounts weeks
later.

## What each contract tells you

| contract | events |
|---|---|
| **C1** | `c1_init` `stake` `unstake` `unstake_claim` `drawdown` `yield_funded` `yield_claim` `yield_sweep` `airdrop` `sweep_unobligated` `schedule_adopted` `skip` |
| **C2** | `c2_init` `emit` `poke` `alloc` `bucket_claim` `tokenop` |
| **C3** | `c3_init` `channel` `pull` `shares` `skip` `epoch_status` `sweep_unallocated` `claim` |

Every payout event carries its own arithmetic — `claim` ships `share`, `total_shares`
and `funded` alongside `payout`, and `yield_claim` ships `stake_used` and
`denominator` — so a claimant can check what they were paid rather than take the
number on faith.

`drawdown` is the one to keep if you keep nothing else. The accumulator telescopes
across every stake mutation inside an epoch, so it is the only part of C1's state an
observer cannot recompute from the outside, and `claimYield` divides by it.

## Two things to watch

**The `skip` tables are alerts, not diagnostics.** `magi_tokenomics_share_skip_events`
and `magi_tokenomics_airdrop_skip_events` record entries a contract dropped — a
reporter account with no ledger domain, a zero share, a malformed airdrop line. The
transaction **succeeded**: the page applied, the totals look right, and that earner is
simply never paid. Nothing else on chain records it. A non-empty skip table means
someone is owed money and a corrected page or batch needs sending.

**Logs survive only successful transactions.** An aborted call leaves nothing behind,
so none of this helps diagnose a failure — which is exactly why the skip events matter:
a silent drop inside a committed transaction is the one failure mode a log can catch.

## Cost

Emission adds 100–350 RC to writing entrypoints and nothing to queries. Two paths are
shaped to keep it flat rather than per-item, both measured in
[`../docs/rc-costs.md`](../docs/rc-costs.md):

- **`submitShares`** emits one log per page carrying the submitted entries verbatim,
  not one per entry. Per-entry events were built and measured first: they took the
  marginal rate from 91 to 229 RC/entry and pushed a 60-entry page past the free tier.
  Applied = submitted − skipped, so the share book is still exactly reconstructible.
- **`airdropBatch`** emits one summary per batch. The liquid path's per-recipient
  record already exists as the token contract's own indexed `transfer` log, and the
  `airdropStaked` path emits `stake` per recipient anyway.
