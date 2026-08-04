// Package submit turns computed pages into the on-chain calls C3/C5 expect, and
// keeps durable progress so a restart cannot re-push work.
//
// The contract is the ultimate backstop (it enforces one applied page per
// (epoch,page) and one finalize per epoch), but a reporter that blindly re-pushed
// would waste RC and, in Attest mode, burn its vote on a stale payload. So we
// record progress before/after each call.
package submit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"magi_token/reporter/sharecore"
)

// Call is one contract invocation the reporter wants to make. Building these is
// separated from broadcasting them so `--dry-run` can show exactly what would go
// on-chain, and so tests need no keys or network.
type Call struct {
	ContractID string `json:"contract_id"`
	Action     string `json:"action"`
	Payload    string `json:"payload"`
	RcLimit    int    `json:"rc_limit"`
	Note       string `json:"note"`
}

// Plan is the full ordered set of calls for one epoch.
type Plan struct {
	Epoch string `json:"epoch"`
	Calls []Call `json:"calls"`
}

// BuildPlan produces submitShares pages followed by finalizeEpoch.
//
// Ordering matters on-chain: the distributor refuses to finalize an epoch that
// has no shares, and refuses shares once finalized — so finalize MUST come last.
// It also refuses to finalize an unfunded epoch, so a keeper must have pulled
// funding first (that is the keeper's job, not the reporter's).
// `channel` names the distributor's reward channel these shares belong to. One
// distributor now serves several channels, each with its own share book and its own
// reporter authority, so every call has to say which one it is writing.
func BuildPlan(contractID, channel, epoch string, pages []sharecore.Page, rcLimit int, finalize bool) Plan {
	p := Plan{Epoch: epoch}
	for _, pg := range pages {
		p.Calls = append(p.Calls, Call{
			ContractID: contractID,
			Action:     "submitShares",
			Payload: fmt.Sprintf(`{"channel":"%s","epoch":"%s","page":"%d","entries":"%s"}`,
				channel, epoch, pg.Index, pg.Entries),
			RcLimit: rcLimit,
			Note:    fmt.Sprintf("page %d (%d entries)", pg.Index, pg.Count),
		})
	}
	if finalize {
		p.Calls = append(p.Calls, Call{
			ContractID: contractID,
			Action:     "finalizeEpoch",
			Payload:    fmt.Sprintf(`{"channel":"%s","epoch":"%s"}`, channel, epoch),
			RcLimit:    rcLimit,
			Note:       "freeze epoch + open challenge window",
		})
	}
	return p
}

// Progress is the durable record of what has already been pushed.
type Progress struct {
	// Done maps "epoch/action/page" -> true
	Done map[string]bool `json:"done"`
	path string
}

func progressKey(epoch, action string, page int) string {
	if action == "submitShares" {
		return fmt.Sprintf("%s/submitShares/%d", epoch, page)
	}
	return fmt.Sprintf("%s/%s", epoch, action)
}

// LoadProgress reads (or creates) the progress file.
func LoadProgress(path string) (*Progress, error) {
	p := &Progress{Done: map[string]bool{}, path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("progress file %s is corrupt: %w", path, err)
	}
	if p.Done == nil {
		p.Done = map[string]bool{}
	}
	p.path = path
	return p, nil
}

func (p *Progress) IsDone(epoch, action string, page int) bool {
	return p.Done[progressKey(epoch, action, page)]
}

// MarkDone records a completed call and flushes to disk immediately — losing the
// process between the broadcast and the flush must not cause a re-push.
func (p *Progress) MarkDone(epoch, action string, page int) error {
	p.Done[progressKey(epoch, action, page)] = true
	return p.flush()
}

func (p *Progress) flush() error {
	if p.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	// deterministic file content, so diffing two reporters' progress is meaningful
	keys := make([]string, 0, len(p.Done))
	for k := range p.Done {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := map[string]bool{}
	for _, k := range keys {
		ordered[k] = true
	}
	b, err := json.MarshalIndent(struct {
		Done map[string]bool `json:"done"`
	}{ordered}, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.path) // atomic
}

// PageOf recovers a call's page index, or -1 for calls that are not paged.
//
// Both the skip check and the progress write must derive the page the SAME way:
// if they disagreed, a call would be skipped under one key and recorded under
// another, and the epoch would look permanently unfinished.
func PageOf(c Call) int {
	if c.Action != "submitShares" {
		return -1
	}
	var q struct {
		Page string `json:"page"`
	}
	if err := json.Unmarshal([]byte(c.Payload), &q); err != nil {
		return -1
	}
	n, err := strconv.Atoi(q.Page)
	if err != nil {
		return -1
	}
	return n
}

// Remaining filters a plan down to calls not yet done.
func Remaining(pl Plan, pr *Progress) []Call {
	out := []Call{}
	for _, c := range pl.Calls {
		if pr != nil && pr.IsDone(pl.Epoch, c.Action, PageOf(c)) {
			continue
		}
		out = append(out, c)
	}
	return out
}
