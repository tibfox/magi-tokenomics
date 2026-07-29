package lpsrc

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"
)

// row is a test fixture event.
type row struct {
	provider string
	amount   string
	height   uint64
}

// fakeIndexer serves add/rem rows with real paging, so the offset walk is exercised
// rather than assumed.
type fakeIndexer struct {
	adds, rems []row
	pageSize   int
	calls      int
}

func (f *fakeIndexer) Query(query string, vars map[string]any, out any) error {
	f.calls++
	src := f.adds
	amountKey := "lp_minted"
	if strings.Contains(query, "rem_liq") {
		src = f.rems
		amountKey = "lp_burned"
	}
	h := uint64(vars["h"].(uint64))
	limit := vars["limit"].(int)
	offset := vars["offset"].(int)

	kept := []row{}
	for _, r := range src {
		if r.height <= h {
			kept = append(kept, r)
		}
	}
	end := offset + limit
	if end > len(kept) {
		end = len(kept)
	}
	page := []row{}
	if offset < len(kept) {
		page = kept[offset:end]
	}
	items := make([]string, 0, len(page))
	for _, r := range page {
		items = append(items, fmt.Sprintf(
			`{"provider":%q,"amount":%s,"indexer_block_height":%d}`, r.provider, r.amount, r.height))
	}
	_ = amountKey
	body := fmt.Sprintf(`{"data":{"rows":[%s]}}`, strings.Join(items, ","))
	return json.Unmarshal([]byte(body), out)
}

func mustShares(t *testing.T, r sharecore.Result) map[string]string {
	t.Helper()
	got := map[string]string{}
	for k, v := range r.Shares {
		got[k] = v.String()
	}
	return got
}

// The headline rule: liquidity must be present at BOTH boundaries to earn, so a
// provider is credited the SMALLER of its two positions.
func TestLPShares_CreditsMinOfBothBoundaries(t *testing.T) {
	f := &fakeIndexer{
		adds: []row{
			{"hive:steady", "1000", 10}, // in before the epoch, never moves
			{"hive:joiner", "5000", 55}, // joins mid-epoch: 0 at start
			{"hive:topper", "1000", 10}, // tops up mid-epoch: earns the SMALLER
			{"hive:topper", "9000", 60},
		},
		rems: []row{
			{"hive:exiter", "800", 70}, // exits mid-epoch; seeded below
		},
	}
	f.adds = append(f.adds, row{"hive:exiter", "800", 5})

	res, err := LPShares(f, Options{Pool: "vsc1pool", Start: 50, End: 100})
	if err != nil {
		t.Fatalf("LPShares: %v", err)
	}
	got := mustShares(t, res)
	want := map[string]string{
		"hive:steady": "1000",
		"hive:topper": "1000", // NOT 10000 — the top-up arrived mid-epoch
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if _, ok := got["hive:joiner"]; ok {
		t.Fatalf("joiner had no position at the start and must earn nothing: %v", got)
	}
	if _, ok := got["hive:exiter"]; ok {
		t.Fatalf("exiter withdrew before the end and must earn nothing: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected extra providers: %v", got)
	}
	if res.Total.String() != "2000" {
		t.Fatalf("total = %s, want 2000", res.Total)
	}
}

// Flash liquidity: in and out entirely inside the epoch earns nothing, which is the
// whole point of evaluating both boundaries.
func TestLPShares_FlashLiquidityEarnsNothing(t *testing.T) {
	f := &fakeIndexer{
		adds: []row{{"hive:flash", "1000000", 60}},
		rems: []row{{"hive:flash", "1000000", 61}},
	}
	res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err != nil {
		t.Fatalf("LPShares: %v", err)
	}
	if len(res.Shares) != 0 {
		t.Fatalf("flash liquidity earned %v, want nothing", mustShares(t, res))
	}
}

// Amounts above 2^53 must survive. A float64 round-trip would corrupt these, which
// is exactly how the Hive path's rshares bug behaved.
func TestLPShares_BigAmountsKeepFullPrecision(t *testing.T) {
	huge := "123456789012345678901234567890"
	f := &fakeIndexer{adds: []row{{"hive:whale", huge, 1}}}
	res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err != nil {
		t.Fatalf("LPShares: %v", err)
	}
	if got := mustShares(t, res)["hive:whale"]; got != huge {
		t.Fatalf("precision lost: got %s, want %s", got, huge)
	}
}

// Paging must be transparent: the same answer whatever the page size.
func TestLPShares_PagingIsTransparent(t *testing.T) {
	adds := []row{}
	for i := 0; i < 25; i++ {
		adds = append(adds, row{fmt.Sprintf("hive:lp%02d", i), "100", 1})
	}
	var first map[string]string
	for _, ps := range []int{1, 2, 7, 25, 1000} {
		f := &fakeIndexer{adds: adds}
		res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100, PageSize: ps})
		if err != nil {
			t.Fatalf("pageSize %d: %v", ps, err)
		}
		got := mustShares(t, res)
		if len(got) != 25 {
			t.Fatalf("pageSize %d returned %d providers, want 25", ps, len(got))
		}
		if first == nil {
			first = got
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(first) {
			t.Fatalf("pageSize %d disagreed with the first run", ps)
		}
	}
}

