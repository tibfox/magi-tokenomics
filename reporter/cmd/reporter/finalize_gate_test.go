package main

import (
	"errors"
	"strings"
	"testing"

	"magi_token/reporter/submit"
)

// fakeState serves distributor state without a chain.
type fakeState struct {
	kv   map[string]string
	err  error
	hits int
}

func (f *fakeState) StateGet(_ string, keys []string) (map[string]string, error) {
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := f.kv[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// NOTE ON THE KEYS BELOW. They carry the CHANNEL — funded|content|41, not
// funded|41 — because that is what the distributor writes now that one contract
// serves several channels. An earlier version of this file used the channel-less
// form, which matched a reporter that read the channel-less form, so the tests
// agreed with the code and both were wrong: every key addressed nothing on a real
// chain, and "not funded / no pages applied" is the answer an absent key gives.
func gateApp(t *testing.T, kv map[string]string, err error) (*app, *fakeState) {
	t.Helper()
	c, cerr := LoadConfig(writeCfg(t, ExampleConfig))
	if cerr != nil {
		t.Fatal(cerr)
	}
	// no waiting in tests; one look at the chain is enough
	c.Submit.ConfirmTries = 1
	c.Submit.ConfirmIntervalSec = 1
	fs := &fakeState{kv: kv, err: err}
	return &app{cfg: c, state: fs}, fs
}

func gatePlan(epoch string, pages int) submit.Plan {
	pl := submit.Plan{Epoch: epoch}
	pl.Calls = append(pl.Calls, submit.Call{Action: "pullFunding", Payload: `{"epoch":"` + epoch + `"}`})
	for i := 0; i < pages; i++ {
		pl.Calls = append(pl.Calls, submit.Call{
			Action:  "submitShares",
			Payload: `{"epoch":"` + epoch + `","page":"` + itoa(i) + `","entries":"hive:a:1"}`,
		})
	}
	pl.Calls = append(pl.Calls, submit.Call{Action: "finalizeEpoch", Payload: `{"epoch":"` + epoch + `"}`})
	return pl
}

func itoa(i int) string { return string(rune('0' + i)) }

// The headline case: pages accepted by Hive but REVERTED on L2. Finalizing here pays
// the whole epoch to whoever landed, and the missing accounts can never be added.
func TestGate_RefusesFinalizeWhenPagesRevertedAndWereNotSentThisRun(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|41":   "5000",
		"status|content|41":   "",
		"ch_rMode|content": "0",
		"ssdone|content|41|4": "1", // only the last page applied
	}, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 5), map[string]bool{}, true)
	if proceed {
		t.Fatal("must NOT finalize an epoch with reverted pages")
	}
	if err == nil {
		t.Fatal("a revert that was not sent in this run must be a hard error, not a quiet skip")
	}
	for _, want := range []string{"NOT applied", "permanently", "0 1 2 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}
}

// All pages confirmed and funded: proceed.
func TestGate_ProceedsWhenEveryPageIsConfirmed(t *testing.T) {
	kv := map[string]string{"funded|content|41": "5000", "status|content|41": "", "ch_rMode|content": "0"}
	for i := 0; i < 5; i++ {
		kv["ssdone|content|41|"+itoa(i)] = "1"
	}
	a, _ := gateApp(t, kv, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 5), map[string]bool{}, true)
	if err != nil || !proceed {
		t.Fatalf("a fully applied epoch must finalize: proceed=%v err=%v", proceed, err)
	}
}

// A page broadcast moments ago may simply be landing. Defer quietly, exit 0, resume
// next run — do not alarm, and do not finalize.
func TestGate_DefersQuietlyForPagesSentThisRun(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|41": "5000", "status|content|41": "", "ch_rMode|content": "0",
	}, nil)
	sent := map[string]bool{"submitShares/0": true}
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 2), sent, true)
	if proceed {
		t.Fatal("must not finalize while our own pages are unconfirmed")
	}
	if err != nil {
		t.Fatalf("a page we just sent is not a failure — it must defer, got: %v", err)
	}
}

