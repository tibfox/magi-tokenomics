# Devnet tests (docker multi-node)

These are the end-to-end tests that run the framework against a **real** VSC devnet:
HAF + Mongo + multiple magi nodes in docker, with real Hive block production.

They live under `testdata/` because they cannot compile inside this module. They are
`package devnet` — part of go-vsc-node's own devnet harness — and use its unexported
internals (`d.cfg`, `d.witnessAccount`, …), so they must sit *inside* that package.
`testdata/` is ignored by the Go tool, so keeping them here preserves them under
version control without breaking `go build ./...`.

## These runs are namespaced — keep it that way

`magiDevnetConfig()` sets `cfg.ProjectName = "tokdevnet-<rand>"`. The harness would
otherwise default to `devnet-test-<rand>`, which is the same namespace every other
go-vsc-node checkout on a machine uses — including a second agent session running the
market contracts' devnet concurrently.

Clean up by matching **that prefix and nothing else**:

```sh
# 1. LIST first, and read it.
docker ps -a --filter "label=com.docker.compose.project" \
  --format '{{.Label "com.docker.compose.project"}} {{.Names}}' \
  | awk '$1 ~ /^tokdevnet-/ {print $2}'

# 2. Only then remove, by piping the SAME command into rm.
docker ps -a --filter "label=com.docker.compose.project" \
  --format '{{.Label "com.docker.compose.project"}} {{.Names}}' \
  | awk '$1 ~ /^tokdevnet-/ {print $2}' | xargs -r docker rm -f
```

**Do not put `-q` on that `docker ps`.** `-aq` silently overrides `--format`
(`WARNING: Ignoring custom format, because both --format and --quiet are set`) and
emits bare container IDs. The project label the awk pattern tests is then gone, so
`$1` is a hex ID, `/^tokdevnet-/` matches nothing, `{print $2}` prints nothing, and
`xargs -r` runs nothing at all. This snippet had that bug and was therefore a silent
no-op: it never removed anything, and a devnet was found still running eleven hours
after its `go test` had exited. It failed *safe* — an over-broad selector is the far
worse failure, and this one could never have reached production containers — but a
cleanup command that quietly cleans nothing still costs a host full of orphans.

Two rules learned the hard way, both from real damage on this host:

- **Never select containers by name substring.** `--filter name=magi-` matches
  `magi-deployer-contractdeploy-*` — the production contract deployers — and removed
  them. The prefix here deliberately contains no "magi" for that reason.
- **Never select by the `devnet-test-` prefix either.** That is the shared default,
  so it matches other people's concurrent runs.
- **List the selector before you pass it to `rm`.** Run it with
  `--format '{{.Names}}'` and read the output first. Both mistakes above were caught
  that way, one of them before it did any harm.

## Before you run them: two things that are not in this directory

**`waitStateKeyPresent` was missing until 2026-08-18.** Eleven suites call it and it
was never versioned here — it existed only inside one go-vsc-node checkout, so a
clean copy of this directory failed `go vet` with "undefined: waitStateKeyPresent".
That is why these went unrun long enough for seven of them to rot. It now lives in
`magi_merkle_helper_test.go`. If you add a helper, add it *here*, not to your
checkout.

**`magi_upgrade_devnet_test.go` needs a go-vsc-node with contract-update support**
in its devnet harness — `Devnet.UpdateContract`, `ContractUpdateOpts` and
`Devnet.PendingUpdates`. They live in `tests/devnet/contract_timelock.go` on the
branch **`tibfox/feat/contract-timelock`** (not on main). Against a checkout without
them the whole `devnet` package fails to compile, so move that one file aside if you
are running the others.

This note used to name `feat/contract-update-timelock`, which does not exist, and the
suite went unrun for months on the strength of it — the branch had been fetched in the
working checkout the whole time. Get it with a WORKTREE rather than a checkout, so a
checkout carrying someone else's work in progress is left alone:

```sh
git worktree add /path/gvn-timelock tibfox/feat/contract-timelock
cp testdata/devnet/magi_*_test.go /path/gvn-timelock/tests/devnet/
cd /path/gvn-timelock && go test -v -run TestDevnetMagiUpgrade -timeout 75m ./tests/devnet/
```

## Running them

