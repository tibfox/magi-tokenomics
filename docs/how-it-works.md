# How this system works

Plain-language guide. No code. If you want the exact `init` parameters, see the
[README](../README.md).

## The one-sentence version

You have a token. Every day the system creates some new tokens, splits them between
groups you choose (writers, liquidity providers, stakers, a treasury), and then each
person comes and collects their share.

## Why it exists

On Hive-Engine, "tribes" ran on SCOT and outposts: you post with a tag, people vote,
and a token gets handed out. That was closed software you rented.

This is the same idea rebuilt as contracts you deploy and own. Nothing is
hardcoded — every number is a setting you choose when you set it up. Two projects
running this can have completely different economics.

## The moving parts

Think of it as a water system.

**The tap — emission (C2).**
On a fixed schedule it releases tokens into buckets. It doesn't create them: you
mint the whole pool up front, park it in an account, and let C2 spend from it — the
same "approve a spender" pattern every token uses. C2 draws that epoch's amount and
pours it into buckets.

That means C2 has **no power over your token at all** — it can't mint, pause, or
change the owner. The flip side: whoever holds the pool can revoke the approval and
stop emission, so the schedule is a promise backed by whoever holds it, not by code.

**The buckets — you define them.**
You decide the buckets and how the flow splits, e.g. 50% content / 30% liquidity /
20% stakers. A bucket can point at one of the payout contracts below, at a DAO, or
at a plain treasury wallet. It's your call.

**The payout contracts — who actually gets paid.**

- **Content rewards (C3)** — for posting and voting on Hive.
- **LP rewards (C5)** — for providing liquidity. Same machinery, separate instance.
- **Staking yield (C7)** — for locking tokens up.

**Staking (C1).**
People lock tokens here. It records a history, so the system can prove later
"this person had 600 tokens staked on day 12" — which is how yield is calculated
fairly after the fact.

**Airdrop (C6).**
A one-off tool for launch: import a snapshot of existing holders and pay them out.
Used once, then it just sits there.

**The reporter — a program you run, not a contract.**
Contracts cannot read Hive. So a small service reads Hive posts and votes, works out
who earned what, and sends that list to C3/C5. It's covered in its
[own README](../reporter/README.md).

## How money actually moves

Nothing is ever pushed to anyone. **Everyone collects their own tokens.**

That sounds like a detail, but it's the most important design decision here. If the
system tried to pay 500 people in one transaction, a single bad address could make
the whole thing fail — and then nobody gets paid, forever. Instead, the system
records what you're owed, and you claim it when you like.

Each round (an "epoch", usually a day):

1. Someone pokes C2 — anyone can, it's not privileged.
2. C2 creates that day's tokens and records how much each bucket is owed.
3. Each payout contract pulls its share.
4. For content and LP, the reporter submits the list of who earned what.
5. A **challenge window** opens — a guardian can cancel a bad report during it.
6. After the window, people claim.

Staking yield skips steps 4 and 5 entirely: it reads the staking history itself, so
there's nothing to report and nothing to challenge.

## Who can do what

The system assumes the person who deploys it is trustworthy. The protection is
against everyone else — outsiders, and token holders who aren't the owner.

| role | can do | cannot do |
|---|---|---|
| anyone | poke the schedule, pull funding, claim their own share | anything else |
| reporter | submit share lists, close an epoch | mint, move funds, pay itself |
| guardian | cancel a bad report during the challenge window; sweep genuinely unclaimable leftovers to a **fixed** treasury | redirect the treasury, take funds |
| owner | queue token operations (pause, change owner) on a **time delay** | act instantly, or bypass the veto |

Two rules are enforced by the contracts, not by convention:

- **The reporter and the guardian must be different parties.** One party who could
  both publish a fraudulent report *and* refuse to cancel it would make the
  challenge window meaningless.
- **The treasury address is fixed when you set up.** A sweep can only ever go
  there, so it can't be redirected later.

Staked funds sit in the staking contract and **no role can touch them** — not the
owner, not the guardian.

## What you configure

### The money

| setting | what it decides | example |
|---|---|---|
| emission rate | how many tokens per year | 1,000,000 |
| epoch length | how often payouts happen | 1 day |
| buckets | who gets what share | 50% content, 30% LP, 20% stakers |
| max supply | the hard ceiling | 21,000,000 |
| the pool | how much C2 is approved to distribute | 10,000,000 |

Emission is **flat** — the same amount every epoch, forever. It pauses when the pool
runs dry (or the approval is withdrawn), and resumes if you top the pool up.

