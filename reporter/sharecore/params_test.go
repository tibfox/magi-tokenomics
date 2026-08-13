package sharecore

import (
	"math/big"
	"testing"
)

func v(voter string, w int64, order int) Vote {
	return Vote{Voter: voter, Weight: big.NewInt(w), Order: order}
}

func sum(r Result) *big.Int {
	t := new(big.Int)
	for _, s := range r.Shares {
		t.Add(t, s)
	}
	return t
}

var linear = Config{AuthorRewardBps: 5000, AuthorCurveNum: 1, AuthorCurveDen: 1,
	CurationCurveNum: 1, CurationCurveDen: 1}

// A downvote reduces what a post earns WITHOUT paying its caster. Both halves
// matter: if it did not reduce, the setting is decorative; if it did pay, downvoting
// would become a way to earn from posts you attack.
func TestCompute_DownweightReducesThePostButPaysNobody(t *testing.T) {
	base := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p",
		Votes: []Vote{v("hive:fan", 1000, 0)},
	}}, linear)

	with := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p",
		Votes:      []Vote{v("hive:fan", 1000, 0)},
		Downweight: big.NewInt(400),
	}}, linear)

	if sum(with).Cmp(sum(base)) >= 0 {
		t.Fatalf("a downvote must reduce the post's total: base %v, with %v", sum(base), sum(with))
	}
	if _, paid := with.Shares["hive:hater"]; paid {
		t.Fatal("a downvoter must never appear in the share book")
	}
	// 1000 - 400 = 600 split 50/50
	if got := with.Shares["hive:alice"]; got.Cmp(big.NewInt(300)) != 0 {
		t.Fatalf("author should earn on the NET 600, got %v", got)
	}
}

// A post voted below zero earns nothing at all, and must not produce a negative
// share — the contract has no way to take shares away, so the floor is zero.
func TestCompute_DownweightBeyondTheTotalEarnsNothing(t *testing.T) {
	r := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p",
		Votes:      []Vote{v("hive:fan", 100, 0)},
		Downweight: big.NewInt(500),
	}}, linear)
	if len(r.Shares) != 0 {
		t.Fatalf("a post voted into the ground earns nothing, got %v", r.Shares)
	}
}

// The app tax must be CONSERVED, not burned. A skim that vanished would silently
// shrink the epoch's total shares, quietly redistributing it to everyone else —
// which looks like the tax working while paying the beneficiary nothing.
func TestCompute_AppTaxIsConservedAndComesOffTheTop(t *testing.T) {
	cfg := linear
	cfg.AppTaxBeneficiary = "hive:treasury"

	untaxed := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p", Votes: []Vote{v("hive:fan", 1000, 0)},
	}}, cfg)
	taxed := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p", Votes: []Vote{v("hive:fan", 1000, 0)},
		TaxBps: 1000, // 10%
	}}, cfg)

	if got := taxed.Shares["hive:treasury"]; got == nil || got.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("beneficiary should receive 10%% of 1000, got %v", got)
	}
	if sum(taxed).Cmp(sum(untaxed)) != 0 {
		t.Fatalf("the tax must move value, not destroy it: untaxed %v, taxed %v",
			sum(untaxed), sum(taxed))
	}
	// author and curator both bear it: 900 split 50/50
	if got := taxed.Shares["hive:alice"]; got.Cmp(big.NewInt(450)) != 0 {
		t.Fatalf("author should earn on the post AFTER tax, got %v", got)
	}
}

// A tax with no beneficiary must not be collected. Skimming into the void is the
// one outcome worse than not taxing: the operator sees a configured tax and the
// slice is simply destroyed.
func TestCompute_AppTaxWithoutBeneficiaryIsNotCollected(t *testing.T) {
	r := ComputeShares([]Post{{
		Author: "hive:alice", Permlink: "p",
		Votes: []Vote{v("hive:fan", 1000, 0)}, TaxBps: 1000,
	}}, linear) // no AppTaxBeneficiary
	if got := sum(r); got.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("with no beneficiary nothing may be skimmed, got total %v", got)
	}
}
