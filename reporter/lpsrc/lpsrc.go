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
	"strings"
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
	// AllowStale skips the freshness gate. Default (false) is the safe setting: refuse
	// to score an epoch unless the indexer can be PROVEN to have processed past its
	// end. See checkFresh for why that proof is one-sided.
	AllowStale bool
}

const defaultPageSize = 1000

// maxRows bounds one table's walk. Without it the offset loop only terminates when a
// short page arrives, so an indexer that keeps returning full pages — buggy, hostile,
// or simply misinterpreting `offset` — spins forever and grows the process until it
// is killed. A real pool would need over a million liquidity events to reach this.
const maxRows = 1_000_000

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
		if len(out) > maxRows {
			return nil, fmt.Errorf("more than %d rows while paging — the indexer is not honouring "+
				"limit/offset, or the pool has implausibly many events", maxRows)
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
	if !o.AllowStale {
		if err := checkFresh(t, o.Pool, o.End); err != nil {
			return res, err
		}
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
		// The distributor credits only domain-prefixed ledger addresses and SILENTLY
		// SKIPS anything else. So a bare provider name would not be an odd entry — it
		// would mean the whole epoch pays nobody while its funding accumulates, with
		// nothing in the logs. Refuse to build such a report.
		if err := checkProvider(name); err != nil {
			return sharecore.Result{Shares: map[string]*big.Int{}, Total: new(big.Int)}, err
		}
		c := new(big.Int).Set(credit)
		res.Shares[name] = c
		res.Total.Add(res.Total, c)
	}
	return res, nil
}

// checkProvider is the last gate before an untrusted string becomes an irreversible
// on-chain payload, so it is deliberately strict.
//
// The distributor receives shares as "acct:amount" pairs joined by commas, and parses
// them by splitting on commas and then the LAST colon. A provider name containing a
// COMMA therefore injects entries. It is not a cosmetic problem — given the provider
// name `hive:a,hive:attacker:999999999,` the payload becomes
//
//	hive:a,hive:attacker:999999999,:<amount>
//
// which the contract reads as a 999,999,999-share entry for hive:attacker: the whole
// epoch, stolen. A trailing colon is milder but still harmful: `hive:alice:` is
// ledger-prefixed, so it is counted into totalShares and then unclaimable forever,
// diluting everyone else.
//
// Multiple colons are otherwise FINE and must stay allowed — did:key:z6Mk... is a
// legitimate provider, and last-colon splitting parses it correctly.
//
// The DEX validates its own recipient addresses, so this is probably unreachable
// today. It is enforced anyway because the indexer is a trusted-but-not-trustless
// component: if it were ever compromised or replaced, this is the only thing standing
// between it and an arbitrary payout.
func checkProvider(name string) error {
	if !isLedgerAddr(name) {
		return fmt.Errorf("provider %q is not a ledger address (needs a hive:/contract:/did:/system: prefix) — "+
			"the distributor would skip it and the epoch would pay nobody", name)
	}
	if strings.Contains(name, ",") {
		return fmt.Errorf("provider %q contains a comma, which is the on-chain entry separator — "+
			"it would inject extra share entries", name)
	}
	if strings.HasSuffix(name, ":") {
		return fmt.Errorf("provider %q ends in a colon — it would be counted into totalShares "+
			"and then be unclaimable, diluting every other provider", name)
	}
	for _, r := range name {
		// Anything outside this set cannot be a real address on any of the four
		// domains, and control characters or quotes risk mangling the payload.
		if r > 0x7e || r <= 0x20 || r == '"' || r == '\\' {
			return fmt.Errorf("provider %q contains an unsafe character %q", name, r)
		}
	}
	return nil
}