// In Attest mode an unapplied page is the NORMAL state for every attester but the
// last. Erroring there would fire on every healthy run and be ignored.
func TestGate_AttestModeDefersInsteadOfErroring(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|41": "5000", "status|content|41": "", "ch_rMode|content": "2",
	}, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 3), map[string]bool{}, true)
	if proceed {
		t.Fatal("must not finalize below the attestation threshold")
	}
	if err != nil {
		t.Fatalf("Attest mode must defer, not error: %v", err)
	}
}

// The gate exists for the case where we cannot see what happened. An unreadable chain
// must never be treated as confirmation.
func TestGate_ReadErrorIsNotConfirmation(t *testing.T) {
	a, _ := gateApp(t, nil, errors.New("gql: connection refused"))
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 2), map[string]bool{}, true)
	if proceed {
		t.Fatal("an unreadable chain must not authorise finalize")
	}
	if err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Fatalf("want a hard confirm error, got: %v", err)
	}
}

// Someone else finalized or cancelled it: stop cleanly, do not send a second finalize.
func TestGate_StopsIfEpochAlreadyClosed(t *testing.T) {
	a, _ := gateApp(t, map[string]string{
		"funded|content|41": "5000", "status|content|41": "cancelled", "ch_rMode|content": "0",
	}, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 2), map[string]bool{}, true)
	if proceed || err != nil {
		t.Fatalf("an already-closed epoch must stop cleanly: proceed=%v err=%v", proceed, err)
	}
}

// funded|ep is zeroed by cancelEpoch, so it must be read as "not funded" rather than
// letting the gate pass on a cancelled epoch.
func TestGate_ZeroFundedIsNotFunded(t *testing.T) {
	kv := map[string]string{"funded|content|41": "0", "status|content|41": "", "ch_rMode|content": "0"}
	for i := 0; i < 2; i++ {
		kv["ssdone|content|41|"+itoa(i)] = "1"
	}
	a, _ := gateApp(t, kv, nil)
	proceed, _ := a.confirmBeforeFinalize(gatePlan("41", 2), map[string]bool{}, true)
	if proceed {
		t.Fatal("funded|<ch>|<ep> == 0 must not satisfy the gate")
	}
}

// A dry run must not be blocked by a gate it cannot evaluate, and must not read as
// evidence that the epoch was verified.
func TestGate_DryRunIsNotEvaluated(t *testing.T) {
	a, fs := gateApp(t, map[string]string{}, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 3), map[string]bool{}, false)
	if !proceed || err != nil {
		t.Fatalf("dry run must pass through: proceed=%v err=%v", proceed, err)
	}
	if fs.hits != 0 {
		t.Fatalf("dry run must not query the chain, made %d calls", fs.hits)
	}
}

// An epoch with no earners must be refused BEFORE funding is pulled into it.
func TestGate_ZeroPagePlanIsRefusedBeforeAnyBroadcast(t *testing.T) {
	pl := gatePlan("41", 0)
	if !planHasFinalize(pl) {
		t.Fatal("fixture should contain finalizeEpoch")
	}
	if countSubmitShares(pl) != 0 {
		t.Fatal("fixture should contain no share pages")
	}
}

