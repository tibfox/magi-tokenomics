// Package events emits contract logs in the format magi-mongo-indexer ingests.
//
// WHY THIS EXISTS. Until now the three contracts emitted nothing at all: no
// sdk.Log call on any path. The indexer builds its entire Postgres model from
// contract logs pulled out of MongoDB's `contract_state`, so with no logs there
// was nothing to index and the only way to learn what a deployment had done was
// to read its raw key-value state or replay every transaction against the wasm.
// Meanwhile the framework already CONSUMED the indexer — the reporter's `lp`
// mode reads liquidity history from Hasura — while feeding it nothing back.
//
// THE FORMAT IS NOT OURS TO INVENT. magi_token-contract is the reference, and
// its shape is what the existing magi_token mappings already parse:
//
//	{"type":"transfer","attributes":{"from":"hive:a","to":"hive:b","amount":"100"}}
//
// A `type` discriminator at the top level (the indexer reads it as the log type
// for parse: "json"), and every value under `attributes`. We match it exactly,
// including the one detail that matters most for correctness:
//
// AMOUNTS ARE STRINGS. The token serialises *big.Int as a JSON string, and so do
// we, for every numeric value without exception. Framework amounts are big.Int
// and epoch/height values are uint64; both exceed what a JSON number survives
// intact through a float64-based decoder, which is what the indexer's
// encoding/json pass into map[string]interface{} gives us. The indexer's data
// layer casts the string to the column type declared in the mapping, so
// `numeric` columns work and nothing is silently rounded on the way in. Emitting
// 18446744073709551615 as a bare number would arrive as 18446744073709552000.
//
// Escaping goes through jwriter (the same tinyjson writer the token uses), so a
// key or value carrying a quote or backslash cannot break the envelope. Contract
// inputs are validated upstream for the state-key delimiters, but a log is built
// from values the caller supplied and must defend its own syntax.
//
// LOGS ONLY SURVIVE A SUCCESSFUL TRANSACTION. The indexer stores logs from
// committed transactions only, so an emit on a path that later aborts is not a
// leak — it simply never existed. The corollary is that events cannot be used to
// diagnose a failure, which is precisely why the `skip` events matter: a skipped
// entry is a silent no-op inside a transaction that SUCCEEDS, the one failure
// mode a log can actually capture.
package events

import (
	"magi_token/sdk"
	"math/big"
	"strconv"

	"github.com/CosmWasm/tinyjson/jwriter"
)

// Event is a single log under construction. Build it with New, add attributes,
// then call Emit exactly once.
type Event struct {
	w     jwriter.Writer
	empty bool
}

// New starts an event of the given type. The type is what the indexer matches a
// mapping's log_type against, so it is part of the wire contract: renaming one
// silently stops a table receiving rows.
func New(logType string) *Event {
	e := &Event{empty: true}
	e.w.RawString(`{"type":`)
	e.w.String(logType)
	e.w.RawString(`,"attributes":{`)
	return e
}

func (e *Event) key(k string) {
	if !e.empty {
		e.w.RawByte(',')
	}
	e.empty = false
	e.w.String(k)
	e.w.RawByte(':')
}

// Str adds a string attribute.
func (e *Event) Str(k, v string) *Event {
	e.key(k)
	e.w.String(v)
	return e
}

// Big adds a big.Int attribute as a decimal string. A nil value is written as
// "0" rather than null, matching the token contract, so a numeric column never
// receives a NULL it would have to be queried around.
func (e *Event) Big(k string, v *big.Int) *Event {
	e.key(k)
	if v == nil {
		e.w.String("0")
		return e
	}
	e.w.String(v.String())
	return e
}

// U64 adds a uint64 attribute as a decimal string (heights, epochs, counters).
func (e *Event) U64(k string, v uint64) *Event {
	e.key(k)
	e.w.String(strconv.FormatUint(v, 10))
	return e
}

// Int adds an int attribute as a decimal string (counts, thresholds).
func (e *Event) Int(k string, v int) *Event {
	e.key(k)
	e.w.String(strconv.Itoa(v))
	return e
}

// Bool adds a boolean as the string "true"/"false". The indexer's data layer
// accepts either for a `bool` column, and keeping every value a string means one
// rule for the whole wire format instead of two.
func (e *Event) Bool(k string, v bool) *Event {
	e.key(k)
	if v {
		e.w.String("true")
		return e
	}
	e.w.String("false")
	return e
}

// Emit closes the envelope and writes the log. Call it once per event.
func (e *Event) Emit() {
	e.w.RawString(`}}`)
	sdk.Log(string(e.w.Buffer.BuildBytes()))
}
