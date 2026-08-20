# Hive-Engine reward-pool settings, and how this framework supports each

Every field from the SCOT admin panel, the value BBHO had it set to, and exactly what
carries it here. Where our answer differs from SCOT's, the difference is stated rather
than smoothed over.

Three settings live on-chain because they cannot work anywhere else; the rest are
reporter config. That split is not arbitrary — anything that decides *how tokens move*
has to be enforced where the tokens are.

---

## Token — `BBHO`

**How we support that.** The value asset is the C0 token contract, named once in every
component's `init` as `token`, and the contracts cross-check each other: the
distributor's init refuses a staking contract holding a different token, so a
misconfigured deployment fails at setup rather than paying out in the wrong asset. One
deployed token per pool; we do not multiplex.

---

## Post Reward Curve — `Power (r^a)` · Parameter — `1`

**How we support that.** `shares.author_curve`, written as an exact rational — `"1/1"`
for BBHO's linear setting, `"3/2"` for r^1.5. It is a rational rather than a decimal on
purpose: the exponent is applied with integer arithmetic (`PowRational`, Newton's method
on `big.Int` with an exact floor correction) and never touches float64. A float would
round differently depending on expression order and platform, and two reporters that
disagree by one unit can never reach an Attest threshold. SCOT's "maximum 2 decimal
precision" limit does not apply to us — any rational works.

---

## Curation Reward Curve — `Power (r^a)` · Parameter — `1`

**How we support that.** `shares.curation_curve`, same exact-rational form. The
mechanism is the marginal slice `C(cum_after) − C(cum_before)`, so the direction is the
part worth stating: **concave (`1/2` = sqrt) pays EARLY voters more**, `1/1` is
order-neutral, and convex (`2/1`) pays late voters more. BBHO's `1` is order-neutral.
SCOT constrains this to 0.5–1; we do not, because convex is a coherent (if unusual)
choice and refusing it would be arbitrary.

---

## Curation Reward Percentage — `50%`

**How we support that.** `shares.author_reward_bps`, expressed from the author's side in
basis points — BBHO's 50% curation is `5000`. Basis points rather than whole percent
because the whole pipeline is integer, and the author cut is computed as
`postWeight × bps / 10000` with the remainder going to curators, so nothing is lost to
rounding twice.

---

## Staked Reward Percentage — `50%`

**How we support that.** `stakedBps` on the **distributor's `init`**, paired with
`stakeContract`. This is the one setting that could not live in the reporter: the
reporter emits a list of share weights and never touches tokens, so the distributor is
the only place a payout exists to be divided.

At claim the payout splits, and both halves are reported — in the return value and in
the emitted event — because a single total would hide which the recipient actually got.
The staked half is credited by approving the staking contract and calling `stakeFor`,
which **pulls** exactly the amount it was told about; a plain transfer would move the
balance and leave the staking contract with no idea whose it was. The allowance granted
is exactly that payout and never standing.

The staked half is *real* stake, subject to the same cooldown as anything a holder
staked themselves — otherwise it would be liquid tokens wearing a different name.

**Cost you should know about:** a staked claim is **2,967 RC against 828 liquid**. A
claim is the one call an ordinary holder makes, and an earner may hold no HBD at all, so
this is measured and regression-tested rather than assumed.

---

## Cashout Window Days — `7`

**How we support that.** `source.cashout_days`; `0` selects Hive's own seven. It sets
both the window the feed is walked over *and* the vote cutoff, so it decides which votes
count, not merely when they are counted.

**Where we differ from SCOT, deliberately:** a post is ALWAYS scored in the epoch its
payout falls in, once voting has closed. SCOT offers a "created" attribution mode; we
removed it. Scoring a post while votes are still arriving means a post created in the
last minute of an epoch is scored with almost none of its votes, and every vote cast
after the snapshot counts for nobody. There is no setting for it.

---

## Vote Regeneration Days — `5` · Vote Power Consumption — `200`
## Downvote Power Regeneration Days — `30` · Downvote Power Consumption — `200`

**How we support that.** `source.vote_regen_days`, `source.vote_power_consumption`,
`source.downvote_regen_days`, `source.downvote_power_consumption` — same units as SCOT,
consumption in hundredths of a percent, so `200` is 2% per full vote.

They apply to **`weight: token_stake` only, where they are REQUIRED**, and are
**refused** for `weight: hive_rshares`. That asymmetry is the whole point:

- `hive_rshares` inherits Hive's mana already — the rshares a node reports have been
  shrunk by how much the voter has been voting. A second budget would charge the same
  vote twice, so the config rejects it rather than silently ignoring it.
- `token_stake` reads a staked balance, and **a balance is not consumed by being used**.
  Without a budget one account votes every post in an epoch at full weight and the
  curation curve stops meaning anything. So these are mandatory there, not optional.

Upvotes and downvotes draw on separate pools, which is why there are four numbers.

**A real limitation, stated plainly:** SCOT keeps mana in persistent state, carried
across epochs forever. Our replay runs within each epoch's own vote set with every voter
starting at full power. It prices voting correctly *within* an epoch but not *across*
them. Continuous mana would need either reporter-side state that two Attest machines
could disagree about, or a contract write per vote — both worse trades until epochs are
short enough for carryover to matter.

---

## Reward Interval Seconds — `120` · Reward Per Interval — `2`

**How we support that.** Differently shaped, same expressive power: the emission
contract's `init` takes `baseAnnual`, `blocksPerYear` and `epochLen`, and pays
`baseAnnual × epochLen / blocksPerYear` per epoch. BBHO's 2-per-120-seconds is 1,440/day
(~525,600/year), which is what `baseAnnual` expresses directly.

