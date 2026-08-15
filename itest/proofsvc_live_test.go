package itest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"magi_token/indexer/proofsvc"
)

// proofsvc over real HTTP, against the rows the indexer mappings describe, filled
// with logs the contract actually emitted.
//
// The in-process test (proofsvc_endtoend_test.go) proves the rebuild and the proof.
// What it does not touch is everything between: the GraphQL query this service
// sends, the envelope it parses, the HTTP handler, and the mapping that decides
// which log field becomes which column. Those are the parts that break silently in
// a deployment, because a wrong column name yields an empty result set and an empty
// result set reads exactly like "the indexer is behind".
//
// Not covered here, and worth stating: magi-mongo-indexer itself is not run. This
// asserts that the mapping's JSONPaths resolve against real emitted events and that
// the service works against a server speaking Hasura's protocol — not that mongo
// ingests them. That last step needs a deployment.

// hasuraStub speaks enough of Hasura's GraphQL protocol for the real client.
type hasuraStub struct {
	roots  []proofsvc.RootRow
	pages  []proofsvc.SharesRow
	secret string
	calls  int
}

func (h *hasuraStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls++
		if h.secret != "" && r.Header.Get("x-hasura-admin-secret") != h.secret {
			// Hasura answers 200 with an errors array, not a 4xx — the shape the
			// client has to handle, and the reason it checks errors at all.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"errors":[{"message":"invalid x-hasura-admin-secret"}]}`)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("proofsvc sent a body Hasura could not parse: %v", err)
			return
		}
		// the client must scope by channel AND epoch, or it would serve one
		// epoch's book under another's root
		for _, want := range []string{"channel", "epoch"} {
			if _, ok := req.Variables[want]; !ok {
				t.Errorf("query variables missing %q: %v", want, req.Variables)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "magi_tokenomics_root_events"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"magi_tokenomics_root_events": h.roots}})
		case strings.Contains(req.Query, "magi_tokenomics_shares_events"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"magi_tokenomics_shares_events": h.pages}})
		default:
			t.Errorf("proofsvc queried an unexpected table: %s", req.Query)
		}
	})
}

