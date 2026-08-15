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

## Serving proofs (`proofsvc`)

The chain no longer holds per-account shares — per-account state writes were ~92%
of an epoch's cost, so the distributor commits a merkle root and logs the leaves.
That makes this indexer the only thing that can answer "what did this account
earn?", and `proofsvc` is what answers it.

```
go build ./indexer/proofsvc/cmd/proofsvc
HASURA_ENDPOINT=https://…/v1/graphql ./proofsvc -addr :8099
```

- `GET /proof?channel=content&epoch=0&account=hive:alice` → the account's share,
  its merkle path, and a ready-to-send `claim_payload`.
- `GET /root?channel=content&epoch=0` → what this service rebuilt, plus every
  entry the contract skipped. Compare it against the chain without asking for
  anybody's proof.
- `GET /health` → process liveness only. Deliberately not tied to whether an epoch
  is servable: coupling them would take the service down exactly when the indexer
  lags and operators most need it answering.

**This service is not trusted, and does not ask to be.** It rebuilds each epoch
from the `shares` logs, recomputes the root, and compares it against the
`magi_tokenomics_root_events` row the contract committed. On any disagreement it
refuses to serve rather than handing out proofs that would be rejected at the point
of payment — the worst place to discover the database was wrong. Four disagreements
are reported separately because they mean different things:

| Symptom | What it means | What to do |
|---|---|---|
| `page N is missing` | the indexer is behind, or a `submitShares` log was dropped | let it catch up; if it persists, the log is gone |
| `does not match what the contract counted` | that row's copy is corrupt | re-ingest the epoch |
| `does not match the committed root` | the book is wrong somewhere | re-ingest; if it survives that, the reporter and the chain disagree |
| `rebuilt total …` / `holds N accounts` | the denominator differs from the chain's | as above |

A claimant never has to trust it either: `reporter proof -account X` recomputes the
same epoch from public Hive data and needs no indexer at all. The two must agree,
and if they do not, the chain's root decides.

Entry rules (which addresses count, what gets skipped) come from
`reporter/sharecore.ParseEntries` — the single mirror of the contract's
`applyEntries`, pinned to it by `TestDevnetDrift_LedgerDomainsMatchTheContract`.

### What is and is not verified

`TestProofsvc_ServesOverHTTPAgainstHasuraProtocol` runs the real service over real
HTTP against a server speaking Hasura's protocol, filled with rows taken from logs
the contract actually emitted — then pays a claim out with the `claim_payload` that
came back over the wire. It also checks that a rejected credential surfaces as an
error: Hasura answers a bad secret with **200 and an `errors` array**, and an
unchecked errors array becomes an empty result set, which reads exactly like "the
indexer is behind".

`TestIndexerMappingPathsResolveAgainstRealEvents` resolves every `$.attributes.*`
path in the mappings above against real emitted events. A path naming a field the
contract does not emit yields a NULL column, and downstream a NULL is
indistinguishable from "not reported yet".

**Not verified here:** magi-mongo-indexer itself is not run. These prove the mapping
describes what the chain emits, and that the service works against the protocol —
not that mongo ingests them. That step needs a deployment.