```sh
# 1. build this framework's artifacts + the reporter binary
cd /path/to/magi-tokenomics
for c in c1-staking c2-emission c3-distributor; do
  GOTOOLCHAIN=go1.25.3 tinygo build -gc=custom -scheduler=none -panic=trap \
    -no-debug -target=wasm-unknown -o $c/artifacts/main.wasm ./$c/contract
done
GOTOOLCHAIN=go1.25.3 go build -o reporter/bin/reporter ./reporter/cmd/reporter

# 2. drop the tests into a go-vsc-node checkout
cp testdata/devnet/magi_*_test.go /path/to/go-vsc-node/tests/devnet/

# 3. point them at your checkouts and run
cd /path/to/go-vsc-node
export MAGI_FRAMEWORK_DIR=/path/to/magi-tokenomics
export MAGI_TOKEN_WASM=/path/to/magi_token-contract/test/artifacts/main.wasm
go test -v -run TestDevnetMagiFull -timeout 60m ./tests/devnet/
```

Both env vars have defaults pointing at the original development machine; set them
to your own paths.

## The twelve suites

| test | covers | ~time |
|---|---|---|
| `magi_tokenomics_devnet_test.go` | C0 + C2 + C3, then 11 outsider attacks | 10 min |
| `magi_stake_lp_airdrop_devnet_test.go` | staking + yield + airdrop (C1) and an LP channel, then outsider attacks | 18 min |
| `magi_reporter_devnet_test.go` | the real `reporter` binary driving C3 against injected Hive data | 12 min |
| `magi_full_devnet_test.go` | **all 4 contracts + the reporter, then 14 staked-holder + 29 outsider attacks** | 40 min |
| `magi_rogue_reporter_devnet_test.go` | the **trusted** reporter role turning malicious: fraud + guardian veto + Attest quorum | 22 min |
| `magi_multiepoch_devnet_test.go` | **operation over time**: keeper catch-up, flat emission, per-epoch isolation, stake history, unstake maturity | 30 min |
| `magi_refill_devnet_test.go` | **batched minting**: pool drained to a standstill, refilled, backlog paid in full | 17 min |
| `magi_lp_multiepoch_devnet_test.go` | **LP rewards**: 3 epochs via the real reporter in `lp` mode against a faked indexer | 24 min |
| `magi_realbroadcast_devnet_test.go` | **the reporter signs and submits its own epoch** — the only suite where the harness does not broadcast — then re-runs it to prove a resume broadcasts nothing | 16 min |
| `magi_cosigned_devnet_test.go` | **auth mode 1 (Cosigned)**: 2-of-2 in ONE transaction, and one authority applying nothing | 13 min |
| `magi_scale_devnet_test.go` | **volume + cost**: 500 users, 200 posts, 10,000 votes in one epoch — what an epoch COSTS and whether anyone is silently dropped across 9 share pages | 20 min |
| `magi_upgrade_devnet_test.go` | contract-update timelock — **does not compile against a stock go-vsc-node**; see the note above and move it aside | — |

`magi_full_devnet_test.go` is the one to run if you only run one. It proves a single
emission splitting three ways into three *different* distributor mechanisms at once,
with exact end-to-end balance conservation — and then attacks it from two directions.

**Three attacker profiles, because they probe different depths.** A pure outsider (no
stake, no share, no tokens) is the easy case: most calls die on the first authority
check. A *staked holder* is the realistic insider — real stake in C1, real shares in
the distributor, claims already collected — so their attacks (double-claim, epoch aliasing,
early withdrawal, sweeping) reach far deeper into the logic before anything refuses.
Both phases refuse to run if their attacker lacks the standing that makes the phase
meaningful.

`magi_rogue_reporter_devnet_test.go` covers the third and hardest profile: the one
role the system actually trusts. A reporter is *supposed* to publish share lists, so
nothing stops it publishing a false one — the test asserts the fraud **is accepted**,
because containment is the guardian and the quorum, not the submission path. It then
proves the fraud is contained: the guardian cancels inside the challenge window, the
funding rolls forward to `unallocated` rather than being stolen or stranded, the
rogue claims nothing, and the guardian recovers it to the **pinned** treasury. In
Attest mode (2-of-3) it proves a single rogue cannot reach threshold, cannot
equivocate (one vote per authority per action, so backing a second payload is
refused), and cannot stop the two honest reporters committing *their* payload.

**The attacker account must be funded, and the funding must be PROVEN.** RC capacity
comes from the account's VSC-ledger HBD balance, and a spend only thaws back over
~5 days — so an unfunded attacker runs out mid-sweep and the remaining attacks fail
with *insufficient RC* instead of *not authorised*: a false pass, green while proving
nothing.