// isLedgerAddr mirrors the distributor's own check (c5-lp/contract: isLedgerAddr).
// Kept deliberately identical: if these ever diverge, the reporter would submit
// entries the contract discards.
func isLedgerAddr(a string) bool {
	for _, p := range []string{"hive:", "contract:", "did:", "system:"} {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// healthQuery reads the indexer's GLOBAL progress: MAX(block_height) across every
// contract it tracks. Kept only for diagnostics — see checkFresh for why it must not
// be used as the freshness proof.
const healthQuery = `query{ indexer_health { latest_block_height } }`

// poolHeightQuery reads how far the indexer has got FOR ONE CONTRACT.
const poolHeightQuery = `query($pool:String!){
  contract_logs_aggregate(where:{contract_address:{_eq:$pool}}){
    aggregate { max { block_height } count }
  }
}`

type healthResp struct {
	Data struct {
		Rows []struct {
			Height json.Number `json:"latest_block_height"`
		} `json:"indexer_health"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type poolHeightResp struct {
	Data struct {
		Agg struct {
			Aggregate struct {
				Max struct {
					Height *json.Number `json:"block_height"`
				} `json:"max"`
				Count int `json:"count"`
			} `json:"aggregate"`
		} `json:"contract_logs_aggregate"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// IndexerHeight returns the highest block the indexer has logged for ANY contract.
func IndexerHeight(t Transport) (uint64, error) {
	var resp healthResp
	if err := t.Query(healthQuery, map[string]any{}, &resp); err != nil {
		return 0, err
	}
	if len(resp.Errors) > 0 {
		return 0, fmt.Errorf("indexer_health query failed: %s", resp.Errors[0].Message)
	}
	if len(resp.Data.Rows) == 0 {
		return 0, fmt.Errorf("indexer_health returned no rows")
	}
	h, err := resp.Data.Rows[0].Height.Int64()
	if err != nil || h < 0 {
		return 0, fmt.Errorf("indexer_health latest_block_height %q is not a height", resp.Data.Rows[0].Height)
	}
	return uint64(h), nil
}

// PoolIndexedHeight returns the highest block the indexer has logged FOR THIS POOL,
// and whether it has any logs for it at all.
func PoolIndexedHeight(t Transport, pool string) (uint64, bool, error) {
	var resp poolHeightResp
	if err := t.Query(poolHeightQuery, map[string]any{"pool": pool}, &resp); err != nil {
		return 0, false, err
	}
	if len(resp.Errors) > 0 {
		return 0, false, fmt.Errorf("contract_logs query failed: %s", resp.Errors[0].Message)
	}
	a := resp.Data.Agg.Aggregate
	if a.Count == 0 || a.Max.Height == nil {
		return 0, false, nil
	}
	h, err := a.Max.Height.Int64()
	if err != nil || h < 0 {
		return 0, false, fmt.Errorf("contract_logs max block_height %q is not a height", *a.Max.Height)
	}
	return uint64(h), true, nil
}

// checkFresh refuses to score an epoch the indexer may not have finished indexing.
//
// THE FAILURE THIS PREVENTS: a lagging indexer does not error, it returns FEWER ROWS.
// Balances come out understated, the report is submitted and finalized, and providers
// are underpaid irreversibly with nothing in any log.
//
// IT MUST BE MEASURED PER POOL. `indexer_health` is MAX(block_height) over ALL of
// contract_logs with no contract filter, but ingestion advances a SEPARATE cursor per
// contract (fetcher/mongo.go lastProcessed[address]), and DEX pools are discovered at
// runtime via a pool_init event rather than configured statically — so a pool starts
// at cursor 0 and backfills while long-tracked contracts already sit at head. On the
// live testnet indexer the global figure is ~5,023,590 while the pool's own logs stop
// at ~2,937,340: two million blocks of false confidence. An earlier version of this
// gate used the global number and would have waved through every epoch in that gap.
//
// WHAT THE EVIDENCE CAN AND CANNOT PROVE. Even scoped to the pool this is the height
// of the last log WRITTEN, not the cursor position. So:
//
//	height >= epochEnd  =>  it has demonstrably logged past our window. PROOF, passes.
//	height <  epochEnd  =>  AMBIGUOUS. Either the indexer is behind on this pool, or
//	                        the pool simply had no activity since — normal on a quiet
//	                        pool, and then our data IS complete.
//
// Nothing exposed distinguishes those, so the gate is one-sided: pass only on proof.
// On a quiet pool that WILL refuse complete epochs, which is what AllowStale is for —
// defaulted off, because an operator wanting throughput should say so rather than
// discover silent underpayment later.
//
// No logs at all for the pool is treated as unproven, not as "nothing happened": an
// undiscovered or not-yet-backfilled pool looks exactly like an empty one.
func checkFresh(t Transport, pool string, end uint64) error {
	h, any, err := PoolIndexedHeight(t, pool)
	if err != nil {
		return fmt.Errorf("cannot verify indexer freshness: %w", err)
	}
	if !any {
		return fmt.Errorf("the indexer holds no logs at all for pool %s, so it cannot be shown to "+
			"have indexed this epoch. A pool that was never discovered looks identical to one "+
			"with no activity. Check indexer.pool, or set indexer.allow_stale if you are certain",
			pool)
	}
	if h >= end {
		return nil
	}
	global, gerr := IndexerHeight(t)
	hint := ""
	if gerr == nil {
		hint = fmt.Sprintf(" (the indexer is at block %d across all contracts, but that says nothing "+
			"about this pool: each contract advances its own cursor, and discovered pools backfill "+
			"from 0)", global)
	}
	return fmt.Errorf("indexer may be behind on pool %s: its newest log for that pool is block %d "+
		"but this epoch ends at %d, so liquidity events in blocks %d..%d may not be indexed yet%s. "+
		"Either it is lagging (scoring now would underpay, irreversibly) or the pool has simply been "+
		"idle (the data is complete). That cannot be told apart from what the indexer exposes. Wait "+
		"for it to catch up, or set indexer.allow_stale if you accept the risk",
		pool, h, end, h+1, end, hint)
}
