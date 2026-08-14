package proofsvc

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"
)

// fixture builds a book the way the chain would: pages of entries, and the root
// that a correct reporter would have committed for them.
type fixture struct {
	pages []SharesRow
	root  RootRow
	fail  error
}

func (f *fixture) Root(string, int64) (RootRow, error)      { return f.root, f.fail }
func (f *fixture) Pages(string, int64) ([]SharesRow, error) { return f.pages, f.fail }

func newFixture(t *testing.T, pageEntries ...string) *fixture {
	t.Helper()
	all := map[string]*big.Int{}
	f := &fixture{}
	for i, e := range pageEntries {
		shares, _ := sharecore.ParseEntries(e)
		for k, v := range shares {
			if cur, ok := all[k]; ok {
				all[k] = new(big.Int).Add(cur, v)
			} else {
				all[k] = v
			}
		}
		f.pages = append(f.pages, SharesRow{
			Channel: "content", Epoch: 0, Page: int64(i),
			Entries: int64(len(shares)), PageTotal: sharecore.TotalOf(shares).String(),
			Submitted: e,
		})
	}
	f.root = RootRow{
		Channel: "content", Epoch: 0, Root: sharecore.BuildTree(all).Root(),
		TotalShares: sharecore.TotalOf(all).String(), Accounts: int64(len(all)),
	}
	return f
}

func get(t *testing.T, s *Server, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestServesAProofThatVerifiesAgainstTheCommittedRoot(t *testing.T) {
	f := newFixture(t, "hive:alice:70,hive:bob:30", "hive:carol:50,hive:dave:25")
	s := New(f)
	code, body := get(t, s, "/proof?channel=content&epoch=0&account=hive:carol")
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, body)
	}
	if body["share"] != "50" {
		t.Fatalf("share = %v, want 50", body["share"])
	}
	path := []string{}
	for _, p := range body["proof"].([]any) {
		path = append(path, p.(string))
	}
	// The whole contract of this service: the proof it hands out must verify
	// against the root the CHAIN holds, using the same verifier the chain uses.
	if !sharecore.VerifyProof("hive:carol", "50", path, f.root.Root) {
		t.Fatal("served a proof that does not verify against the committed root — a claimant " +
			"following this service would have their claim rejected on chain")
	}
	if !strings.Contains(body["claim_payload"].(string), `"channel":"content"`) {
		t.Fatalf("claim_payload is not channel-scoped: %v", body["claim_payload"])
	}
}

// The reason this service commits a root at all. If its copy of the book has been
// tampered with — or is merely wrong — it must refuse, not serve proofs that the
// contract will reject at the point of payment.
func TestRefusesToServeWhenTheRebuiltRootDisagrees(t *testing.T) {
	f := newFixture(t, "hive:alice:70,hive:bob:30")
	// an operator (or an attacker with database access) raises alice's share
	f.pages[0].Submitted = "hive:alice:9000,hive:bob:30"
	f.pages[0].PageTotal = "9030"
	s := New(f)
	code, body := get(t, s, "/proof?channel=content&epoch=0&account=hive:alice")
	if code == http.StatusOK {
		t.Fatalf("served a tampered book: %v", body)
	}
	if !strings.Contains(body["error"].(string), "does not match the committed root") {
		t.Fatalf("refused for the wrong reason: %v", body["error"])
	}
}

// A page-total that disagrees with its own entries means the indexer's copy of
// that row is corrupt, and must be named before the root check swallows it.
func TestRefusesAPageWhoseLoggedTotalDisagrees(t *testing.T) {
	f := newFixture(t, "hive:alice:70,hive:bob:30")
	f.pages[0].PageTotal = "999"
	code, body := get(t, New(f), "/root?channel=content&epoch=0")
	if code == http.StatusOK {
		t.Fatalf("accepted a page whose total disagrees: %v", body)
	}
	if !strings.Contains(body["error"].(string), "does not match what the contract counted") {
		t.Fatalf("refused for the wrong reason: %v", body["error"])
	}
}

// A gap in the pages is the ordinary case of "the indexer is behind". Rebuilding
// from what is present would produce a wrong root, so it must be caught as a gap.
func TestRefusesWhenAPageIsMissing(t *testing.T) {
	f := newFixture(t, "hive:alice:70", "hive:bob:30", "hive:carol:10")
	f.pages = append(f.pages[:1], f.pages[2]) // drop page 1
	code, body := get(t, New(f), "/root?channel=content&epoch=0")
	if code == http.StatusOK {
		t.Fatalf("served an incomplete book: %v", body)
	}
	if !strings.Contains(body["error"].(string), "is missing") {
		t.Fatalf("refused for the wrong reason: %v", body["error"])
	}
}

// "00" is a different epoch string on chain and `claim` refuses it. Handing out a
// proof under an alias no claim can use would strand the claimant.
func TestRejectsNonCanonicalEpochSpellings(t *testing.T) {
	s := New(newFixture(t, "hive:alice:70"))
	for _, ep := range []string{"00", "0x0", "+0", " 0", "-1"} {
		code, _ := get(t, s, "/proof?channel=content&epoch="+url.QueryEscape(ep)+"&account=hive:alice")
		if code != http.StatusBadRequest {
			t.Errorf("epoch %q accepted with %d, want 400", ep, code)
		}
	}
}

func TestAccountWithNoShareIsNotFoundRatherThanEmpty(t *testing.T) {
	code, body := get(t, New(newFixture(t, "hive:alice:70")),
		"/proof?channel=content&epoch=0&account=hive:nobody")
	if code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %v", code, body)
	}
	if !strings.Contains(body["error"].(string), "earned nothing") {
		t.Fatalf("unhelpful error: %v", body["error"])
	}
}

// A failed build must not be cached: the common cause is the indexer lagging, and
// freezing that into a permanent refusal would outlast the condition.
func TestATransientFailureIsNotCached(t *testing.T) {
	f := newFixture(t, "hive:alice:70,hive:bob:30")
	good := f.pages[0].Submitted
	f.pages[0].Submitted = "hive:alice:1"
	s := New(f)
	if code, _ := get(t, s, "/root?channel=content&epoch=0"); code == http.StatusOK {
		t.Fatal("expected the first build to fail")
	}
	f.pages[0].Submitted = good
	if code, body := get(t, s, "/root?channel=content&epoch=0"); code != http.StatusOK {
		t.Fatalf("still refusing after the source recovered (%d): %v — a cached failure "+
			"outlives the indexer lag that caused it", code, body)
	}
}

// Skipped entries are no-ops inside a SUCCESSFUL transaction: nothing else records
// that the earner was ever present, so this service is where they surface.
func TestSkippedEntriesAreReported(t *testing.T) {
	f := newFixture(t, "hive:alice:70,bob:30")
	code, body := get(t, New(f), "/root?channel=content&epoch=0")
	if code != http.StatusOK {
		t.Fatalf("got %d: %v", code, body)
	}
	sk := body["skipped"].([]any)
	if len(sk) != 1 || !strings.Contains(sk[0].(map[string]any)["reason"].(string), "ledger") {
		t.Fatalf("skipped entries not surfaced: %v", body["skipped"])
	}
}
