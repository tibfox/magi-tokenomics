package hivesrc

import (
	"fmt"
	"strings"
	"time"
)

// HiveTimeLayout is the timestamp format Hive returns (UTC, no zone suffix).
const HiveTimeLayout = "2006-01-02T15:04:05"

// ParseHiveTime parses a Hive timestamp, tolerating a trailing zone if a node
// ever adds one.
func ParseHiveTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(HiveTimeLayout, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unrecognised hive timestamp %q", s)
	}
	return t.UTC(), nil
}

// HeadBlock returns the current L1 head block number.
//
// This doubles as the VSC height: a VSC contract's `block.height` IS the Hive
// block height that anchored the transaction, so epoch arithmetic can be done
// entirely against L1 without a second source of truth.
func HeadBlock(tr Transport) (uint64, error) {
	var props struct {
		HeadBlockNumber uint64 `json:"head_block_number"`
	}
	if err := tr.Call("condenser_api.get_dynamic_global_properties", []any{}, &props); err != nil {
		return 0, err
	}
	if props.HeadBlockNumber == 0 {
		return 0, fmt.Errorf("hive: head_block_number came back 0")
	}
	return props.HeadBlockNumber, nil
}

// BlockTime returns the timestamp of a block.
//
// Epoch boundaries are BLOCK heights but posts are timestamped, so one of the two
// has to be converted. We convert heights to times by asking the chain rather than
// multiplying by 3s: block production slips, and an estimate would put different
// reporters on slightly different windows — which in Attest mode means their pages
// never match and the epoch never reaches threshold.
func BlockTime(tr Transport, height uint64) (time.Time, error) {
	var hdr struct {
		Header *struct {
			Timestamp string `json:"timestamp"`
		} `json:"header"`
	}
	if err := tr.Call("block_api.get_block_header",
		map[string]any{"block_num": height}, &hdr); err != nil {
		return time.Time{}, err
	}
	if hdr.Header == nil || hdr.Header.Timestamp == "" {
		return time.Time{}, fmt.Errorf("hive: no header for block %d (not produced yet?)", height)
	}
	return ParseHiveTime(hdr.Header.Timestamp)
}

// Window is one epoch's block range and the wall-clock range it maps to.
type Window struct {
	Epoch      uint64
	StartBlock uint64
	EndBlock   uint64
	StartTime  time.Time
	EndTime    time.Time
}

// EpochWindow computes epoch ep's block range from the distributor's genesis and
// epochLen, and resolves both boundaries to timestamps.
//
// The range is [genesis+ep*len, genesis+(ep+1)*len-1] — the same closed interval
// the contracts use (C7 computes hEnd as g+(ep+1)*el-1).
func EpochWindow(tr Transport, genesis, epochLen, ep uint64) (Window, error) {
	if epochLen == 0 {
		return Window{}, fmt.Errorf("epochLen must be > 0")
	}
	start, end, ok := EpochBounds(genesis, epochLen, ep)
	if !ok {
		return Window{}, fmt.Errorf("epoch %d is out of range for genesis %d and epochLen %d "+
			"(its block range overflows)", ep, genesis, epochLen)
	}
	w := Window{
		Epoch:      ep,
		StartBlock: start,
		EndBlock:   end,
	}
	var err error
	if w.StartTime, err = BlockTime(tr, w.StartBlock); err != nil {
		return w, fmt.Errorf("epoch %d start block %d: %w", ep, w.StartBlock, err)
	}
	if w.EndTime, err = BlockTime(tr, w.EndBlock); err != nil {
		return w, fmt.Errorf("epoch %d end block %d: %w", ep, w.EndBlock, err)
	}
	return w, nil
}

// LatestClosedEpoch returns the newest epoch that has fully elapsed at `head`.
//
// Reporting on the epoch still in progress would submit a partial report and then
// finalize it, permanently locking out the rest of that epoch's activity, so the
// default target is always the previous one.
func LatestClosedEpoch(genesis, epochLen, head uint64) (uint64, error) {
	if epochLen == 0 {
		return 0, fmt.Errorf("epochLen must be > 0")
	}
	if head < genesis+epochLen {
		return 0, fmt.Errorf("no epoch has closed yet: head %d, first epoch ends at %d",
			head, genesis+epochLen-1)
	}
	cur := (head - genesis) / epochLen
	if cur == 0 {
		return 0, fmt.Errorf("epoch 0 is still in progress")
	}
	return cur - 1, nil
}

// EpochBounds returns the closed block interval of an epoch — the same
// [g+ep*el, g+(ep+1)*el-1] the contracts use — and reports whether it is
// representable.
//
// The overflow check is not academic. `-epoch` is operator input, and
// g+(ep+1)*el-1 wraps for large ep: with g=100 and el=28800, epoch 640511947003808
// yields start=118884, end=147683. Both look like ordinary block numbers, so a
// closed-epoch test of the form `head < end` passes trivially and the reporter
// would score a REAL past block range and submit it under a nonsense epoch index —
// scoring those blocks twice, once legitimately and once not.
func EpochBounds(genesis, epochLen, ep uint64) (start, end uint64, ok bool) {
	if epochLen == 0 {
		return 0, 0, false
	}
	next := ep + 1
	if next < ep {
		return 0, 0, false // ep is max uint64
	}
	span := next * epochLen
	if span/epochLen != next {
		return 0, 0, false // multiplication wrapped
	}
	end = genesis + span
	if end < genesis {
		return 0, 0, false // addition wrapped
	}
	start = genesis + ep*epochLen
	return start, end - 1, true
}
