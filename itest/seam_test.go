package itest_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"magi_token/reporter/hivesrc"
	"magi_token/reporter/sharecore"
	"magi_token/reporter/submit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SEAM TEST — the reporter's real output byte-for-byte into the real contracts.
//
// Until this existed, the two halves of the system were tested only in isolation:
// every contract test hand-wrote its shares string ("alice:75,bob:25") and every
// reporter test stopped at producing a page or dry-running a transaction. Nothing
// checked that what the reporter EMITS is what the contract ACCEPTS, so a mismatch
// in address prefixing, entry separators, page numbering or payload field names
// would have passed both suites and failed on the first real epoch.
//
// So: no payload below is written by hand. Realistic Hive data goes in at the top,
// the actual reporter pipeline runs, and `pl.Calls[i].Payload` is handed to the
// wasm engine verbatim.

const (
	seamToken = "vsc1BfqCB2b5ppiq4snQP74joWrJ3BMUN58pn9"
	seamC2    = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	seamC3    = "vsc1BmLNMQep1RaaUdYTPfEhqn1inESqNz4Ekt"
)

// realisticEpoch builds one day of a tribe's activity. Values are dummy but shaped
// like real Hive: rshares spanning six orders of magnitude, a whale, dust voters, a
// downvote, self-voting, an author who also curates, and votes that land after the
// post paid out.
func realisticEpoch() ([]hivesrc.RawPost, map[string][]hivesrc.RawVote) {
	mk := func(author, permlink, created string) hivesrc.RawPost {
		return hivesrc.RawPost{
			Author: author, Permlink: permlink, Created: created,
			Category: "hive-167922", Depth: 0,
			IsPaidout: true,
			// Hive payout is creation + 7 days
			PayoutAt: strings.Replace(created, "2026-03-02", "2026-03-09", 1),
		}
	}
	posts := []hivesrc.RawPost{
		mk("alice", "my-trading-journal-week-12", "2026-03-02T06:14:22"),
		mk("bob", "why-i-stake-everything", "2026-03-02T09:41:05"),
		mk("carol", "market-recap-and-charts", "2026-03-02T13:02:58"),
		mk("dave", "a-short-shitpost", "2026-03-02T18:55:11"),
		mk("erin", "deep-dive-tokenomics", "2026-03-02T21:30:00"),
	}
	v := func(voter, rshares string, pct int, t string) hivesrc.RawVote {
		return hivesrc.RawVote{Voter: voter, Rshares: rshares, Percent: pct, Time: t}
	}
	votes := map[string][]hivesrc.RawVote{
		// heavily curated, whale votes early
		"alice/my-trading-journal-week-12": {
			v("whale", "184320000000000", 10000, "2026-03-02T06:30:00"),
			v("bob", "9420000000000", 5000, "2026-03-02T07:15:44"),
			v("curator1", "812000000000", 10000, "2026-03-02T08:02:10"),
			v("dust1", "9400000", 1500, "2026-03-02T11:00:00"),
			v("dust2", "412000", 300, "2026-03-03T02:14:00"),
			v("latevoter", "56000000000", 10000, "2026-03-08T23:00:00"), // still inside payout
			// cast AFTER payout closed — must be ignored, or the report stops being
			// reproducible the moment another vote lands
			v("toolate", "999000000000000", 10000, "2026-03-11T04:00:00"),
		},
		"bob/why-i-stake-everything": {
			v("alice", "7300000000000", 10000, "2026-03-02T10:05:00"),
			v("bob", "2100000000000", 10000, "2026-03-02T09:42:00"), // self-vote
			v("curator1", "640000000000", 8000, "2026-03-02T12:20:00"),
			v("whale", "92160000000000", 5000, "2026-03-03T06:00:00"),
		},
		"carol/market-recap-and-charts": {
			v("curator1", "980000000000", 10000, "2026-03-02T13:30:00"),
			v("curator2", "410000000000", 10000, "2026-03-02T14:10:00"),
			v("dust1", "8800000", 1000, "2026-03-02T15:00:00"),
			// a downvote: negative rshares must clamp to zero, not subtract
			v("flagger", "-45000000000", -10000, "2026-03-02T16:00:00"),
		},
		// only a downvote and dust -> may end up with no positive weight at all
		"dave/a-short-shitpost": {
			v("flagger", "-12000000000", -10000, "2026-03-02T19:00:00"),
			v("dust2", "150000", 100, "2026-03-02T19:30:00"),
		},
		// muted author: everything here must vanish
		"erin/deep-dive-tokenomics": {
			v("whale", "46080000000000", 2500, "2026-03-02T22:00:00"),
			v("curator2", "300000000000", 10000, "2026-03-02T22:30:00"),
		},
	}
	return posts, votes
}

