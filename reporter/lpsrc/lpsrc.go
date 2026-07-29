// Package lpsrc reconstructs per-provider LP positions for one epoch and maps them
// onto sharecore's deterministic output, so C5 can be fed the same way C3 is.
//
// WHY THIS EXISTS AT ALL — the DEX cannot answer the question we need to ask:
//
//	`vsc-eco/dex-contracts` stores LP balances at `lp-{address}` with the pool total
//	at `tlp`, but ONLY as current state; it keeps no height checkpoints. A reward
//	epoch is priced AFTER it ends, so "who held LP during epoch 41" is unanswerable
//	from live state. Paying against live balances would also be trivially gameable:
//	add liquidity just before the snapshot, remove it just after, collect a full
//	epoch. C1 keeps stake checkpoints precisely so C7 does not have this problem.
//
// So we reconstruct history from the indexer's event log instead. `add_liq` and
// `rem_liq` events carry provider, lp_minted/lp_burned and indexer_block_height, so
//
//	LP(provider, H) = SUM(lp_minted where height <= H) - SUM(lp_burned where height <= H)
//
// is exact, and can be evaluated at any past height.
//
// ANTI-FLASH-LIQUIDITY — we credit min(LP(start), LP(end)), mirroring what C7 does
// for stake. Liquidity must have been present at BOTH epoch boundaries to earn, so
// neither adding late nor withdrawing early is rewarded.
//
// ON TRUST — the indexer is operated, not trustless, and this is a deliberate
// tradeoff: reconstructing LP history on-chain would cost gas nobody wants to pay.
// It does NOT add a new trust assumption beyond the reporter itself, because the
// events are derived from on-chain transactions and Hasura is publicly queryable —
// so anyone can recompute this independently and a guardian can still meaningfully
// veto. That is the property that must not be given up: an unverifiable data source
// would make the challenge window theatre.
//
// DETERMINISM — every query is pinned to explicit block heights, never to "now", so
// two machines querying at different moments (or querying different indexers) derive
// identical shares. That is what keeps Attest mode usable for LP.
package lpsrc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"time"

	"magi_token/reporter/sharecore"
)

// Transport performs a GraphQL query. Swappable so tests need no network.
type Transport interface {
	Query(query string, vars map[string]any, out any) error
}

// HTTPTransport talks to a Hasura GraphQL endpoint.
type HTTPTransport struct {
	Endpoint string // e.g. http://indexer:8081/v1/graphql
	Secret   string // x-hasura-admin-secret; empty when the endpoint is public
	Client   *http.Client
}

func (t *HTTPTransport) Query(query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.Secret != "" {
		req.Header.Set("x-hasura-admin-secret", t.Secret)
	}
	cl := t.Client
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("indexer returned %d: %s", resp.StatusCode, truncate(raw))
	}
	// Amounts are Postgres `numeric` (arbitrary precision) and can exceed 2^53.
	// Decoding through float64 would silently corrupt them, so decode numbers as
	// json.Number and parse them as big.Int. This is the same trap the Hive path
	// hit with rshares.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

// Options selects the pool and the epoch boundary heights to price.
type Options struct {
	// Pool is the pool contract id as the indexer records it (indexer_contract_id).
	Pool string
	// Start and End are the epoch's first and last block. Both boundaries are
	// evaluated; a provider earns on the SMALLER of the two positions.
	Start, End uint64
	// PageSize bounds each GraphQL page. 0 uses a sane default.
	PageSize int
}

const defaultPageSize = 1000

// event is one liquidity change: +lp_minted or -lp_burned at a height.
type event struct {
	Provider string
	Delta    *big.Int
	Height   uint64
}

// gqlRow is the wire shape of both event tables; only the amount column differs, so
// each query aliases it to `amount` and one struct serves both.
type gqlRow struct {
	Provider string      `json:"provider"`
	Amount   json.Number `json:"amount"`
	Height   json.Number `json:"indexer_block_height"`
}

// Both tables are read through one parameterised document. Ordering is explicit and
// TOTAL (height, then tx hash) because an offset walk over an unstable order
// silently drops and duplicates rows.
const addQuery = `query($pool:String!,$h:bigint!,$limit:Int!,$offset:Int!){
  rows: dex_pool_add_liq_events(
    where:{indexer_contract_id:{_eq:$pool},indexer_block_height:{_lte:$h}}
    order_by:[{indexer_block_height:asc},{indexer_tx_hash:asc}]
    limit:$limit offset:$offset
  ){ provider amount: lp_minted indexer_block_height }
}`

const remQuery = `query($pool:String!,$h:bigint!,$limit:Int!,$offset:Int!){
  rows: dex_pool_rem_liq_events(
    where:{indexer_contract_id:{_eq:$pool},indexer_block_height:{_lte:$h}}
    order_by:[{indexer_block_height:asc},{indexer_tx_hash:asc}]
    limit:$limit offset:$offset
  ){ provider amount: lp_burned indexer_block_height }
}`