Three things are needed, and only doing the first is not enough:

1. Deposit in **repeated descending rounds**, so an account moves as much L1 balance
   as it has rather than stopping at the first amount that happens to succeed.
2. **Poll until the deposits credit the L2 ledger.** They are L1 transfers to the
   gateway and settle asynchronously; a fixed sleep is not enough, and a 25s one
   leaves `hbd=0` on every account. The suites poll for up to 4 minutes and fail if
   it never lands.
3. **Assert the resulting headroom.** The tests log
   `ledger balance hive:X hbd=N -> RC ~N+10000` for every actor and fail if the
   attacker lacks the capacity for the whole sweep. The 34 attacks need roughly
   48,000 RC back to back, with no time to thaw; a healthy run puts the attacker near
   `hbd=100000` → RC ~110,000.

`N/N attacks reached the chain` is also logged, so a regression in any of this is
visible rather than silent.

Equally, the pre-attack snapshot is checked for emptiness before the sweep runs: a
baseline of empty strings compares equal to anything, so a mistyped state key would
turn every "nothing moved" assertion into a no-op.

Most reporter tests serve dummy Hive data from a local `httptest` JSON-RPC server, so
they need no Hive access — but they do talk to the devnet's real GraphQL endpoint and
broadcast real transactions to the devnet chain.

`magi_realbroadcast_devnet_test.go` is the exception, and the reason is worth knowing
before you try to extend it. There the *reporter* signs and broadcasts, and it uses
ONE endpoint list for both reads and broadcasts — so it cannot read from a fixture and
submit to the chain at the same time. That is why the suite runs in **LP mode**: LP
reads come from the indexer, leaving Hive needed only for the head block, which the
devnet's real node answers on the same endpoint it accepts transactions on. Content
mode has no such split and stays fixture-driven, with the harness broadcasting.

### `magi_refill_devnet_test.go`

Proves a drained pool can be REFILLED — the property that makes batched minting
viable. Pool is sized to exactly one epoch's emission, so epoch 0 funds and epoch 1
starves regardless of how many epochs really elapsed when each poke lands; devnet
wall-clock timing is not controllable, so the dependency is designed out rather than
raced. Deploys only the token and C2 (the bucket pays a plain hive account), and never
hands ownership to C2 — `mint` is owner-only, so the handover would make the refill
impossible. The test fails if anyone reintroduces it.

That used to make this suite the exception. It no longer is: **no suite hands the
token to C2**, because C2 has no entrypoint that could use ownership since the guardian
passthrough was removed, and a handover would strand `mint`/`pause`/`changeOwner`
permanently. Refill was simply the first to need what is now the rule everywhere.

## Verification status

Every suite here has been run to completion on a real devnet, not merely compiled.
Keep this table honest as the code moves: a suite that stops funding a pool now funds
nothing and fails several minutes later with a misleading message, so a stale "yes"
is worse than no entry at all.

**Record the DATE, not just the runtime.** A bare number cannot be checked against
the code it ran on, which is how this table silently went stale: its runtimes were
written on 2026-07-31, the six-into-three merge landed on 2026-08-04, and nothing in
the table said so. A reader saw ten green rows for a layout none of them had run.

| suite | run green on devnet | runtime | last verified |
|---|---|---|---|
| `magi_full_devnet_test.go` | yes | 1710s | **2026-08-23** |
| `magi_multiepoch_devnet_test.go` | yes | 2060s | **2026-08-21** |
| `magi_rogue_reporter_devnet_test.go` | yes | 1509s | **2026-08-21** |
| `magi_tokenomics_devnet_test.go` | yes | 598s | **2026-08-20** |
| `magi_stake_lp_airdrop_devnet_test.go` | yes | 992s | **2026-08-21** |
| `magi_refill_devnet_test.go` | yes | 1049s | **2026-08-21** |
| `magi_lp_multiepoch_devnet_test.go` | yes | 1269s | **2026-08-23** |
| `magi_reporter_devnet_test.go` | yes | 788s | **2026-08-23** |
| `magi_realbroadcast_devnet_test.go` | yes | 727s | **2026-08-23** |
| `magi_cosigned_devnet_test.go` | yes | 733s | **2026-08-21** |
| `magi_scale_devnet_test.go` | yes | 1296s | **2026-08-23** |
| `magi_upgrade_devnet_test.go` | yes | 456s | **2026-09-02** |

