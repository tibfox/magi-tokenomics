# Devnet tests (docker multi-node)

These are the end-to-end tests that run the framework against a **real** VSC devnet:
HAF + Mongo + multiple magi nodes in docker, with real Hive block production.

They live under `testdata/` because they cannot compile inside this module. They are
`package devnet` — part of go-vsc-node's own devnet harness — and use its unexported
internals (`d.cfg`, `d.witnessAccount`, …), so they must sit *inside* that package.
`testdata/` is ignored by the Go tool, so keeping them here preserves them under
version control without breaking `go build ./...`.

## Running them

```sh
# 1. build this framework's artifacts + the reporter binary
cd /path/to/magi-tokenomics
for c in c1-staking c2-emission c3-distributor c5-lp c6-migration c7-yield; do
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

## The six suites

| test | covers | ~time |
|---|---|---|
| `magi_tokenomics_devnet_test.go` | C0 + C2 + C3, then 13 outsider attacks | 10 min |
| `magi_c5c6c7_devnet_test.go` | C1 + C2 + C5 + C6 + C7, then outsider attacks | 18 min |
| `magi_reporter_devnet_test.go` | the real `reporter` binary driving C3 against injected Hive data | 12 min |
| `magi_full_devnet_test.go` | **all 7 contracts + the reporter, then 14 staked-holder + 34 outsider attacks** | 30 min |
| `magi_rogue_reporter_devnet_test.go` | the **trusted** reporter role turning malicious: fraud + guardian veto + Attest quorum | 22 min |
| `magi_multiepoch_devnet_test.go` | **operation over time**: keeper catch-up, flat emission, per-epoch isolation, stake history, unstake maturity | 30 min |

`magi_full_devnet_test.go` is the one to run if you only run one. It proves a single
emission splitting three ways into three *different* distributor mechanisms at once,
with exact end-to-end balance conservation — and then attacks it from two directions.

**Three attacker profiles, because they probe different depths.** A pure outsider (no
stake, no share, no tokens) is the easy case: most calls die on the first authority
check. A *staked holder* is the realistic insider — real stake in C1, real shares in
C3/C5, claims already collected — so their attacks (double-claim, epoch aliasing,
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

**The attacker account must be funded, and the funding must be PROVEN.** RC is
`ledger HBD + 10,000 free` — about seven transactions. Past that, attacks fail with
*insufficient RC* instead of *not authorised*: a false pass, green while proving
nothing.

Three things are needed, and only doing the first is not enough:

1. Deposit in **repeated descending rounds**, so an account moves as much L1 balance
   as it has rather than stopping at the first amount that happens to succeed.
2. **Poll until the deposits credit the L2 ledger.** They are L1 transfers to the
   gateway and settle asynchronously — a 25s sleep produced `hbd=0` for every
   account. The suites poll for up to 4 minutes and fail if it never lands.
3. **Assert the resulting headroom.** The tests log
   `ledger balance hive:X hbd=N -> RC ~N+10000` for every actor and fail if the
   attacker is not funded well past the free tier. A recent run had the attacker at
   `hbd=100000` → RC ~110,000, against ~48,000 needed for the 34-attack sweep.

`N/N attacks reached the chain` is also logged, so a regression in any of this is
visible rather than silent.

Equally, the pre-attack snapshot is checked for emptiness before the sweep runs: a
baseline of empty strings compares equal to anything, so a mistyped state key would
turn every "nothing moved" assertion into a no-op.

The reporter tests serve dummy Hive data from a local `httptest` JSON-RPC server, so
they need no Hive access — but they do talk to the devnet's real GraphQL endpoint and
broadcast real transactions to the devnet chain.

## Verification status against the allowance model

C2 changed from minting each epoch to drawing from an approved pool. Every suite
had to be re-run, because a suite that never mints a pool now funds nothing and
fails several minutes later with a misleading message. Where each one stands:

| suite | run against the pool model | runtime |
|---|---|---|
| `magi_full_devnet_test.go` | yes | 1869s |
| `magi_multiepoch_devnet_test.go` | yes | 1903s |
| `magi_rogue_reporter_devnet_test.go` | yes | 1620s |
| `magi_tokenomics_devnet_test.go` | **no** — setup patched, not executed | — |
| `magi_c5c6c7_devnet_test.go` | **no** — setup patched, not executed | — |

Keep this table honest. "Patched and compiles" is not "verified"; the last two
rows are the ones to run before trusting the set.

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

- **`totalSupply` no longer tracks emission.** It is constant once the pool is
  minted. Measure the pool holder's falling balance, or C2's rising one, instead.
- **Keep the pool holder separate from any participant**, or its balance mixes
  undrawn pool with earned rewards. The full-system suite conflates them (the
  deployer holds the pool and also stakes), which is why its conservation check
  measures a claim *delta* rather than an absolute balance.

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
  and C7 credits `min(stakeAt(start), stakeAt(end))` — so stake arriving later is
  zero at both epoch-0 boundaries and that epoch's yield is unclaimable.
