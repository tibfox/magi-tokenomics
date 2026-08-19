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
	// Contract scopes every query to ONE distributor.
	//
	// magi-mongo-indexer adds indexer_contract_id to every table it creates, but the
	// queries below filtered on (channel, epoch) alone. One Hasura serving two
	// deployments therefore merged their share books — and the collision is the
	// LIKELY case, not a contrived one: channel names are conventional ("content",
	// "lp") and every deployment numbers its epochs from 0, so two tenants collide on
	// their first epoch. BuildBook would then reconcile pages from one contract
	// against another's root and fail, denying proofs to both.
	//
	// Empty means unscoped, which is correct for a single-tenant indexer and is what
	// existing deployments have; set it whenever an indexer serves more than one.
	Contract string
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

// The queries come in two shapes: scoped to a distributor, and not. A single-tenant
// indexer has no indexer_contract_id worth filtering on and existing deployments were
// built without it, so an unset Contract keeps the original behaviour exactly.
const rootQuery = `query Root($channel: String!, $epoch: numeric!) {
  magi_tokenomics_root_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch}}) {
    channel epoch root total_shares accounts
  }
}`

const rootQueryScoped = `query Root($channel: String!, $epoch: numeric!, $contract: String!) {
  magi_tokenomics_root_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch},
                                      indexer_contract_id: {_eq: $contract}}) {
    channel epoch root total_shares accounts
  }
}`

const pagesQuery = `query Pages($channel: String!, $epoch: numeric!) {
  magi_tokenomics_shares_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch}},
                                order_by: {page: asc}) {
    channel epoch page entries page_total submitted
  }
}`

const pagesQueryScoped = `query Pages($channel: String!, $epoch: numeric!, $contract: String!) {
  magi_tokenomics_shares_events(where: {channel: {_eq: $channel}, epoch: {_eq: $epoch},
                                        indexer_contract_id: {_eq: $contract}},
                                order_by: {page: asc}) {
    channel epoch page entries page_total submitted
  }
}`

// scope picks the query shape and completes the variables for it.
func (h *Hasura) scope(plain, scoped string, vars map[string]any) (string, map[string]any) {
	if h.Contract == "" {
		return plain, vars
	}
	vars["contract"] = h.Contract
	return scoped, vars
}

func (h *Hasura) Root(channel string, epoch int64) (RootRow, error) {
	var out struct {
		Rows []RootRow `json:"magi_tokenomics_root_events"`
	}
	q, vars := h.scope(rootQuery, rootQueryScoped, map[string]any{"channel": channel, "epoch": epoch})
	if err := h.query(q, vars, &out); err != nil {
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
	q, vars := h.scope(pagesQuery, pagesQueryScoped, map[string]any{"channel": channel, "epoch": epoch})
	if err := h.query(q, vars, &out); err != nil {
		return nil, err
	}
	return out.Rows, nil
}
