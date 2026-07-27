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

## The four suites

| test | covers | ~time |
|---|---|---|
| `magi_tokenomics_devnet_test.go` | C0 + C2 + C3, then 13 outsider attacks | 10 min |
| `magi_c5c6c7_devnet_test.go` | C1 + C2 + C5 + C6 + C7, then outsider attacks | 18 min |
| `magi_reporter_devnet_test.go` | the real `reporter` binary driving C3 against injected Hive data | 12 min |
| `magi_full_devnet_test.go` | **all 7 contracts + the reporter in one run** | 23 min |

`magi_full_devnet_test.go` is the one to run if you only run one. It proves a single
emission splitting three ways into three *different* distributor mechanisms at once,
with exact end-to-end balance conservation.

The reporter tests serve dummy Hive data from a local `httptest` JSON-RPC server, so
they need no Hive access — but they do talk to the devnet's real GraphQL endpoint and
broadcast real transactions to the devnet chain.

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
- **Never assert on an aggregate straight after broadcasting.** `total_staked`,
  `owed|`, `funded|`, `totalShares|` and the `claimed|` markers all appear as soon as
  the *first* contributing transaction lands and then grow — waiting only for a key
  to exist samples a partial value. Use the `waitValue` / `waitStateKeyPresent`
  helpers, never a bare read.
- **Stake before initialising C2.** C2's `genesis` is the block it initialises at,
  and C7 credits `min(stakeAt(start), stakeAt(end))` — so stake arriving later is
  zero at both epoch-0 boundaries and that epoch's yield is unclaimable.
