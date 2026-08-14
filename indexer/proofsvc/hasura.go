package proofsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Hasura reads the rows the indexer mappings produce.
type Hasura struct {
	Endpoint string
	Secret   string // x-hasura-admin-secret, optional
	Client   *http.Client
}

func NewHasura(endpoint, secret string) *Hasura {
	return &Hasura{Endpoint: endpoint, Secret: secret, Client: &http.Client{Timeout: 20 * time.Second}}
}

func (h *Hasura) query(q string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Secret != "" {
		req.Header.Set("x-hasura-admin-secret", h.Secret)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hasura returned %s", resp.Status)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	// GraphQL reports failure in the body with a 200, so an unchecked errors
	// array becomes an empty result set — and an empty page list is how this
	// service decides a book is incomplete. Reporting it as an error keeps a
	// query fault from being read as "the indexer is behind".
	if len(env.Errors) > 0 {
		return fmt.Errorf("hasura: %s", env.Errors[0].Message)
	}
	return json.Unmarshal(env.Data, out)
}

const rootQuery = `query Root($channel: String!, $epoch: numeric!) {
  magi_tokenomics_root_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch}}) {
    channel epoch root total_shares accounts
  }
}`

const pagesQuery = `query Pages($channel: String!, $epoch: numeric!) {
  magi_tokenomics_shares_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch}},
                                order_by: {page: asc}) {
    channel epoch page entries page_total submitted
  }
}`

func (h *Hasura) Root(channel string, epoch int64) (RootRow, error) {
	var out struct {
		Rows []RootRow `json:"magi_tokenomics_root_events"`
	}
	if err := h.query(rootQuery, map[string]any{"channel": channel, "epoch": epoch}, &out); err != nil {
		return RootRow{}, err
	}
	if len(out.Rows) == 0 {
		return RootRow{}, fmt.Errorf("no root committed for %s epoch %d: either the reporter "+
			"has not submitted one yet, or the indexer has not ingested it", channel, epoch)
	}
	// One root per (channel, epoch) is the contract's rule — submitRoot refuses to
	// overwrite. More than one row means the indexer double-ingested, and picking
	// one arbitrarily could serve proofs under a root the chain does not hold.
	if len(out.Rows) > 1 {
		return RootRow{}, fmt.Errorf("%d roots for %s epoch %d: the contract stores exactly one, "+
			"so the indexer's copy is duplicated and cannot be trusted",
			len(out.Rows), channel, epoch)
	}
	return out.Rows[0], nil
}

func (h *Hasura) Pages(channel string, epoch int64) ([]SharesRow, error) {
	var out struct {
		Rows []SharesRow `json:"magi_tokenomics_shares_events"`
	}
	if err := h.query(pagesQuery, map[string]any{"channel": channel, "epoch": epoch}, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}