// Determinism is the precondition for Attest mode: identical input must yield
// byte-identical canonical output, independent of page size or map iteration order.
func TestLPShares_CanonicalOutputIsStable(t *testing.T) {
	adds := []row{
		{"hive:zed", "300", 1}, {"hive:alice", "100", 2}, {"hive:mid", "200", 3},
	}
	want := ""
	for i, ps := range []int{1, 3, 1000} {
		f := &fakeIndexer{adds: adds}
		res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100, PageSize: ps})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := sharecore.Canonicalize(res)
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("canonical output differs across page sizes:\n %q\n %q", got, want)
		}
	}
	if want != "hive:alice:100,hive:mid:200,hive:zed:300" {
		t.Fatalf("canonical form unexpected: %q", want)
	}
}

// A GraphQL error arrives inside a 200 body. Ignoring it would read as "no rows" —
// i.e. every provider silently earning nothing, on a schedule that keeps paying.
func TestLPShares_GraphQLErrorIsNotSilentlyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, `{"errors":[{"message":"field \"lp_minted\" not found"}]}`)
	}))
	defer srv.Close()
	_, err := LPShares(&HTTPTransport{Endpoint: srv.URL}, Options{Pool: "p", Start: 1, End: 2})
	if err == nil {
		t.Fatal("a GraphQL error must fail loudly, not return an empty share set")
	}
	if !strings.Contains(err.Error(), "lp_minted") {
		t.Fatalf("error should surface the indexer's message, got: %v", err)
	}
}

// Burns exceeding mints cannot happen on-chain, so it means the data is wrong.
// Crediting 0 and carrying on would hide that every other balance is suspect too.
func TestLPShares_InconsistentDataFailsLoudly(t *testing.T) {
	f := &fakeIndexer{
		adds: []row{{"hive:a", "100", 1}},
		rems: []row{{"hive:a", "500", 2}},
	}
	_, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err == nil || !strings.Contains(err.Error(), "negative LP balance") {
		t.Fatalf("want a loud inconsistency error, got: %v", err)
	}
}

func TestLPShares_RejectsBadOptions(t *testing.T) {
	if _, err := LPShares(&fakeIndexer{}, Options{Start: 1, End: 2}); err == nil {
		t.Fatal("missing pool must be rejected")
	}
	if _, err := LPShares(&fakeIndexer{}, Options{Pool: "p", Start: 9, End: 2}); err == nil {
		t.Fatal("end before start must be rejected")
	}
}

var _ = big.NewInt
