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
	_, err := NewHiveBroadcaster([]string{"https://api.hive.blog"}, "vsc-testnet", "hive:rep", envName, "")
	if err == nil || !strings.Contains(err.Error(), envName) {
		t.Fatalf("an empty key env var must name itself in the error, got %v", err)
	}

	t.Setenv(envName, "5Kdummy")
	b, err := NewHiveBroadcaster([]string{"https://api.hive.blog"}, "vsc-testnet", "hive:rep", envName, "")
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
		if _, err := NewHiveBroadcaster(tc.addrs, tc.netID, tc.acct, envName, ""); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		}
	}
}

func TestBroadcasters_SatisfyTheInterface(t *testing.T) {
	var _ Broadcaster = &DryRun{}
	var _ Broadcaster = &HiveBroadcaster{}
}

// required_posting_auths must be an EMPTY ARRAY, never nil.
//
// hivego passes this field straight into CustomJsonOperation, and a nil slice
// marshals to JSON `null`. Hive's fc deserialiser rejects that with "Bad Cast:
// Invalid cast from null_type to Array" and the transaction never executes — so a nil
// here means the reporter cannot broadcast at all. That is precisely what happened,
// and it survived because every devnet suite broadcasts through the test harness,
// which builds the operation itself.
func TestHiveBroadcaster_PostingAuthsAreEmptyNotNil(t *testing.T) {
	t.Setenv("TEST_WIF", "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n")
	h, err := NewHiveBroadcaster([]string{"http://localhost:1"}, "vsc-devnet", "hive:someone", "TEST_WIF", "")
	if err != nil {
		t.Fatalf("constructing broadcaster: %v", err)
	}
	if h.Account != "someone" {
		t.Fatalf("account = %q, want the hive: prefix stripped", h.Account)
	}

	// Marshalling a nil []string yields "null"; an empty one yields "[]". Pin the
	// distinction directly, because it is invisible in Go and fatal on the wire.
	var nilAuths []string
	b, _ := json.Marshal(nilAuths)
	if string(b) != "null" {
		t.Fatalf("precondition: a nil []string should marshal to null, got %s", b)
	}
	b, _ = json.Marshal([]string{})
	if string(b) != "[]" {
		t.Fatalf("precondition: an empty []string should marshal to [], got %s", b)
	}

	// And the source must not regress to nil.
	src, rerr := os.ReadFile("broadcast.go")
	if rerr != nil {
		t.Fatalf("read broadcast.go: %v", rerr)
	}
	if strings.Contains(string(src), `BroadcastJson([]string{h.Account}, nil,`) {
		t.Fatal("Send passes nil for required_posting_auths — Hive rejects the transaction " +
			"with \"Invalid cast from null_type to Array\" and nothing is ever broadcast")
	}
}

// A VSC network can run on a Hive chain that is not mainnet, and signatures must be
// made over THAT chain's id. Signing against the wrong one does not fail loudly as a
// chain mismatch — the signature recovers to a different key and the node reports
// "missing required active authority", which reads as a permissions problem and sends
// the operator hunting for the wrong thing.
func TestHiveBroadcaster_ChainIDIsConfigurable(t *testing.T) {
	t.Setenv("TEST_WIF", "5JNHfZYKGaomSFvd4NUdQ9qMcEAC43kujbfjueTHpVapX1Kzq2n")
	const custom = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"

	h, err := NewHiveBroadcaster([]string{"http://localhost:1"}, "vsc-devnet", "hive:a", "TEST_WIF", custom)
	if err != nil {
		t.Fatal(err)
	}
	if h.Node.ChainID != custom {
		t.Fatalf("ChainID = %q, want the configured %q", h.Node.ChainID, custom)
	}

	// empty must leave hivego's default alone rather than blanking it
	def, err := NewHiveBroadcaster([]string{"http://localhost:1"}, "vsc-mainnet", "hive:a", "TEST_WIF", "")
	if err != nil {
		t.Fatal(err)
	}
	if def.Node.ChainID != "" {
		t.Fatalf("an unset chain_id must leave hivego's default in place, got %q", def.Node.ChainID)
	}
}
