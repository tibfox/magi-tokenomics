// Package broadcast turns planned calls into signed Hive custom_json operations.
//
// A VSC contract call is an L1 custom_json with id `vsc.call` and body
//
//	{"net_id":..., "contract_id":..., "action":..., "payload":{...}, "rc_limit":N}
//
// (see go-vsc-node state_engine.go, `cj.Id == "vsc.call"` → TxVscCallContract).
//
// AUTHORITY: the framework's auth package calls RequireActive, which rejects a
// caller that appears only in required_posting_auths — the VSC runtime derives
// msg.caller from RequiredPostingAuths[0] when no active auth is present, so
// accepting posting keys would let a posting key satisfy the reporter role. This
// package therefore always signs with ACTIVE authority and never populates
// required_posting_auths.
package broadcast

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/vsc-eco/hivego"

	"magi_token/reporter/submit"
)

// Broadcaster sends one planned call and returns the L1 transaction id.
type Broadcaster interface {
	Send(c submit.Call) (string, error)
}

// callBody is the vsc.call custom_json payload.
type callBody struct {
	NetID      string          `json:"net_id"`
	ContractID string          `json:"contract_id"`
	Action     string          `json:"action"`
	Payload    json.RawMessage `json:"payload"`
	RcLimit    uint            `json:"rc_limit"`
}

// Body renders the custom_json for a call. Exported so `--dry-run` prints the
// exact bytes that would be broadcast rather than an approximation of them.
func Body(netID string, c submit.Call) (string, error) {
	if !json.Valid([]byte(c.Payload)) {
		return "", fmt.Errorf("broadcast: payload for %s is not valid json: %s", c.Action, c.Payload)
	}
	if c.RcLimit <= 0 {
		return "", fmt.Errorf("broadcast: rc_limit must be positive for %s", c.Action)
	}
	b, err := json.Marshal(callBody{
		NetID:      netID,
		ContractID: c.ContractID,
		Action:     c.Action,
		Payload:    json.RawMessage(c.Payload),
		RcLimit:    uint(c.RcLimit),
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// HiveBroadcaster signs and broadcasts with an active key.
type HiveBroadcaster struct {
	Node    *hivego.HiveRpcNode
	NetID   string
	Account string // Hive account name WITHOUT the "hive:" prefix
	wif     string
}

// NewHiveBroadcaster reads the active WIF from an ENVIRONMENT VARIABLE, never a
// config field — a config file holding a live active key tends to end up in a
// backup or a git repo. The env var name is configurable so an operator can wire
// it to whatever secret manager they already run.
func NewHiveBroadcaster(apiAddrs []string, netID, account, wifEnv string) (*HiveBroadcaster, error) {
	if len(apiAddrs) == 0 {
		return nil, fmt.Errorf("broadcast: no hive api endpoint configured")
	}
	if netID == "" {
		return nil, fmt.Errorf("broadcast: net_id is required (e.g. vsc-mainnet / vsc-testnet)")
	}
	acct := strings.TrimPrefix(account, "hive:")
	if acct == "" {
		return nil, fmt.Errorf("broadcast: reporter account is required")
	}
	if wifEnv == "" {
		wifEnv = "REPORTER_ACTIVE_WIF"
	}
	wif := strings.TrimSpace(os.Getenv(wifEnv))
	if wif == "" {
		return nil, fmt.Errorf("broadcast: %s is empty — export the reporter's ACTIVE key there", wifEnv)
	}
	return &HiveBroadcaster{
		Node:    hivego.NewHiveRpc(apiAddrs),
		NetID:   netID,
		Account: acct,
		wif:     wif,
	}, nil
}

func (h *HiveBroadcaster) Send(c submit.Call) (string, error) {
	body, err := Body(h.NetID, c)
	if err != nil {
		return "", err
	}
	// Active auth only; required_posting_auths deliberately EMPTY (see above) — but
	// empty must be `[]string{}`, NEVER nil.
	//
	// hivego drops this straight into CustomJsonOperation, and a nil slice marshals to
	// JSON `null`. Hive's fc deserialiser then rejects the whole transaction with
	// "Bad Cast: Invalid cast from null_type to Array", so EVERY broadcast failed at
	// the node. It went unnoticed because no test ever ran this path against a real
	// Hive node: the devnet suites hand their payloads to the test harness, which
	// builds the operation itself with []string{}.
	return h.Node.BroadcastJson([]string{h.Account}, []string{}, "vsc.call", body, &h.wif)
}

// DryRun records what would have been sent and sends nothing.
type DryRun struct {
	NetID string
	Sent  []string
	Out   *os.File // optional; defaults to stdout
}

func (d *DryRun) Send(c submit.Call) (string, error) {
	body, err := Body(d.NetID, c)
	if err != nil {
		return "", err
	}
	d.Sent = append(d.Sent, body)
	w := d.Out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, "  [dry-run] custom_json vsc.call %s\n            %s\n", c.Action, body)
	// A fake id, clearly marked: a caller that mistakes this for a real txid and
	// records progress from it would skip a call that never landed.
	return "dry-run-not-broadcast", nil
}
