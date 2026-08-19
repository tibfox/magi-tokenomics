package proofsvc

import (
	"encoding/json"
	"strings"
	"testing"
)

// The rows must decode from the shape Hasura ACTUALLY sends.
//
// `total_shares` and `page_total` are declared `numeric` in
// indexer/magi_tokenomics_mappings.yaml, magi-mongo-indexer creates them as Postgres
// NUMERIC, and Hasura serialises NUMERIC as an UNQUOTED JSON number. The Go structs
// declared them `string`, so json.Unmarshal failed and every /proof and /root
// returned 503.
//
// The existing live test could not catch this: its stub answers by JSON-encoding
// []RootRow straight back out, so it emits whatever the struct says. The fake agreed
// with the code by construction — the exact hazard reporter/README.md flags for a
// second implementation of the same thing. These fixtures are therefore RAW JSON
// text, written the way the wire has it, never round-tripped through the struct.
//
// Note the same `numeric` mapping type is decoded as int64 for epoch, page, entries
// and accounts. Those four and these two cannot both be right.

const wireRoot = `{"magi_tokenomics_root_events":[
  {"channel":"content","epoch":0,"root":"ab12","total_shares":208552153997506000,"accounts":502}
]}`

const wirePages = `{"magi_tokenomics_shares_events":[
  {"channel":"content","epoch":0,"page":0,"entries":2,"page_total":150,"submitted":"hive:alice:100,hive:bob:50"}
]}`

func TestWire_RootRowDecodesUnquotedNumeric(t *testing.T) {
	var out struct {
		Rows []RootRow `json:"magi_tokenomics_root_events"`
	}
	if err := json.Unmarshal([]byte(wireRoot), &out); err != nil {
		t.Fatalf("decoding the shape Hasura sends: %v", err)
	}
	if len(out.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out.Rows))
	}
	r := out.Rows[0]
	// The denominator every payout divides by. It must survive the wire exactly —
	// a NUMERIC big enough to lose precision through float64 is the normal case at
	// scale (this value is the one the 502-earner scale run produces).
	if got := r.TotalShares.String(); got != "208552153997506000" {
		t.Errorf("total_shares = %q, want 208552153997506000", got)
	}
	if r.Accounts != 502 || r.Epoch != 0 || r.Channel != "content" {
		t.Errorf("other columns decoded wrong: %+v", r)
	}
}

func TestWire_SharesRowDecodesUnquotedNumeric(t *testing.T) {
	var out struct {
		Rows []SharesRow `json:"magi_tokenomics_shares_events"`
	}
	if err := json.Unmarshal([]byte(wirePages), &out); err != nil {
		t.Fatalf("decoding the shape Hasura sends: %v", err)
	}
	p := out.Rows[0]
	if got := p.PageTotal.String(); got != "150" {
		t.Errorf("page_total = %q, want 150", got)
	}
	if p.Entries != 2 || p.Page != 0 {
		t.Errorf("other columns decoded wrong: %+v", p)
	}
}

// A quoted value must still decode. Hasura's NUMERIC serialisation is what it is
// today, but the reconciliations downstream are `ok`-guarded on parsing this text —
// if a future Hasura, a view, or a cast ever quotes it, silently reading "" would
// switch the page-total and committed-total checks OFF rather than fail loudly.
func TestWire_QuotedNumericAlsoDecodes(t *testing.T) {
	var out struct {
		Rows []RootRow `json:"magi_tokenomics_root_events"`
	}
	q := `{"magi_tokenomics_root_events":[{"channel":"c","epoch":1,"root":"ff","total_shares":"12345","accounts":3}]}`
	if err := json.Unmarshal([]byte(q), &out); err != nil {
		t.Fatalf("a quoted numeric must decode too: %v", err)
	}
	if got := out.Rows[0].TotalShares.String(); got != "12345" {
		t.Errorf("total_shares = %q, want 12345", got)
	}
}

// An absent or null column must read as empty rather than as a bogus zero: the
// integrity checks treat unparseable as "not asserted", and a silent 0 would make a
// missing denominator look like a real one.
func TestWire_NullNumericIsEmptyNotZero(t *testing.T) {
	var out struct {
		Rows []RootRow `json:"magi_tokenomics_root_events"`
	}
	q := `{"magi_tokenomics_root_events":[{"channel":"c","epoch":1,"root":"ff","total_shares":null,"accounts":3}]}`
	if err := json.Unmarshal([]byte(q), &out); err != nil {
		t.Fatalf("a null numeric must not fail the whole decode: %v", err)
	}
	if got := out.Rows[0].TotalShares.String(); got != "" {
		t.Errorf("null total_shares = %q, want empty", got)
	}
}

// Queries must be scoped to one distributor when an indexer serves several.
//
// magi-mongo-indexer adds indexer_contract_id to every table, but the queries
// filtered on (channel, epoch) alone. The collision is the likely case rather than a
// contrived one: channel names are conventional ("content", "lp") and every
// deployment numbers its epochs from 0, so two tenants collide on their very first
// epoch — and BuildBook then reconciles one contract's pages against another's root
// and fails, denying proofs to both.
func TestWire_QueriesScopeToTheDistributorWhenSet(t *testing.T) {
	h := &Hasura{}
	q, vars := h.scope(rootQuery, rootQueryScoped, map[string]any{"channel": "content", "epoch": int64(0)})
	if _, ok := vars["contract"]; ok {
		t.Error("an unset Contract must stay unscoped — existing single-tenant deployments " +
			"have no reason to change")
	}
	if q != rootQuery {
		t.Error("unset Contract must use the unscoped query")
	}

	h.Contract = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
	q, vars = h.scope(rootQuery, rootQueryScoped, map[string]any{"channel": "content", "epoch": int64(0)})
	if vars["contract"] != h.Contract {
		t.Errorf("a set Contract must be passed as a variable, got %v", vars["contract"])
	}
	if !strings.Contains(q, "indexer_contract_id") {
		t.Error("a set Contract must select the query that filters on indexer_contract_id")
	}
}
