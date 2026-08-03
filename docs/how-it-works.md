# How this system works

Plain-language guide. No code. If you want the exact `init` parameters, see the
[README](../README.md).

## The one-sentence version

You have a token. Every day the system releases some of it from a pool, splits that between
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
- **LP rewards (C5)** — for providing liquidity. Same machinery, separate instance;
  you run a second copy of the reporter in "lp" mode to feed it.
- **Staking yield (C7)** — for locking tokens up.

**Staking (C1).**
People lock tokens here. It records a history, so the system can prove later
"this person had 600 tokens staked on day 12" — which is how yield is calculated
fairly after the fact.

**Airdrop (C6).**
A one-off tool for launch: import a snapshot of existing holders and pay them out.
Used once, then it just sits there.

**The reporter — a program you run, not a contract.**
Contracts cannot read the outside world. So a small service works out who earned what
and sends that list in. It runs in one of two modes: reading **Hive posts and votes**
to feed content rewards, or reading **liquidity history** to feed LP rewards. You run
one copy per reward type. It's covered in its [own README](../reporter/README.md).

For liquidity there's a wrinkle worth knowing: the exchange only records who holds
liquidity *right now*, and a reward day is worked out after it has ended. So the
service replays the deposit and withdrawal history to reconstruct who held what, and
pays on **the smaller of your start-of-day and end-of-day position**. Otherwise you
could add liquidity for a minute at the right moment and collect a whole day.

## How money actually moves

Nothing is ever pushed to anyone. **Everyone collects their own tokens.**

That sounds like a detail, but it's the most important design decision here. If the
system tried to pay 500 people in one transaction, a single bad address could make
the whole thing fail — and then nobody gets paid, forever. Instead, the system
records what you're owed, and you claim it when you like.

Each round (an "epoch", usually a day):

1. Someone pokes C2 — anyone can, it's not privileged.
2. C2 draws that day's tokens from the pool and records how much each bucket is owed.
3. Each payout contract pulls its share.
4. For content and LP, a reporter submits the list of who earned what.
5. A **challenge window** opens — a guardian can cancel a bad report during it.
6. After the window, people claim.

Staking yield skips steps 4 and 5 entirely: it reads the staking history itself, so
there's nothing to report and nothing to challenge.

### Nothing you earn ever expires

**Claim whenever you like — content, LP and staking yield alike.** Come back after a
month, or a year. Your share waits for you.

This was not always true. Staking yield used to have a roughly ten-day deadline, and
the story of why is worth a paragraph, because it shows how one small inaccuracy
turns into a rule that punishes users.

Staking yield pays you on the *smaller* of your stake at the start and end of the day,
so that only people who held throughout get paid. Fine. But to work out your share the
contract has to divide by the total everyone earned on — and it couldn't count that.
Adding up each person's smaller-of-two-numbers means going through every staker one by
one, which a contract cannot do. So it divided by the total staked instead, which is a
slightly bigger number whenever some people added and others removed on the same day.

Dividing by too big a number means paying out less than the full pot. The remainder
belonged to nobody. To stop it piling up forever, a guardian was allowed to sweep it —
but a sweep can't tell leftover money from money you simply haven't collected yet. So
claims had to be closed first. **That deadline existed only to make the sweep safe.**

It's fixed now, at the source. The staking contract keeps one running number per day:
how far people have dropped below where they started. Subtract it from the day's
opening total and you have the exact figure, with no counting through stakers. The
division is now correct, the pot is paid out in full, there's nothing left to sweep,
and so there's nothing to close claims for.

The only money still left behind is rounding — fractions of a token too small to
divide. That's the same in all three contracts.

One narrow exception exists: if a day passes with *nobody staked at all*, that day's
yield can never be claimed by anyone, and the guardian can recover it to the treasury.
That needs no deadline, because whether anyone was staked is settled history and no
amount of waiting could change it.

## Who can do what

The system assumes the person who deploys it is trustworthy. The protection is
against everyone else — outsiders, and token holders who aren't the owner.

| role | can do | cannot do |
|---|---|---|
| anyone | poke the schedule, pull funding, claim their own share | anything else |
| reporter | submit share lists, close an epoch | mint, move funds, pay itself |
| guardian | cancel a bad report during the challenge window; sweep a day that **nobody could ever claim** to a **fixed** treasury | redirect the treasury, take funds, touch a day that had any staker |
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

### The LP rewards

| setting | what it decides |
|---|---|
| indexer | which indexer to read liquidity history from — you can point at your own |
| pool | which liquidity pool counts |

There is no curve or split to choose here: an LP day is scored purely on how much
liquidity you held, and you are paid on the smaller of your two day-boundary
positions.

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

- **The reporter is trusted-ish.** It decides who earned what for content. It
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
  accounts loses you money rather than gaining it. A share small enough that its
  payout truncates to zero cannot be claimed at all — the dust stays behind.
- **A day with no stakers at all is recoverable by the guardian.** Its yield can
  never reach anyone, so it goes to the treasury rather than sitting locked. This is
  the only case where anyone can take reward money, and it cannot touch a day that had
  even one staker.
- **NFT-backed tokens are not supported.** The setting exists but is deliberately
  blocked rather than half-working.
- **LP rewards depend on an indexer.** Content rewards can be checked by anyone
  against Hive. LP rewards are reconstructed from an indexer's event log, because the
  exchange keeps no history of its own and putting that history on-chain would cost
  gas nobody wants to pay. The events come from public transactions and the indexer is
  publicly queryable, so the numbers can still be checked independently and a guardian
  can still cancel a bad report — but it is one more operator you are relying on.
