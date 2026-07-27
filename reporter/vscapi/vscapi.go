// Package vscapi reads VSC contract state over the node's GraphQL API.
//
// Two hard constraints from the node shape the whole package:
//
//  1. getStateByKeys is EXACT-KEY ONLY (1..100 keys, no prefix scan) — the same
//     restriction the contracts themselves live under. So anything that looks
//     like iteration has to be a walk over keys we can name.
//  2. It serves the LATEST state merkle only (GetLastOutput at MaxInt64). There
//     is no "state as of height H" query.
//
// (2) is why historical stake is read the way it is below: C1 stores an append-only
// per-account checkpoint history, so the *current* state still contains every past
// value. We reproduce C1's own binary search off-chain rather than asking the node
// for a historical view it cannot give.
package vscapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a VSC node GraphQL client.
type Client struct {
	Endpoint string
	HTTP     *http.Client
}

func New(endpoint string) *Client {
	return &Client{Endpoint: endpoint, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResp struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Query runs a GraphQL query and decodes `data` into out.
func (c *Client) Query(query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlReq{Query: query, Variables: vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("vsc api: http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var r gqlResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("vsc api: bad json: %w", err)
	}
	if len(r.Errors) > 0 {
		return fmt.Errorf("vsc api: %s", r.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Data, out)
}

const stateQuery = `query($c:String!,$k:[String!]!){getStateByKeys(contractId:$c,keys:$k)}`

// StateGet reads contract state keys. A key that is absent is simply missing from
// the returned map — callers must not treat "" and "absent" as different, because
// the wasm runtime itself returns an empty string for a missing key.
func (c *Client) StateGet(contractID string, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return map[string]string{}, nil
	}
	if len(keys) > 100 {
		return nil, fmt.Errorf("vsc api: getStateByKeys accepts at most 100 keys, got %d", len(keys))
	}
	var out struct {
		GetStateByKeys map[string]any `json:"getStateByKeys"`
	}
	if err := c.Query(stateQuery, map[string]any{"c": contractID, "k": keys}, &out); err != nil {
		return nil, err
	}
	res := make(map[string]string, len(out.GetStateByKeys))
	for k, v := range out.GetStateByKeys {
		s, ok := v.(string)
		if !ok || s == "" { // nil => key not present
			continue
		}
		res[k] = s
	}
	return res, nil
}

// StateGetOne is a convenience wrapper; ok is false when the key is absent/empty.
func (c *Client) StateGetOne(contractID, key string) (string, bool, error) {
	m, err := c.StateGet(contractID, []string{key})
	if err != nil {
		return "", false, err
	}
	v, ok := m[key]
	return v, ok, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