func TestSeam_ReporterOutputDrivesRealContracts(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(seamToken, owner, read(tokenWasmPath))
	ct.RegisterContract(seamC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(seamC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	// ---- 1. run the REAL reporter pipeline over realistic Hive data ----------

	rawPosts, rawVotes := realisticEpoch()
	opt := hivesrc.Options{
		Tag:             "hive-167922",
		Mode:            hivesrc.WeightHiveRshares,
		ExcludeAccounts: []string{"hive:erin"}, // muted author
	}
	excl := map[string]bool{"hive:erin": true}

	var posts []sharecore.Post
	for _, rp := range rawPosts {
		if excl["hive:"+rp.Author] {
			continue
		}
		p, err := hivesrc.MapPost(rp, rawVotes[rp.Author+"/"+rp.Permlink], opt, excl)
		require.NoError(t, err, "MapPost %s/%s", rp.Author, rp.Permlink)
		if len(p.Votes) > 0 {
			posts = append(posts, p)
		}
	}
	require.NotEmpty(t, posts, "fixture produced no scorable posts")

	cfg := sharecore.Config{
		AuthorRewardBps:  5000, // 50/50 author/curators
		AuthorCurveNum:   1,
		AuthorCurveDen:   1,
		CurationCurveNum: 1, // sqrt -> early curators earn more
		CurationCurveDen: 2,
		Muted:            []string{"hive:erin"},
	}
	res := sharecore.ComputeShares(posts, cfg)
	canon := sharecore.Canonicalize(res)
	require.Positive(t, res.Total.Sign(), "no shares computed")

	// Deliberately small pages: the epoch MUST split across several submitShares
	// calls, so page numbering and per-page accumulation are exercised rather than
	// assumed.
	pages := sharecore.Paginate(canon, 3, 3800)
	require.Greater(t, len(pages), 1, "want a multi-page report, got %d", len(pages))
	t.Logf("reporter: %d posts -> %d accounts, total shares %s, %d pages",
		len(posts), len(res.Shares), res.Total, len(pages))

	// the post-payout vote must have been dropped before it ever reached a page
	assert.NotContains(t, canon, "toolate",
		"a vote cast after payout leaked into the report — it would not be reproducible")
	assert.NotContains(t, canon, "erin", "muted account leaked into the report")
	assert.NotContains(t, canon, "flagger", "a downvoter earned shares")

	plan := submit.BuildFullPlan(submit.PlanOpts{
		Epoch:         "0",
		DistributorID: seamC3,
		FunderID:      seamC2,
		PullFunding:   true,
		Finalize:      true,
		Pages:         pages,
		RcLimit:       500000,
	})

	// ---- 2. stand up the contracts ------------------------------------------

	call(t, &ct, seamToken, "init",
		`{"name":"Tribe","symbol":"TRIBE","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, seamToken, seamC2, "500000000", 0)
	call(t, &ct, seamToken, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, seamC2), owner, 0, true)
	call(t, &ct, seamC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10",`+
			`"dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:10000"}`, seamToken, seamC3), owner, 0, true)
	call(t, &ct, seamC3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		seamToken, seamC2), owner, 0, true)

	// ---- 3. execute the reporter's plan VERBATIM ----------------------------

	// Nothing below constructs a payload. Every one comes from the reporter.
	caller := func(action string) string {
		if action == "submitShares" || action == "finalizeEpoch" {
			return "hive:reporter"
		}
		return "hive:keeper" // distributeEpoch / pullFunding are permissionless
	}
	for i, c := range plan.Calls {
		require.True(t, json.Valid([]byte(c.Payload)), "call %d payload is not json", i)
		t.Logf("plan[%d] %-16s %s %s", i, c.Action, c.ContractID, truncate(c.Payload, 110))
		call(t, &ct, c.ContractID, c.Action, c.Payload, caller(c.Action), 2, true)
	}

	// ---- 4. the contract must agree with the reporter -----------------------

	funded := stateBig(t, &ct, seamC3, "funded|0")
	onChainTotal := stateBig(t, &ct, seamC3, "totalShares|0")
	require.Equal(t, res.Total.String(), onChainTotal.String(),
		"contract's totalShares disagrees with the reporter's — the seam is broken")
	require.Positive(t, funded.Sign(), "epoch was never funded")
	assert.Equal(t, "finalized", stateStr(t, &ct, seamC3, "status|0"))
	t.Logf("on-chain: funded=%s totalShares=%s (reporter total %s)", funded, onChainTotal, res.Total)

	// every account the reporter named must be able to claim exactly
	// funded*share/totalShares, and the sum must never exceed what was funded.
	paid := new(big.Int)
	claimants := 0
	for acct, share := range res.Shares {
		want := new(big.Int).Div(new(big.Int).Mul(funded, share), onChainTotal)
		if want.Sign() == 0 {
			// contract aborts rather than emit a zero transfer; assert that instead
			call(t, &ct, seamC3, "claim", `{"epoch":"0"}`, acct, 4, false)
			continue
		}
		r := call(t, &ct, seamC3, "claim", `{"epoch":"0"}`, acct, 4, true)
		assert.Contains(t, r.Ret, `"claimed":"`+want.String()+`"`,
			"%s: payout disagrees with funded*share/total", acct)
		assert.Equal(t, want.String(), balanceOf(t, &ct, seamToken, acct),
			"%s: token balance does not match its claim", acct)
		paid.Add(paid, want)
		claimants++
	}
	require.Positive(t, claimants, "nobody could claim")
	assert.LessOrEqual(t, paid.Cmp(funded), 0,
		"INVARIANT VIOLATED: claims %s exceed funding %s", paid, funded)
	t.Logf("%d claimants paid %s of %s funded (dust %s stays in the contract)",
		claimants, paid, funded, new(big.Int).Sub(funded, paid))

	// a second claim must fail for everyone
	for acct := range res.Shares {
		call(t, &ct, seamC3, "claim", `{"epoch":"0"}`, acct, 5, false)
		break
	}
}