**Every runnable suite — all eleven — has been run against the build with no C2 token
authority, and every runtime in this table was measured on that build.** Nothing here
is inherited from an earlier sweep any more. 2026-08-20: `magi_tokenomics` (597.61s),
`magi_realbroadcast` (939.34s), `magi_full` (1679.36s). 2026-08-21:
`magi_cosigned` (732.70s), `magi_reporter` (757.27s), `magi_stake_lp_airdrop`
(991.91s), `magi_refill` (1048.50s), `magi_scale` (1051.52s), `magi_lp_multiepoch`
(1330.68s), `magi_rogue_reporter` (1508.65s), `magi_multiepoch` (2059.71s).

**All twelve now run.** `magi_upgrade` needs the timelock branch in a worktree (see
the note above); everything else runs against a stock checkout.

**`magi_rogue_reporter` went red and is green again.** It failed on 2026-08-21 with
`rogue ended up with 1949999 (started 950000)`: the rogue is also the deployer, so
once the token stopped being handed to C2 it OWNED the token, and the "reporter mints"
attack the suite exists to refuse became an authorised call — +999,999, exactly the
attack payload. Fixed by giving the token to the TREASURY at bootstrap: a real account
that is not a reporter, which keeps `mint` owner-only and out of the rogue's reach
without handing it to a contract that could never use it. Green at 1508.65s.

If you write a suite where an attacker is also the deployer, say so at the top and
check every owner-only assertion against it. This one held for months only because a
handover happened to move the token out of the attacker's hands, and nothing recorded
that the coverage depended on it. The rows marked
"2026-08-19 sweep" are from that sweep's record rather than a run timed here — treat
their runtimes as indicative. `magi_stake_lp_airdrop` had been sitting at "pending
re-run on the merged layout" since 2026-08-04; it now passes on the merged layout,
with all 13 outsider attacks broadcast and rejected on chain and C5/C6/C7 state
unchanged afterwards.

Note its log labels still say C5/C6/C7. They are variable names that outlived the
merge: `c5ID` is the `c3-distributor` wasm, and every `c6.*`/`c7.*` call is sent to
`c1ID`, which absorbed the airdrop and the yield. The suite deploys FOUR wasm — token,
c1-staking, c2-emission, c3-distributor — so it is on the current layout despite
reading otherwise.

**Re-run 2026-08-23 after the reporter changed.** Five suites drive the reporter
BINARY, so their greens said nothing once it was rebuilt: `json_metadata` decoding,
per-block pacing, the policy digest and the chunked backlog scan all landed that day.
All five were re-run and all five pass — `magi_scale` matters most of them, because
its 9-page book is roughly the size that first hit Hive's 5-ops-per-block cap, and it
is the pacing fix's real proof.

A suite that runs a BINARY is only as verified as the binary it ran. The
`ReporterBinaryIsNewerThanItsSources` guard catches a STALE binary; nothing catches a
freshly-built one that no suite has exercised yet.

**Do not mark a suite verified by reasoning about its source. Run it.**

Two traps make source-reading unreliable here — either will leave a suite looking
fine while it proves nothing:

- **A suite's calls are not all in its source.** `magi_reporter_devnet_test.go` never
  writes `distributeEpoch` or `pullFunding` — they come from the **reporter's
  generated plan** at runtime. Grepping the file for them finds nothing and proves
  nothing. Every suite must mint and approve a pool before the ownership handover, or
  emission funds nothing and the run dies minutes in at `epoch 0 was never finalized
  on chain`.
- **Assert balance DELTAS, never absolutes.** The deployer holds the pool *and* earns
  a share, so an absolute check reads 973286 for a 3286 payout — the other 970000
  being undrawn pool. A passing run states both halves plainly:
  `magi.test1 claimed exactly 3286 (balance 970000 -> 973286)` beside
  `magi.test2 claimed exactly 537 (balance 0 -> 537)`.

Keep this table honest as the code moves on: "patched and compiles" is not
"verified", and this table is the only place that distinction is recorded.

### `magi_lp_multiepoch_devnet_test.go`

Three epochs of LP rewards driven by the real `reporter` binary in `source.kind: "lp"`.
The indexer is faked with an httptest GraphQL server, exactly as the content reporter
suite fakes Hive; everything else — C2, the distributor, submission, claims — is the live chain.

The fixture gives each epoch a **different** correct answer, so a pass means the
`min(LP(start), LP(end))` rule actually discriminates rather than one assertion
passing three times:

