# The first real deployment

> **TL;DR** — Three deployments on vsc-testnet, all owner `hive:tester0`, covering
> single / cosigned / attest modes. Everything before this was devnet-only. Read this
> before your own first deployment, because **the traps are all immutable-at-init
> choices whose consequence only appears several steps later**:
>
> 1. **Deploy all four contracts before initialising any of them.** C1's `allow` list
>    is written once at init and is the `stakeFor` allowlist — if C3's id is not in it,
>    every staked payout aborts at payment time.
> 2. **Pick the guardian last.** C3's guardian is fixed at init, and `addChannel`
>    requires reporter and guardian to be *disjoint*; guardian = your only spare key
>    makes a cosigned channel impossible on that C3.
> 3. **An attest channel must carry a policy digest** or `addChannel` aborts with no
>    useful signal.
>
> Two things that look like contract faults and are not: **RC exhaustion** (a spent
> account's calls return `ok=false`, indistinguishable from a rejection — check
> `getAccountRC` first), and a **failed deploy costs nothing**, so just retry it.

vsc-testnet, 2026-08-21, from the build with no C2 token authority (11 of 11 runnable
devnet suites green). Everything before this was devnet-only. Written down because the
things that went wrong were all **immutable-at-init choices whose consequence appears
several steps later** — the kind a devnet run cannot show you, because devnet builds its
own setup instead of following the deployment sequence.

| | contract id |
|---|---|
| C0 token | `vsc1BWhTWFYqY2hNGwAtTCpnPpQhS9XCbig88V` |
| C1 staking | `vsc1BqaVaHs9T4NkRcxmHLbEwbT1rMKk84R1Py` |
| C2 emission | `vsc1BhAQsus26XzQPP3AyskbvGWKQV6SCysDH2` |
| C3 distributor | `vsc1BU6uUMdAztS7hNZtvVSBcJp7kF5ZojnA1S` |

Owner `hive:tester0`, 10 TBD per deploy (40 total). Shakedown economics: `epochLen` 50,
`genesis` 5941860, `baseAnnual` 1000000, `blocksPerYear` 1000 → 50,000 per epoch, split
50/50 into `content` (C3) and `yield` (C1). L1→L2 settles in about 18 seconds.

## What the chain proved

Token init and mint 1,000,000. Airdrop batch of 600 + 400 against a 1,000 float, ending
`unobligated: 0`. Staking of 1,000 across two accounts **holding genuinely different
keys**. Epoch 0 emission of 50,000 split 25,000 / 25,000. `C1.pullFunding(0)` = 25,000.
Then claims, exactly pro-rata:

    tester0         600/1000  ->  +15,000
    magi.contracts  400/1000  ->  +10,000
                                  ------- = 25,000 = funded

## The cosigned key layer, and why it looked broken

This is the gap devnet cannot close: its accounts share an active authority, so ONE
signature satisfies a two-account `required_auths` and mode 1 is never really tested.

A two-key transaction failed with `Missing Active Authority magi.contracts` **while
carrying a valid signature that recovers to exactly that account's on-chain active
key**. Both keys worked alone. The cause:

> `required_auths` is a `flat_set` in Hive, so the node re-serialises it **sorted**.
> Sign over an unsorted list and the node computes a different digest than the one you
> signed, so the authority reads as unsigned.

Sort the accounts — keeping each key paired with its account through the sort — and it
works in either input order. `cosign-call.js` in the testnet deployment scripts does
this. Nothing about the error message points at ordering, and the signature really is
valid, which is what makes it cost an afternoon.

## Two traps that are now constraints 6 and 7

**`allow` is immutable and gates every staked payout.** It is written only in `C1.Init`.
`stakedBps` makes each claim route part of its value through `C1.stakeFor`, which aborts
for any caller outside `allow`. An empty `allow` therefore does not disable staked
payouts — it makes every claim on that channel abort **at the point of payment**. C3 has
to be deployed before C1 is initialised so its id can go in the list.

**C3's guardian is immutable and must be disjoint from every reporter.** With two
accounts and one of them made guardian, a 2-of-2 cosigned channel on that C3 is
impossible — and `addChannel` only tells you at the point of adding the channel, long
after `C3.init` fixed the guardian.

## Reading state: do not trust `getStateByKeys`

Amounts are stored as raw big-endian bytes. Any byte that is not valid UTF-8 comes back
over GraphQL as U+FFFD, so a balance of **1000 reads as 66043837**. It is silent and it
looks like a number. Read through the contract's own query entrypoints instead —
`simulateContractCalls` against `balanceOf` / `stakeOf` / `fundedOf` / `owedOf` returns
real JSON, and it is what every figure above was verified with.

## Still open here

The `content` channel is not registered — it needs a reporter and a merkle root, and
until then that half of each epoch simply accrues as `owed` on C2, which is harmless.
Cosigned-on-C3 is blocked by constraint 7 on this particular deployment: the only two
keys available are `tester0` and `magi.contracts`, and the latter is the guardian.
Closing it needs a third funded account, or a fresh C3 whose guardian is someone else.