// C3 and C5 are the same code deployed twice, so every other cross-check passes for
// either. Without the role label a swapped distributor id reports Hive posts into the
// LP contract (or liquidity into the content one) and then finalizes it.
func TestVerifyChainConfig_RoleMismatchIsReported(t *testing.T) {
	base := map[string]string{
		"cfg_genesis": "0", "cfg_epochLen": "28800", "cfg_funder": "vsc1...C2",
	}
	withRole := func(role string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		if role != "" {
			m["cfg_role"] = role
		}
		return m
	}
	newApp := func(kind string, kv map[string]string) *app {
		c, err := LoadConfig(writeCfg(t, ExampleConfig))
		if err != nil {
			t.Fatal(err)
		}
		c.Source.Kind = kind
		c.Epoch.Genesis, c.Epoch.Len = 0, 28800
		c.Contracts.Funder = "vsc1...C2"
		return &app{cfg: c, state: &fakeState{kv: kv}}
	}

	// the mistake this exists to catch
	probs := newApp(SourceLP, withRole("content")).verifyChainConfig()
	if len(probs) == 0 || !strings.Contains(strings.Join(probs, " "), "ROLE MISMATCH") {
		t.Fatalf("an lp reporter pointed at a content distributor must be flagged, got %v", probs)
	}

	// agreement is silent
	if probs := newApp(SourceLP, withRole("lp")).verifyChainConfig(); len(probs) != 0 {
		t.Fatalf("a matching role must not be reported: %v", probs)
	}
	if probs := newApp(SourceContent, withRole("content")).verifyChainConfig(); len(probs) != 0 {
		t.Fatalf("a matching role must not be reported: %v", probs)
	}

	// unset stays legal — the label is optional and existing deployments have none
	if probs := newApp(SourceContent, withRole("")).verifyChainConfig(); len(probs) != 0 {
		t.Fatalf("an unset role is not a problem: %v", probs)
	}
}

// An exhausted pool is a DIFFERENT diagnosis from missing pages, and conflating them
// sends the operator hunting for a page problem that does not exist. With the pool
// empty, distributeEpoch SUCCEEDS while distributing nothing, pullFunding has nothing
// to claim, and the pages still apply — so everything looks fine except that no money
// arrived.
func TestGate_UnfundedIsReportedAsFundingNotPages(t *testing.T) {
	kv := map[string]string{"funded|content|41": "0", "status|content|41": "", "ch_rMode|content": "0"}
	for i := 0; i < 3; i++ {
		kv["ssdone|content|41|"+itoa(i)] = "1" // every page applied
	}
	a, _ := gateApp(t, kv, nil)
	proceed, err := a.confirmBeforeFinalize(gatePlan("41", 3), map[string]bool{}, true)
	if proceed {
		t.Fatal("an unfunded epoch must not be finalized")
	}
	if err == nil {
		t.Fatal("want a diagnosis, got a silent skip")
	}
	if !strings.Contains(err.Error(), "NOT FUNDED") {
		t.Fatalf("the error must name FUNDING as the problem, got: %v", err)
	}
	if strings.Contains(err.Error(), "share pages are NOT applied") {
		t.Fatalf("must not blame the pages when every page applied: %v", err)
	}
}

// A run handles one epoch, so an epoch that falls below the lookback window is never
// selected again and nothing else ever mentions it — its funding just sits there.
func TestStrandedBelowWindow_FindsAbandonedEpochs(t *testing.T) {
	kv := map[string]string{
		"status|content|0": "finalized",
		"status|content|1": "", // stranded
		"status|content|2": "cancelled",
		"status|content|3": "", // stranded
	}
	a, _ := gateApp(t, kv, nil)
	got := a.strandedBelowWindow(4)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("want epochs [1 3] reported as stranded, got %v", got)
	}
	// nothing below epoch 0 to strand
	if s := a.strandedBelowWindow(0); len(s) != 0 {
		t.Fatalf("window floor 0 cannot strand anything, got %v", s)
	}
}

// The window must be configurable: a hardcoded one makes a backlog longer than the
// window permanently unworkable, since a run only ever handles one epoch.
func TestWindowFloor_IsConfigurable(t *testing.T) {
	a, _ := gateApp(t, nil, nil)
	if f := a.windowFloor(100); f != 81 { // default 20
		t.Fatalf("default floor = %d, want 81", f)
	}
	a.cfg.Epoch.Lookback = 50
	if f := a.windowFloor(100); f != 51 {
		t.Fatalf("floor with lookback 50 = %d, want 51", f)
	}
	// and it must never underflow
	if f := a.windowFloor(3); f != 0 {
		t.Fatalf("floor below the window start = %d, want 0", f)
	}
}
