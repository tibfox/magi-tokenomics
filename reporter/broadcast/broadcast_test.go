package broadcast

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"magi_token/reporter/submit"
)

func call() submit.Call {
	return submit.Call{
		ContractID: "vsc1BnMAaeUzhzVcfKMDG5vphthhymk6irjLNq",
		Action:     "submitShares",
		Payload:    `{"epoch":"7","page":"0","entries":"hive:a:10"}`,
		RcLimit:    60000,
	}
}

// The body must match go-vsc-node's TxVscCallContract exactly: net_id,
// contract_id, action, payload (as an OBJECT, not a string) and rc_limit.
func TestBody_MatchesTxVscCallContractShape(t *testing.T) {
	body, err := Body("vsc-testnet", call())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		NetID      string          `json:"net_id"`
		ContractID string          `json:"contract_id"`
		Action     string          `json:"action"`
		Payload    json.RawMessage `json:"payload"`
		RcLimit    uint            `json:"rc_limit"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not valid json: %v\n%s", err, body)
	}
	if got.NetID != "vsc-testnet" {
		t.Fatalf("net_id: %q", got.NetID)
	}
	if got.ContractID != call().ContractID {
		t.Fatalf("contract_id: %q", got.ContractID)
	}
	if got.Action != "submitShares" {
		t.Fatalf("action: %q", got.Action)
	}
	if got.RcLimit != 60000 {
		t.Fatalf("rc_limit: %d", got.RcLimit)
	}
	// payload must be an embedded object; a quoted string would arrive at the
	// contract as a JSON string and every field lookup would come back empty.
	if !strings.HasPrefix(strings.TrimSpace(string(got.Payload)), "{") {
		t.Fatalf("payload must be an object, got %s", got.Payload)
	}
	var inner map[string]string
	if err := json.Unmarshal(got.Payload, &inner); err != nil {
		t.Fatalf("payload not an object: %v", err)
	}
	if inner["epoch"] != "7" || inner["page"] != "0" {
		t.Fatalf("payload fields lost: %+v", inner)
	}
}

func TestBody_RejectsInvalidPayload(t *testing.T) {
	c := call()
	c.Payload = `{not json`
	if _, err := Body("vsc-testnet", c); err == nil {
		t.Fatal("an unparseable payload must be caught locally, not broadcast")
	}
}

func TestBody_RejectsZeroRcLimit(t *testing.T) {
	c := call()
	c.RcLimit = 0
	if _, err := Body("vsc-testnet", c); err == nil {
		t.Fatal("rc_limit 0 would make the tx unexecutable; must be rejected")
	}
}

// DryRun must never look like a real send: a caller that recorded progress from
// its return value would skip a call that never landed.
func TestDryRun_SendsNothingAndSaysSo(t *testing.T) {
	devnull, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	d := &DryRun{NetID: "vsc-testnet", Out: devnull}
	txid, err := d.Send(call())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txid, "dry-run") {
		t.Fatalf("dry-run txid must be obviously fake, got %q", txid)
	}
	if len(d.Sent) != 1 {
		t.Fatalf("want 1 recorded body, got %d", len(d.Sent))
	}
	if !json.Valid([]byte(d.Sent[0])) {
		t.Fatal("recorded body should be the real json that would be sent")
	}
}

func TestDryRun_PropagatesBodyErrors(t *testing.T) {
	devnull, _ := os.CreateTemp(t.TempDir(), "out")
	d := &DryRun{NetID: "vsc-testnet", Out: devnull}
	c := call()
	c.Payload = "oops"
	if _, err := d.Send(c); err == nil {
		t.Fatal("dry-run must surface the same validation errors as a real send")
	}
}

// Guard the key-handling contract: no WIF in config, and a missing key is a clear
// error rather than an unsigned broadcast attempt.
func TestNewHiveBroadcaster_RequiresKeyFromEnv(t *testing.T) {
	const envName = "TEST_REPORTER_WIF_ABSENT"
	t.Setenv(envName, "")
	_, err := NewHiveBroadcaster([]string{"https://api.hive.blog"}, "vsc-testnet", "hive:rep", envName)
	if err == nil || !strings.Contains(err.Error(), envName) {
		t.Fatalf("an empty key env var must name itself in the error, got %v", err)
	}

	t.Setenv(envName, "5Kdummy")
	b, err := NewHiveBroadcaster([]string{"https://api.hive.blog"}, "vsc-testnet", "hive:rep", envName)
	if err != nil {
		t.Fatal(err)
	}
	// the "hive:" prefix is a VSC address form; the L1 op needs the bare name
	if b.Account != "rep" {
		t.Fatalf("account should be the bare Hive name, got %q", b.Account)
	}
}

func TestNewHiveBroadcaster_ValidatesConfig(t *testing.T) {
	const envName = "TEST_REPORTER_WIF"
	t.Setenv(envName, "5Kdummy")
	for _, tc := range []struct {
		name  string
		addrs []string
		netID string
		acct  string
	}{
		{"no endpoint", nil, "vsc-testnet", "rep"},
		{"no net id", []string{"x"}, "", "rep"},
		{"no account", []string{"x"}, "vsc-testnet", ""},
	} {
		if _, err := NewHiveBroadcaster(tc.addrs, tc.netID, tc.acct, envName); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
	}
}

func TestBroadcasters_SatisfyTheInterface(t *testing.T) {
	var _ Broadcaster = &DryRun{}
	var _ Broadcaster = &HiveBroadcaster{}
}
