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
	health     uint64 // per-POOL indexed height
	noPoolLogs bool   // pool has no logs at all
}

// health lets a test pretend the indexer is at a given block. Zero means "far ahead",
// so existing tests are unaffected by the freshness gate.
func (f *fakeIndexer) Query(query string, vars map[string]any, out any) error {
	f.calls++
	if strings.Contains(query, "contract_logs_aggregate") {
		h := f.health
		if h == 0 {
			h = 1 << 62
		}
		if f.noPoolLogs {
			return json.Unmarshal([]byte(
				`{"data":{"contract_logs_aggregate":{"aggregate":{"max":{"block_height":null},"count":0}}}}`), out)
		}
		return json.Unmarshal([]byte(fmt.Sprintf(
			`{"data":{"contract_logs_aggregate":{"aggregate":{"max":{"block_height":%d},"count":7}}}}`, h)), out)
	}
	if strings.Contains(query, "indexer_health") {
		// GLOBAL height, deliberately far ahead of the pool's — the gate must not
		// be satisfied by this.
		return json.Unmarshal([]byte(
			`{"data":{"indexer_health":[{"latest_block_height":999999999}]}}`), out)
	}
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

// The distributor credits only domain-prefixed ledger addresses and silently skips
// the rest. A bare provider name would therefore not be one bad entry — it would be
// an epoch that pays nobody while its funding accumulates, with nothing logged.
func TestLPShares_BareProviderNameIsRefused(t *testing.T) {
	f := &fakeIndexer{adds: []row{{"alice", "1000", 1}}}
	_, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err == nil {
		t.Fatal("a bare provider name must be refused, not submitted")
	}
	if !strings.Contains(err.Error(), "ledger address") {
		t.Fatalf("error should explain the prefix requirement, got: %v", err)
	}
}

// contract: and did: providers are legitimate — a contract can hold LP.
func TestLPShares_AcceptsAllLedgerDomains(t *testing.T) {
	f := &fakeIndexer{adds: []row{
		{"hive:alice", "100", 1},
		{"contract:vsc1pool", "200", 1},
		{"did:key:z6Mk", "300", 1},
	}}
	res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err != nil {
		t.Fatalf("all ledger domains must be accepted: %v", err)
	}
	if len(res.Shares) != 3 {
		t.Fatalf("want 3 providers, got %v", mustShares(t, res))
	}
}

// A provider name is untrusted input that becomes an on-chain payload. The contract
// splits entries on commas, so a comma in a name injects entries — and with a colon
// too, an arbitrary share for an arbitrary account. This is theft of a whole epoch.
func TestLPShares_ProviderCannotInjectEntries(t *testing.T) {
	for _, tc := range []struct{ name, provider, want string }{
		{"comma redirects a share", "hive:evil,hive:victim", "comma"},
		{"comma+colon mints a share", "hive:a,hive:attacker:999999999,", "comma"},
		{"trailing colon dilutes", "hive:alice:", "colon"},
		{"space", "hive:al ice", "unsafe character"},
		{"newline", "hive:alice\n", "unsafe character"},
		{"quote", "hive:alice\"", "unsafe character"},
		{"non-ascii", "hive:alicé", "unsafe character"},
	} {
		f := &fakeIndexer{adds: []row{{tc.provider, "100", 1}}}
		_, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
		if err == nil {
			t.Fatalf("%s: provider %q was accepted — it must be refused", tc.name, tc.provider)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error should mention %q, got: %v", tc.name, tc.want, err)
		}
	}
}

// Multiple colons must stay legal: did:key:... is a real provider and the contract's
// last-colon split parses it correctly. Over-tightening would break DID providers.
func TestLPShares_MultiColonDidStillAccepted(t *testing.T) {
	f := &fakeIndexer{adds: []row{{"did:key:z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8", "100", 1}}}
	res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err != nil {
		t.Fatalf("a multi-colon DID provider must be accepted: %v", err)
	}
	if len(res.Shares) != 1 {
		t.Fatalf("want the DID credited, got %v", mustShares(t, res))
	}
}

// An indexer that never returns a short page would otherwise spin forever and grow
// the process until it is killed.
type floodIndexer struct{ calls int }

func (f *floodIndexer) Query(query string, vars map[string]any, out any) error {
	f.calls++
	if strings.Contains(query, "contract_logs_aggregate") {
		return json.Unmarshal([]byte(
			`{"data":{"contract_logs_aggregate":{"aggregate":{"max":{"block_height":999999},"count":3}}}}`), out)
	}
	limit := vars["limit"].(int)
	rows := make([]string, 0, limit)
	for i := 0; i < limit; i++ { // always a FULL page
		rows = append(rows, `{"provider":"hive:a","amount":1,"indexer_block_height":1}`)
	}
	return json.Unmarshal([]byte(`{"data":{"rows":[`+strings.Join(rows, ",")+`]}}`), out)
}

func TestLPShares_UnboundedIndexerIsCutOff(t *testing.T) {
	f := &floodIndexer{}
	_, err := LPShares(f, Options{Pool: "p", Start: 1, End: 2, PageSize: 1000})
	if err == nil {
		t.Fatal("an indexer that never pages out must be cut off, not followed forever")
	}
	if !strings.Contains(err.Error(), "not honouring") {
		t.Fatalf("error should name the cause, got: %v", err)
	}
	t.Logf("cut off after %d calls", f.calls)
}

// A lagging indexer does not error — it returns FEWER ROWS — so scoring an epoch it
// has not reached underpays providers irreversibly. Refuse unless it can be PROVEN
// to have indexed past the epoch end.
func TestLPShares_RefusesWhenIndexerMayBeBehind(t *testing.T) {
	f := &fakeIndexer{
		adds:   []row{{"hive:alice", "1000", 10}},
		health: 90, // epoch ends at 100, so blocks 91..100 are unverified
	}
	_, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err == nil {
		t.Fatal("an epoch past the indexer's newest log must be refused")
	}
	for _, want := range []string{"indexer may be behind", "91..100", "allow_stale"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}
}

// Proof of sufficiency: indexed at or past the epoch end is enough.
func TestLPShares_ProceedsWhenIndexerIsProvablyAhead(t *testing.T) {
	for _, h := range []uint64{100, 101, 5000} {
		f := &fakeIndexer{adds: []row{{"hive:alice", "1000", 10}}, health: h}
		res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
		if err != nil {
			t.Fatalf("health %d >= end 100 must pass, got: %v", h, err)
		}
		if len(res.Shares) != 1 {
			t.Fatalf("health %d: want alice credited, got %v", h, mustShares(t, res))
		}
	}
}

// The escape hatch works, and must be explicit — never the default.
func TestLPShares_AllowStaleOverridesTheGate(t *testing.T) {
	f := &fakeIndexer{adds: []row{{"hive:alice", "1000", 10}}, health: 90}
	if _, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100}); err == nil {
		t.Fatal("the gate must be ON by default")
	}
	res, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100, AllowStale: true})
	if err != nil {
		t.Fatalf("AllowStale must bypass the gate: %v", err)
	}
	if len(res.Shares) != 1 {
		t.Fatalf("want alice credited, got %v", mustShares(t, res))
	}
}

