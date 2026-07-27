package vscapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// realResponse is a VERBATIM response from the live testnet node
// (magi-test.techcoderx.com) for getStateByKeys against a deployed contract.
// Keeping the real bytes here pins the two behaviours the client depends on:
// a present key comes back as a string, and an ABSENT key comes back as null.
const realResponse = `{"data":{"getStateByKeys":{"cfg_feeBps":null,"cfg_owner":null,"feeBps":null,"nonexistent_key_xyz":null,"owner":"hive:magi.contracts"}}}`

func serve(t *testing.T, status int, body string, capture *string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			b, _ := io.ReadAll(r.Body)
			*capture = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	return c
}

func TestStateGet_ParsesRealNodeResponse(t *testing.T) {
	var sent string
	c := serve(t, 200, realResponse, &sent)

	got, err := c.StateGet("vsc1BnMAaeUzhzVcfKMDG5vphthhymk6irjLNq",
		[]string{"cfg_owner", "owner", "feeBps", "cfg_feeBps", "nonexistent_key_xyz"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the key that actually has a value should be present. Absent keys must
	// NOT appear as empty strings, because the whole framework distinguishes
	// "unset" from "set" by value, not by nil-ness.
	if len(got) != 1 {
		t.Fatalf("want exactly 1 present key, got %d: %+v", len(got), got)
	}
	if got["owner"] != "hive:magi.contracts" {
		t.Fatalf("owner: %q", got["owner"])
	}
	if _, ok := got["nonexistent_key_xyz"]; ok {
		t.Fatal("a null value must not be reported as present")
	}

	// the request must carry variables, not string-interpolated keys
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(sent), &req); err != nil {
		t.Fatalf("request body not json: %v", err)
	}
	if !strings.Contains(req.Query, "getStateByKeys") {
		t.Fatalf("query: %s", req.Query)
	}
	if req.Variables["c"] != "vsc1BnMAaeUzhzVcfKMDG5vphthhymk6irjLNq" {
		t.Fatalf("contract id not passed as a variable: %+v", req.Variables)
	}
}

func TestStateGetOne(t *testing.T) {
	c := serve(t, 200, realResponse, nil)
	v, ok, err := c.StateGetOne("vsc1x", "owner")
	if err != nil || !ok || v != "hive:magi.contracts" {
		t.Fatalf("got (%q,%v,%v)", v, ok, err)
	}

	v, ok, err = c.StateGetOne("vsc1x", "cfg_owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok || v != "" {
		t.Fatalf("absent key should report ok=false, got (%q,%v)", v, ok)
	}
}

// A GraphQL error arrives with HTTP 200, so it must be read out of the body.
// Treating such a response as "no keys present" would make the reporter compute
// against an empty view of the contract and submit nonsense.
func TestQuery_GraphQLErrorIsNotSilentlyEmpty(t *testing.T) {
	c := serve(t, 200, `{"errors":[{"message":"invalid cid: cid too short"}],"data":null}`, nil)
	_, err := c.StateGet("vsc1bogus", []string{"owner"})
	if err == nil {
		t.Fatal("a graphql error must surface as an error")
	}
	if !strings.Contains(err.Error(), "cid too short") {
		t.Fatalf("error should carry the node's message, got %v", err)
	}
}

func TestQuery_HTTPErrorSurfaces(t *testing.T) {
	c := serve(t, 502, "bad gateway", nil)
	if _, err := c.StateGet("vsc1x", []string{"owner"}); err == nil {
		t.Fatal("a 502 must be an error")
	}
}

func TestQuery_MalformedBodySurfaces(t *testing.T) {
	c := serve(t, 200, "<html>not json</html>", nil)
	if _, err := c.StateGet("vsc1x", []string{"owner"}); err == nil {
		t.Fatal("a non-json body must be an error")
	}
}

// The node caps getStateByKeys at 100 keys and rejects the whole call above it,
// so the client must refuse locally rather than lose an entire batch.
func TestStateGet_RefusesOversizedBatch(t *testing.T) {
	c := serve(t, 200, realResponse, nil)
	keys := make([]string, 101)
	for i := range keys {
		keys[i] = "k"
	}
	if _, err := c.StateGet("vsc1x", keys); err == nil {
		t.Fatal("101 keys must be rejected locally (node limit is 100)")
	}
}

func TestStateGet_EmptyKeysIsANoOp(t *testing.T) {
	// no server: an empty request must not even be sent
	c := New("http://127.0.0.1:1/never-reached")
	got, err := c.StateGet("vsc1x", nil)
	if err != nil {
		t.Fatalf("empty key list should be a no-op, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %+v", got)
	}
}