| provider | e0 | e1 | e2 | why |
|---|---:|---:|---:|---|
| steady | 1000 | 1000 | 1000 | in before the run, never moves |
| grower | 1000 | 1000 | 4000 | tops up mid-epoch-1 → counts from e2 only |
| exiter | 1000 | 0 | 0 | exits mid-epoch-1 → forfeits that epoch |
| flash | 0 | 0 | 0 | in and out inside e1 |
| **total** | 3000 | 2000 | 5000 | |

Event heights are positioned relative to the real `cfg_genesis` read off-chain.
`page_size: 2` forces the indexer paging walk (26 queries in the passing run). Claims
are asserted as deltas because the deployer also holds the undrawn pool.

The fake indexer also answers `indexer_health`, advancing in step with the chain,
because the reporter refuses to score an epoch the indexer cannot be proven to have
passed. A stand-in that ignored that query would be refused — as it should be.

**What it cannot catch:** a mismatch between the reporter's queries and the REAL
Hasura schema. A fake server written alongside the queries will always agree with
them. That needs one read-only `reporter compute -epoch N -json` against a live
indexer.

## Writing a multi-epoch test

`magi_multiepoch_devnet_test.go` took five runs to get right, and every failure was
the test rather than the contracts. The lessons generalise:

- **You cannot place a transaction in a chosen epoch.** Doing so means predicting
  block timing across every preceding call. A top-up intended for "early epoch 1"
  landed in epoch 2. Assert across a *range* instead — compare an epoch that is
  unambiguously before a change against one unambiguously after.
- **You cannot predict how many epochs will elapse.** A run's own transactions
  consume real chain time: this one spends ~10 minutes ≈ 6 epochs at
  `epochLen=30`. Asserting "exactly 3 epochs minted" failed a perfectly healthy
  system reading 183000. Derive the expectation from the chain — read C2's
  `cfg_lastEpoch_v` and assert `supply == bootstrap + (lastEpoch+1) x emission`,
  which proves flat emission over however many epochs actually ran.
- **`epochLen` must exceed one epoch's processing time.** Each devnet call costs
  ~9s and an epoch's pull/shares/finalize is ~6 calls (~54s), so a 10-block (30s)
  epoch makes intra-epoch placement impossible in principle.
- **Changing `epochLen` changes the emission**, since it is
  `baseAnnual * epochLen / blocksPerYear`. Tripling the epoch tripled every expected
  figure.

## The emission pool (allowance model)

C2 does not mint. It draws each epoch from an account that has `approve`d it, so
every suite must, **before handing the token to C2** (only the owner may mint):

```go
token.mint    {"amount": POOL}
token.approve {"spender": "contract:<C2>", "amount": POOL}
```

Get this wrong and `distributeEpoch` funds nothing — the symptom is
`owed never reached N (last "")`, several minutes after the actual mistake.

Two knock-on effects when writing assertions:

- **`totalSupply` does not track emission.** It is constant once the pool is minted,
  because C2 draws from the pool rather than minting. Measure the pool holder's
  falling balance, or C2's rising one, instead.
- **Keep the pool holder separate from any participant**, or its balance mixes
  undrawn pool with earned rewards. The full-system suite conflates them (the
  deployer holds the pool and also stakes), which is why its conservation check
  measures a claim *delta* rather than an absolute balance.

## Cleaning up between runs

Remove containers **and networks and volumes**, not just containers:

```bash
docker rm -f $(docker ps -aq --filter "name=devnet-test-")
docker network ls --format '{{.Name}}' | grep devnet-test- | xargs -r docker network rm
docker volume ls --format '{{.Name}}' | grep devnet-test- | xargs -r docker volume rm -f
docker images --format '{{.Repository}}:{{.Tag}}' | grep '^devnet-test-' | xargs -r docker rmi -f
rm -rf <go-vsc-node>/.devnet
```

Removing only containers leaves a network holding the published ports, and the next
run dies during setup with `Bind for 127.0.0.1:18057 failed: port is already
allocated` — which looks like a code failure and is not one.

## Operational notes

These cost real resources and have sharp edges. All of these were learned the hard
way; ignoring them wastes 10–25 minutes per run.

- **Each run builds a ~766MB `devnet-test-<id>-magi` docker image and never removes
  it.** Seventeen of these accumulated once and filled the disk mid-run. Clean up
  between runs:
  ```sh
  docker rm -f $(docker ps -aq --filter "name=devnet-test-")
  docker images --format '{{.Repository}}:{{.Tag}}' | grep '^devnet-test-' | xargs -r docker rmi -f
  docker builder prune -f && rm -rf .devnet
  ```