We work in **blocks, not seconds** — VSC's clock is `block.height`, and a timestamp is a
parse-needy string. SCOT's "divisible by 3" rule is Hive's 3-second block time showing
through; here the block *is* the unit, so the constraint disappears.

One operational difference: emission is **poked**, not automatic. A keeper calls
`distributeEpoch`, which is idempotent and catches up missed epochs in bounded chunks
(`maxCatch`). Falling behind costs RC in one transaction rather than losing emission.

---

## Tags — `bbh`, `inleo`, `drip`, `tip`, `passive`

**How we support that.** `source.tags`, capped at 5 like SCOT. Each tag is a separate
feed walk, and **a post reachable under several is collected once** — paying per
matching tag would multiply a payout by however many of the pool's own tags an author
happened to list.

Ordering is canonicalised *before* the post limit truncates, so two Attest machines with
the same five tags listed in a different order still produce byte-identical pages.

---

## Excluded Tags — *(empty)*

**How we support that.** `source.excluded_tags`, also capped at 5, applied **after** the
tag walk: a post reached through an indexed tag is still dropped if it carries an
excluded one. The reverse order would make the setting inert, since everything the pool
sees arrives via an included tag. A tag appearing on both lists is refused at config
load rather than silently reading an empty feed.

Matched against the post's category (which carries the community) and its
`json_metadata` tags.

> Not to be confused with `source.exclude`, which is a list of **accounts** that earn
> nothing. Similar name, different filter.

---

## Setup App Tax — `OFF`

**How we support that.** `shares.app_tax` — `bps`, `apps` (matched on the part before
the `/`, so `peakd/2023.1` matches `peakd`), and `beneficiary`. The skim comes off the
top before the author/curator split, so both bear it in proportion.

Two things the implementation insists on. The skim is **conserved, not burned** — it is
paid to the beneficiary as its own share entry, because a burned slice silently shrinks
the epoch and redistributes it to everyone else, which looks like the tax working while
paying the beneficiary nothing. And a rate with no `apps` or no `beneficiary` is refused
at config load: with no designated app *every* post is taxed, including those from your
own front-end.

**Worth knowing before enabling it:** the `app` is self-declared in `json_metadata`.
Anyone posting via the API can put any string there. It shapes ordinary users on
ordinary clients and does nothing to anyone motivated to route around it.

---

## Disable Downvotes — `ON`

**How we support that.** `source.disable_downvotes`. Set, negative votes are dropped
entirely and cannot affect a payout — BBHO's configuration. Unset, they net off the
post's total before the curve.

Two rules hold either way. A post voted below zero earns **nothing**, never a negative
share: the contract has no way to take shares back, so zero is the floor. And a
downvoter **never enters the curation split** — a downvoter is not a curator, and paying
them from the curation pool would make attacking posts a way to earn from them.

---

## Ignore Declined Payout — `OFF`

**How we support that.** `source.ignore_declined_payout`. Left off, as BBHO has it, an
author who declined their Hive payout is honoured here too. Hive has no boolean for
this — the author sets `max_accepted_payout` to `0.000 HBD`, so the amount *is* the flag,
and an absent or unparseable field counts as *not* declined (defaulting the other way
would silently stop paying everyone).

The **whole post** is skipped, not just the author's cut. Paying curators to farm a post
whose author wanted no payout is the loophole that makes the setting pointless.

---

## Setup Reward Reduction — `OFF`

**How we support that.** Emission is flat by design — there is no decay schedule to
switch off, so BBHO's setting is our only behaviour. Halving was removed from the
emission contract deliberately; the exact removed code, config schema and a restore
checklist are preserved in [halving-schedule.md](halving-schedule.md) if a pool ever
wants it back.

The only terminator is the token's `maxSupply`: the final epoch mints the remainder and
latches, so emission stops by running out rather than by decaying toward zero.

---

## Settings SCOT has that we deliberately do not

- **Attribution mode (`created` vs `cashout`)** — removed. See Cashout Window above.
- **NFT-backed value assets** — the adapter refuses `kind=1`. The prerequisites are not
  implemented, and NFTs are not supported by the DEX yet, so a deployment must be
  impossible rather than broken later.

## How the share book is stored — a difference worth knowing

SCOT keeps every account's share in its own state. This framework commits a **merkle
root** on chain and logs the leaves for the indexer, because per-account state writes
were ~92% of an epoch's cost (a full page: 15,309 RC → 1,871).

The consequence for anyone integrating: the chain cannot answer "what did this account
earn?" — the indexer does, and the root proves its answer. A claimant passes their
share and a proof, obtainable from `reporter proof -account X`, which recomputes the
epoch from public Hive data and therefore needs no indexer at all.

## Settings we have that SCOT does not

- **Attest (M-of-N) reporting** — several machines independently compute the same epoch
  and a page commits only at threshold. Determinism is what makes it possible: identical
  input yields byte-identical output, so no coordination is needed.
- **Guardian, veto and challenge window** — a fraudulent report can be cancelled before
  it pays, and the funding recovered to a treasury pinned at init.
- **Trustless staking yield** — reads the stake history directly and pays pro-rata, so
  there is no report to falsify and nothing to challenge.
- **A dust threshold** (`shares.min_share_bps`) — drops earners below a share of the
  epoch. This is our design, not SCOT parity; it is the one lever that scales an
  epoch's cost down, since every earner costs the same to publish whatever they are
  owed.