func TestProofsvc_ServesOverHTTPAgainstHasuraProtocol(t *testing.T) {
	ct := mkSetup(t)
	shares := mkShares(10)
	b := shareBookBig(shares)

	// publish for real, then take the rows from the CONTRACT'S OWN logs
	var logs []string
	logs = append(logs, collectLogs(t, ct, mkDist, "submitShares", fmt.Sprintf(
		`{"channel":"content","epoch":"0","page":"0","entries":"%s"}`, entriesOf(b)),
		"hive:creporter", 1)...)
	logs = append(logs, collectLogs(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s","accounts":"%d"}`,
		b.tree.Root(), b.total.String(), len(b.tree.Leaves)), "hive:creporter", 1)...)
	call(t, ct, mkDist, "finalizeEpoch", `{"channel":"content","epoch":"0"}`, "hive:creporter", 1, true)

	pages, roots := rowsFromLogs(t, logs)
	stub := &hasuraStub{roots: roots, pages: pages, secret: "s3cret"}
	gql := httptest.NewServer(stub.handler(t))
	t.Cleanup(gql.Close)

	// the REAL Hasura client and the REAL HTTP handler
	svc := proofsvc.New(proofsvc.NewHasura(gql.URL, "s3cret"))
	api := httptest.NewServer(svc.Handler())
	t.Cleanup(api.Close)

	resp, err := http.Get(api.URL + "/proof?channel=content&epoch=0&account=hive:alice")
	if err != nil {
		t.Fatalf("GET /proof: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /proof returned %d: %s", resp.StatusCode, body)
	}
	var got struct {
		Share        string `json:"share"`
		ClaimPayload string `json:"claim_payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding /proof: %v", err)
	}
	if stub.calls < 2 {
		t.Fatalf("the service answered without querying both tables (%d calls)", stub.calls)
	}

	// THE ASSERTION: pay out with exactly what came back over the wire.
	before := balanceOf(t, ct, tokenID, "hive:alice")
	call(t, ct, mkDist, "claim", got.ClaimPayload, "hive:alice", 2, true)
	after := balanceOf(t, ct, tokenID, "hive:alice")
	b0, _ := new(big.Int).SetString(before, 10)
	b1, _ := new(big.Int).SetString(after, 10)
	if b1.Cmp(b0) <= 0 {
		t.Fatalf("a proof fetched over HTTP did not pay: %s -> %s", before, after)
	}
	t.Logf("claimed with an HTTP-served proof: share=%s, balance %s -> %s", got.Share, before, after)

	// a wrong secret must surface as an error, not as an empty book
	bad := proofsvc.New(proofsvc.NewHasura(gql.URL, "wrong"))
	badAPI := httptest.NewServer(bad.Handler())
	t.Cleanup(badAPI.Close)
	r2, err := http.Get(badAPI.URL + "/root?channel=content&epoch=0")
	if err != nil {
		t.Fatalf("GET /root: %v", err)
	}
	defer r2.Body.Close()
	body, _ := io.ReadAll(r2.Body)
	if r2.StatusCode == http.StatusOK {
		t.Fatalf("a rejected credential produced a served book: %s", body)
	}
	if !strings.Contains(string(body), "invalid x-hasura-admin-secret") {
		t.Errorf("a GraphQL error was not surfaced; a caller would read this as the indexer "+
			"being behind: %s", body)
	}
}

// The mapping is the contract between what the chain emits and what the indexer
// stores. A JSONPath naming a field the contract does not emit produces a NULL
// column, and a NULL column reads downstream as "not reported yet".
func TestIndexerMappingPathsResolveAgainstRealEvents(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "indexer", "magi_tokenomics_mappings.yaml"))
	if err != nil {
		t.Fatalf("read mappings: %v", err)
	}
	var doc struct {
		Contracts []struct {
			Events []struct {
				LogType string            `yaml:"log_type"`
				Table   string            `yaml:"table"`
				Fields  map[string]string `yaml:"fields"`
			} `yaml:"events"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("mappings are not valid yaml: %v", err)
	}

	// real events, from a real run
	ct := mkSetup(t)
	b := shareBookBig(mkShares(6))
	var logs []string
	logs = append(logs, collectLogs(t, ct, mkDist, "submitShares", fmt.Sprintf(
		`{"channel":"content","epoch":"0","page":"0","entries":"%s,notaledger:5"}`, entriesOf(b)),
		"hive:creporter", 1)...)
	logs = append(logs, collectLogs(t, ct, mkDist, "submitRoot", fmt.Sprintf(
		`{"channel":"content","epoch":"0","root":"%s","totalShares":"%s","accounts":"%d"}`,
		b.tree.Root(), b.total.String(), len(b.tree.Leaves)), "hive:creporter", 1)...)
	logs = append(logs, collectLogs(t, ct, mkDist, "finalizeEpoch",
		`{"channel":"content","epoch":"0"}`, "hive:creporter", 1)...)

	// Key by (log_type, scope). Two contracts emit a log type called "skip" —
	// C1's airdrop skip carries batch_id, C3's shares skip carries channel/epoch/
	// page — and they are told apart by their `scope` attribute. Collapsing them
	// under the bare type name made this test report a real mapping as broken.
	key := func(logType string, attrs map[string]any) string {
		if sc, ok := attrs["scope"].(string); ok && sc != "" {
			return logType + "/" + sc
		}
		return logType
	}
	byType := map[string]map[string]any{}
	for _, rawLog := range logs {
		var top struct {
			Type  string         `json:"type"`
			Attrs map[string]any `json:"attributes"`
		}
		if json.Unmarshal([]byte(rawLog), &top) == nil && top.Type != "" {
			byType[key(top.Type, top.Attrs)] = top.Attrs
		}
	}
	if len(byType) == 0 {
		t.Fatal("captured no events — this test is not checking anything")
	}

	checked, skipped := 0, []string{}
	for _, c := range doc.Contracts {
		for _, lg := range c.Events {
			// a mapping's own scope field tells us which variant it describes
			want := lg.LogType
			if sc, ok := lg.Fields["scope"]; ok {
				_ = sc
				for k := range byType {
					if strings.HasPrefix(k, lg.LogType+"/") && strings.Contains(lg.Table, strings.TrimPrefix(k, lg.LogType+"/")) {
						want = k
					}
				}
			}
			attrs, ok := byType[want]
			if !ok {
				skipped = append(skipped, lg.LogType)
				continue
			}
			for col, path := range lg.Fields {
				const pfx = "$.attributes."
				if !strings.HasPrefix(path, pfx) {
					continue // block/tx metadata, not an event attribute
				}
				checked++
				if _, present := attrs[strings.TrimPrefix(path, pfx)]; !present {
					keys := make([]string, 0, len(attrs))
					for k := range attrs {
						keys = append(keys, k)
					}
					t.Errorf("%s.%s maps to %s, which the %q event does not emit. The column "+
						"would be NULL, and a NULL reads downstream as \"not reported yet\".\n"+
						"  emitted: %v", lg.Table, col, path, lg.LogType, keys)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("resolved no field paths — this test is not checking anything")
	}
	t.Logf("checked %d mapped fields against real events; log types not produced here: %v",
		checked, skipped)
}

// entriesOf renders a book as the entries string submitShares takes.
func entriesOf(b *book) string {
	parts := make([]string, 0, len(b.tree.Leaves))
	for _, l := range b.tree.Leaves {
		parts = append(parts, l.Account+":"+l.Share)
	}
	return strings.Join(parts, ",")
}