- **Deploy before depositing.** Each deploy costs 10 HBD of the deploying account's
  L1 balance; depositing first starves it.
- **A contract must be initialised by the node that deployed it** — the deployer
  becomes `contract.owner` and `init` aborts otherwise. The tests carry an
  `ownerNode` map for exactly this.
- **Never assert on chain state straight after broadcasting.** Two distinct traps,
  both of which have cost real runs here:
  - *Aggregates grow.* `total_staked`, `owed|`, `funded|` and `totalShares|` appear
    as soon as the **first** contributing transaction lands and then keep growing,
    so waiting merely for the key to exist samples a partial value (this once read
    `total_staked=600` while the second stake was still in flight). Use
    `waitValue(id, key, want)`, which polls for an exact value.
  - *Per-actor state needs a per-actor wait.* Waiting for **one** account's
    `claimed|<ep>|<acct>` marker says nothing about the others: a balance read a few
    seconds after a second account's claim is indistinguishable from that claim
    being rejected (this reported `honest C should hold 2000, has 0` when C's claim
    was simply still in flight). Loop `waitStateKeyPresent` over **every** actor
    before comparing any balance.

  Using the helper is not enough on its own — it has to cover every actor and every
  key you are about to assert on.
- **Stake before initialising C2.** C2's `genesis` is the block it initialises at,
  and yield credits `min(stakeAt(start), stakeAt(end))` — so stake arriving later is
  zero at both epoch-0 boundaries and that epoch's yield is unclaimable.
- **`C1.adoptSchedule` after `C2.init`.** It arms both the per-epoch drawdown
  accumulator (the exact `Σ min(aᵢ,bᵢ)` yield denominator) and the bucket yield is
  funded from. Neither can be given at C1's own init, because C2's genesis and buckets
  do not exist yet. `pullFunding` refuses until it has run.
- **`addChannel` per reward stream**, after `C2.init` — registering a channel verifies
  its bucket against the funder, so it cannot happen earlier.

## The drift guards, and why they exist

A devnet run costs 11–40 minutes and stands up a five-node chain, a Hive replica and
an indexer before it can tell you that a test read the wrong state key. Every guard in
`itest/devnet_drift_test.go` exists because a specific run was spent discovering
something a few milliseconds of parsing could have found. They run with the ordinary
`go test ./itest/`, need no docker, and each one names the run it came from.

| Guard | The run it cost |
|---|---|
| `EveryReferencedActionExists` | an action renamed in the contract |
| `ActionsExistOnTheContractTheyAreSentTo` | `claim` sent to C1, which does not export it |
| `EveryReferencedStateKeyExists` | a renamed key read as `""` forever — extended after `magi_rogue_reporter` waited on `unallocated` while the contract writes `unalloc\|<ch>` |
| `EveryLookupMatchesAKeyThatWasRead` | an assertion comparing `""` to `""` |
| `OwnerOnlyActionsAreCalledFromTheOwnerNode` | an owner-only call sent from the wrong node |
| `ChannelScopedCallsAndKeysCarryAChannel` | channel-less claims and keys after the merge |
| `ReporterConfigKeysExist` / `ReporterBinaryIsNewerThanItsSources` | a stale binary reporting yesterday's behaviour |
| `ArtifactsAreNewerThanSources` | a run against a `.wasm` older than its `main.go` |
| `LedgerDomainsMatchTheContract` | `system:` counted on chain, dropped from the root |
| `EveryReporterSubcommandTheSuitesUseAcceptsConfig` | `reporter root` rejecting `-config`, 17 minutes in |
| `EveryClaimCarriesAShareAndProof` | six suites still claiming with the pre-merkle payload |
| `ClaimedKeyIsNeverReadAsAShare` | two suites computing a payout from a presence flag |
| `TotalSharesIsOnlyReadWhereARootIsSubmitted` | `magi_cosigned` asserting against a key nothing would write |

**Mutate a guard before trusting it.** Four times now a guard here has passed against
a file that contained the exact defect it claimed to catch — a fixed-width scan that
read the next table row, a resolver blind to method calls, a cycle guard that memoised
the visit instead of the answer, and a field check satisfied by a struct tag. A guard
is only evidence once you have reverted the fix and watched it fail. The same applies
to the suites themselves: `magi_cosigned`'s central assertion — that one authority of a
2-of-2 pair applies nothing — held whether the threshold worked or was bypassed
entirely, because it read a key that would never be written either way.