type gqlResp struct {
	Data struct {
		Rows []gqlRow `json:"rows"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// fetchTable pages one event table up to and including height h, applying sign to
// each amount (+1 for mints, -1 for burns).
func fetchTable(t Transport, query, pool string, h uint64, pageSize, sign int) ([]event, error) {
	out := []event{}
	for offset := 0; ; offset += pageSize {
		var resp gqlResp
		vars := map[string]any{"pool": pool, "h": h, "limit": pageSize, "offset": offset}
		if err := t.Query(query, vars, &resp); err != nil {
			return nil, err
		}
		// GraphQL reports failures inside a 200 body, so an unchecked `errors` array
		// reads as "no rows" — i.e. every provider silently earning nothing.
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("indexer query failed: %s", resp.Errors[0].Message)
		}
		for _, r := range resp.Data.Rows {
			amt, ok := new(big.Int).SetString(r.Amount.String(), 10)
			if !ok {
				return nil, fmt.Errorf("provider %q: amount %q is not an integer", r.Provider, r.Amount)
			}
			if amt.Sign() < 0 {
				return nil, fmt.Errorf("provider %q: negative amount %s", r.Provider, amt)
			}
			hv, err := r.Height.Int64()
			if err != nil || hv < 0 {
				return nil, fmt.Errorf("provider %q: bad height %q", r.Provider, r.Height)
			}
			if sign < 0 {
				amt = new(big.Int).Neg(amt)
			}
			out = append(out, event{Provider: r.Provider, Delta: amt, Height: uint64(hv)})
		}
		if len(resp.Data.Rows) < pageSize {
			return out, nil
		}
	}
}

// balanceAt folds the events at or below h into a per-provider balance.
func balanceAt(events []event, h uint64) map[string]*big.Int {
	bal := map[string]*big.Int{}
	for _, e := range events {
		if e.Height > h {
			continue
		}
		cur, ok := bal[e.Provider]
		if !ok {
			cur = new(big.Int)
			bal[e.Provider] = cur
		}
		cur.Add(cur, e.Delta)
	}
	return bal
}

// LPShares reconstructs LP positions at both epoch boundaries and returns the share
// weights for one epoch: min(LP(start), LP(end)) per provider.
//
// Both boundaries come from ONE fetch (everything up to End) rather than two, so the
// two balances cannot disagree about the underlying event set — refetching would
// leave a window where the indexer advances between calls and start/end come from
// different views of history.
func LPShares(t Transport, o Options) (sharecore.Result, error) {
	res := sharecore.Result{Shares: map[string]*big.Int{}, Total: new(big.Int)}
	if o.Pool == "" {
		return res, fmt.Errorf("pool contract id is required")
	}
	if o.End < o.Start {
		return res, fmt.Errorf("epoch end %d is before start %d", o.End, o.Start)
	}
	pageSize := o.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}

	adds, err := fetchTable(t, addQuery, o.Pool, o.End, pageSize, +1)
	if err != nil {
		return res, fmt.Errorf("add_liq events: %w", err)
	}
	rems, err := fetchTable(t, remQuery, o.Pool, o.End, pageSize, -1)
	if err != nil {
		return res, fmt.Errorf("rem_liq events: %w", err)
	}
	all := append(adds, rems...)

	atStart := balanceAt(all, o.Start)
	atEnd := balanceAt(all, o.End)

	// Iterate a sorted union so the walk order is fixed. The arithmetic is
	// order-independent, but a stable walk keeps failures reproducible.
	names := make([]string, 0, len(atEnd))
	seen := map[string]bool{}
	for _, m := range []map[string]*big.Int{atStart, atEnd} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)

	for _, name := range names {
		s, e := atStart[name], atEnd[name]
		if s == nil {
			s = new(big.Int) // no position at the start: joined mid-epoch, earns nothing
		}
		if e == nil {
			e = new(big.Int) // exited before the end: earns nothing
		}
		credit := s
		if e.Cmp(credit) < 0 {
			credit = e
		}
		// A negative balance means burns exceeded mints for this provider, which the
		// DEX cannot produce — treat it as corrupt rather than silently crediting 0,
		// since it would also mean every other balance is suspect.
		if credit.Sign() < 0 {
			return sharecore.Result{Shares: map[string]*big.Int{}, Total: new(big.Int)},
				fmt.Errorf("provider %q has negative LP balance (start=%s end=%s) — indexer data is inconsistent",
					name, s, e)
		}
		if credit.Sign() == 0 {
			continue // sharecore.Canonicalize drops zeroes anyway; skip explicitly
		}
		c := new(big.Int).Set(credit)
		res.Shares[name] = c
		res.Total.Add(res.Total, c)
	}
	return res, nil
}