**You can mint the pool in batches.** Rather than creating the whole supply on day
one, you can mint, say, 25% at a time and add the next slice when it runs low. Less
sits pre-minted, and the schedule keeps running across the boundary. Two rules:

- Use **`increaseAllowance`** to top up, not `approve` — `approve` replaces the
  figure and would throw away whatever the last batch had left over.
- **Don't hand token ownership to the emission contract** if you plan to mint again.
  Only the owner can mint, and the emission contract has no way to do it. It doesn't
  need to own the token anyway.

If the pool sits empty for a while, the missed days aren't lost — once refilled, the
system pays them out one at a time until it has caught up. A long gap therefore means
a large catch-up, so top up with that in mind.

One small thing: a leftover too small to cover a whole day is never paid out as a
part-day. It waits in the pool for the next top-up.

One thing to know: because the whole pool is minted up front, **total supply doesn't
grow as rewards go out**. Explorers will show the full supply from day one, with most
of it sitting in the pool account. To see how much has actually been distributed,
watch that account's balance fall. There's no built-in halving; if you want a decaying schedule,
the design is preserved in [`halving-schedule.md`](halving-schedule.md) and can be
put back.

### The content rewards

| setting | what it decides |
|---|---|
| tag | which Hive community/tag counts |
| author share | how the pot splits between the writer and the voters (e.g. 50/50) |
| curation curve | whether **early** or **late** voters earn more |
| muted accounts | who earns nothing |
| voting power | comes from **HIVE stake**, or from **your own token's** stake |

**The curation curve is worth a moment.** It's the setting people get wrong.

- Reward early voters (the classic Hive behaviour) → people hunt for good posts
  before anyone else. Rewards discovery.
- Reward late voters → people wait to see what's already popular and pile on.

Same knob, opposite cultures. Pick deliberately.

### When a post is counted

Hive keeps voting open on a post for 7 days. So there's a choice:

- **Wait for voting to finish** (the default). Every vote counts exactly once, and
  it's fair to everyone. The cost is that rewards arrive about a week after posting —
  the same lag Hive itself has.
- **Pay immediately.** Faster, but a post made near the end of an epoch is scored
  before most of its votes arrive, so late posters are systematically underpaid and
  the votes cast afterwards are counted by nobody, ever.

The default is the fair one.

### Staking

| setting | what it decides |
|---|---|
| cooldown | how long an unstake waits before withdrawal |
| epoch length | must be shorter than the cooldown |

Only stake held for a **whole** epoch earns yield. Otherwise someone could stake for
one block at the snapshot moment and skim rewards they never took risk for.

### Who holds the keys

Any privileged role can be one of:

- **one account** — simplest;
- **several accounts signing together** in one transaction;
- **several machines agreeing independently** — each submits the same thing
  separately, and it takes effect once enough of them match.

That last mode is how you run the reporter on several machines at once. Because the
calculation is exactly reproducible, they all produce identical output without
talking to each other — so no single machine, or single key, is trusted.

## What you can leave out

You don't need all of it.

- **Just an emission schedule into a treasury?** Token + C2. Two contracts.
- **Add trustless staking rewards?** + C1 + C7. Still no reporter, still nothing to
  trust.
- **Add content rewards?** + C3 and the reporter. This is the first piece that
  needs an honest operator — which is exactly why the challenge window and the
  multi-machine mode exist.
- **Add LP rewards?** + C5.
- **Migrating from Hive-Engine?** + C6 for the initial snapshot, used once.

## The honest limitations

- **The reporter is trusted-ish.** It decides who earned what for content and LP. It
  can't mint or steal — it only submits a list of numbers — but it could submit a
  *wrong* list. The defences are that anyone can recompute it exactly, a guardian can
  cancel it during the challenge window, and you can require several machines to
  agree. Staking yield has none of this exposure because it reads the chain directly.

  This is not theoretical: a devnet test has the reporter publish 100% of an epoch's
  rewards to itself, and shows the fraud being accepted, then vetoed, the funding
  recovered to the treasury, and the rogue paid nothing. See the README's security
  section for what that suite establishes.
- **Something has to poke the schedule.** Contracts can't wake themselves up. It's
  permissionless, so anyone can do it, but if nobody does, nothing happens — and the
  longer it's left, the more expensive the catch-up poke becomes. See
  [`rc-costs.md`](rc-costs.md).
- **Rounding always favours the pot.** Division truncates, so tiny remainders stay
  in the contract rather than being over-paid. Splitting your holdings across many
  accounts loses you money rather than gaining it.
- **NFT-backed tokens are not supported.** The setting exists but is deliberately
  blocked rather than half-working.