// A broken health query must not be read as "fresh".
func TestLPShares_UnreadableHealthIsNotTreatedAsFresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"indexer_health not found"}]}`)
	}))
	defer srv.Close()
	_, err := LPShares(&HTTPTransport{Endpoint: srv.URL}, Options{Pool: "p", Start: 1, End: 2})
	if err == nil || !strings.Contains(err.Error(), "cannot verify indexer freshness") {
		t.Fatalf("an unreadable health view must fail closed, got: %v", err)
	}
}

// The gate must be measured PER POOL. indexer_health is a global max over every
// tracked contract, but each contract advances its own cursor and discovered pools
// backfill from zero — so a pool can be two million blocks behind while the global
// figure sits at head. An earlier version of this gate used the global number.
func TestLPShares_FreshnessIsScopedToThePoolNotGlobal(t *testing.T) {
	f := &fakeIndexer{
		adds:   []row{{"hive:alice", "1000", 10}},
		health: 90, // this POOL is only indexed to 90; the fake's global says 999999999
	}
	_, err := LPShares(f, Options{Pool: "p", Start: 50, End: 100})
	if err == nil {
		t.Fatal("a pool behind the epoch end must be refused even when the global height is ahead")
	}
	if !strings.Contains(err.Error(), "behind on pool") {
		t.Fatalf("error should name the POOL as the thing that is behind, got: %v", err)
	}
	// and it should surface the misleading global figure as a diagnostic
	if !strings.Contains(err.Error(), "999999999") {
		t.Fatalf("error should cite the global height to explain the discrepancy, got: %v", err)
	}
}

// A pool with no logs cannot be distinguished from one never discovered, so it is
// unproven rather than empty.
func TestLPShares_PoolWithNoLogsIsUnproven(t *testing.T) {
	f := &fakeIndexer{adds: []row{{"hive:alice", "1000", 10}}, noPoolLogs: true}
	_, err := LPShares(f, Options{Pool: "vsc1nope", Start: 50, End: 100})
	if err == nil || !strings.Contains(err.Error(), "no logs at all") {
		t.Fatalf("an unknown pool must be refused as unproven, got: %v", err)
	}
	if !strings.Contains(err.Error(), "vsc1nope") {
		t.Fatalf("error should name the pool, got: %v", err)
	}
}