func stateStr(t *testing.T, ct *test_utils.ContractTest, id, key string) string {
	t.Helper()
	return strings.Trim(ct.StateGet(id, key), `"`)
}

func stateBig(t *testing.T, ct *test_utils.ContractTest, id, key string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(stateStr(t, ct, id, key), 10)
	if !ok {
		t.Fatalf("state %s|%s is not an integer: %q", id, key, stateStr(t, ct, id, key))
	}
	return v
}

func balanceOf(t *testing.T, ct *test_utils.ContractTest, token, acct string) string {
	t.Helper()
	r := call(t, ct, token, "balanceOf", fmt.Sprintf(`{"account":"%s"}`, acct), acct, 6, true)
	var out struct {
		Balance json.Number `json:"balance"`
	}
	if err := json.Unmarshal([]byte(r.Ret), &out); err != nil {
		t.Fatalf("balanceOf(%s) returned %q: %v", acct, r.Ret, err)
	}
	return out.Balance.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// The seam is only sound if the CONTRACT enforces what the reporter happens to
// emit. It did not: applyEntries used the permissive validateAddr, so a bare
// "alice" (no ledger domain) was accepted and counted into totalShares — and then
// `claim` looked up share|<ep>|hive:alice, found nothing, and the share was
// unclaimable forever. Measured before the fix: totalShares=200, hive:alice could
// not claim, and hive:bob received 50000 instead of 100000 — silently diluted 50%
// by a share nobody could ever redeem, with that slice of funding stranded.
//
// C3/C5 now require a ledger domain on share recipients (and C6 on airdrop
// recipients, where a bare address would credit an unspendable balance).
func TestSeam_BareAddressIsRejectedNotStranded(t *testing.T) {
	_ = os.RemoveAll("data/badger")
	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(seamToken, owner, read(tokenWasmPath))
	ct.RegisterContract(seamC2, owner, read("../c2-emission/artifacts/main.wasm"))
	ct.RegisterContract(seamC3, owner, read("../c3-distributor/artifacts/main.wasm"))

	call(t, &ct, seamToken, "init",
		`{"name":"T","symbol":"T","decimals":0,"maxSupply":"1000000000"}`, owner, 0, true)
	fundC2Pool(t, &ct, seamToken, seamC2, "500000000", 0)
	call(t, &ct, seamToken, "changeOwner",
		fmt.Sprintf(`{"newOwner":"contract:%s"}`, seamC2), owner, 0, true)
	call(t, &ct, seamC2, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","genesis":"0","epochLen":"1","baseAnnual":"1000000",`+
			`"blocksPerYear":"10",`+
			`"dustBucket":"author","timelock":"1","guardianMode":"0","guardianAuth":"hive:guardian",`+
			`"guardianThreshold":"1","vetoMode":"0","vetoAuth":"hive:veto","vetoThreshold":"1",`+
			`"buckets":"author:contract:%s:10000"}`, seamToken, seamC3), owner, 0, true)
	call(t, &ct, seamC3, "init", fmt.Sprintf(
		`{"token":"%s","kind":"0","funder":"%s","window":"1","reporterMode":"0",`+
			`"reporterAuth":"hive:reporter","reporterThreshold":"1","treasury":"hive:treasury",`+
			`"guardianMode":"0","guardianAuth":"hive:guardian","guardianThreshold":"1"}`,
		seamToken, seamC2), owner, 0, true)

	call(t, &ct, seamC2, "distributeEpoch", `{}`, "hive:keeper", 2, true)
	call(t, &ct, seamC3, "pullFunding", `{"epoch":"0"}`, "hive:keeper", 2, true)

	// The domain-less entry is now SKIPPED rather than counted — consistent with how
	// every other malformed entry is handled, and enough to close the stranding bug
	// because nothing uncountable ever enters totalShares.
	call(t, &ct, seamC3, "submitShares",
		`{"epoch":"0","page":"0","entries":"alice:100,hive:bob:100"}`, "hive:reporter", 2, true)
	assert.Equal(t, "100", stateStr(t, &ct, seamC3, "totalShares|0"),
		"a domain-less account must not be counted into totalShares")
	call(t, &ct, seamC3, "finalizeEpoch", `{"epoch":"0"}`, "hive:reporter", 2, true)

	// bob gets the WHOLE pot: no share is stranded on an unclaimable address.
	// (Before the fix he received 50000 and the other half was lost forever.)
	r := call(t, &ct, seamC3, "claim", `{"epoch":"0"}`, "hive:bob", 4, true)
	assert.Contains(t, r.Ret, `"claimed":"100000"`,
		"funding must not be stranded on an address that cannot claim")
	call(t, &ct, seamC3, "claim", `{"epoch":"0"}`, "hive:alice", 4, false)
}
