package submit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"magi_token/reporter/sharecore"
)

func pages() []sharecore.Page {
	return []sharecore.Page{
		{Index: 0, Entries: "hive:a:10,hive:b:20", Count: 2},
		{Index: 1, Entries: "hive:c:30", Count: 1},
	}
}

// finalizeEpoch MUST be the last call: the distributor rejects shares once an
// epoch is finalized, so finalizing early would strand the remaining pages.
func TestBuildPlan_FinalizeComesLast(t *testing.T) {
	pl := BuildPlan("vsc1abc", "content", "7", pages(), 90000, true)
	if len(pl.Calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(pl.Calls))
	}
	if pl.Calls[0].Action != "submitShares" || pl.Calls[1].Action != "submitShares" {
		t.Fatal("share pages must come first")
	}
	if pl.Calls[2].Action != "finalizeEpoch" {
		t.Fatalf("last call must be finalizeEpoch, got %s", pl.Calls[2].Action)
	}
	// payload shape must match what the contract parses, incl. canonical page ids
	if !strings.Contains(pl.Calls[0].Payload, `"epoch":"7"`) ||
		!strings.Contains(pl.Calls[0].Payload, `"page":"0"`) ||
		!strings.Contains(pl.Calls[0].Payload, `"entries":"hive:a:10,hive:b:20"`) {
		t.Fatalf("unexpected payload: %s", pl.Calls[0].Payload)
	}
}

func TestBuildPlan_CanSkipFinalize(t *testing.T) {
	pl := BuildPlan("vsc1abc", "content", "7", pages(), 90000, false)
	for _, c := range pl.Calls {
		if c.Action == "finalizeEpoch" {
			t.Fatal("finalize should be omitted")
		}
	}
}

// A restart must not re-push completed work.
func TestProgress_ResumeSkipsCompletedCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.json")

	pr, err := LoadProgress(path)
	if err != nil {
		t.Fatal(err)
	}
	pl := BuildPlan("vsc1abc", "content", "7", pages(), 90000, true)

	if got := len(Remaining(pl, pr)); got != 3 {
		t.Fatalf("fresh progress: want 3 remaining, got %d", got)
	}
	// simulate page 0 landing, then a crash
	if err := pr.MarkDone("7", "submitShares", 0); err != nil {
		t.Fatal(err)
	}

	// fresh process re-reads the file
	pr2, err := LoadProgress(path)
	if err != nil {
		t.Fatal(err)
	}
	rem := Remaining(pl, pr2)
	if len(rem) != 2 {
		t.Fatalf("after resume: want 2 remaining, got %d", len(rem))
	}
	if strings.Contains(rem[0].Payload, `"page":"0"`) {
		t.Fatal("page 0 must not be re-pushed after a restart")
	}

	// finish everything
	_ = pr2.MarkDone("7", "submitShares", 1)
	_ = pr2.MarkDone("7", "finalizeEpoch", -1)
	pr3, _ := LoadProgress(path)
	if got := len(Remaining(pl, pr3)); got != 0 {
		t.Fatalf("all done: want 0 remaining, got %d", got)
	}
	// a different epoch is unaffected
	pl8 := BuildPlan("vsc1abc", "content", "8", pages(), 90000, true)
	if got := len(Remaining(pl8, pr3)); got != 3 {
		t.Fatalf("epoch 8 should be untouched, got %d remaining", got)
	}
}

func TestProgress_WritesAtomicallyAndDeterministically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.json")
	pr, _ := LoadProgress(path)
	for i := 0; i < 5; i++ {
		if err := pr.MarkDone("3", "submitShares", i); err != nil {
			t.Fatal(err)
		}
	}
	b1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// rewriting the same set must produce identical bytes (sorted keys)
	pr2, _ := LoadProgress(path)
	if err := pr2.MarkDone("3", "submitShares", 4); err != nil { // already present
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path)
	if string(b1) != string(b2) {
		t.Fatalf("progress file not deterministic:\n%s\n---\n%s", b1, b2)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file should have been renamed away")
	}
}

func TestLoadProgress_CorruptFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProgress(path); err == nil {
		t.Fatal("a corrupt progress file must be reported, not silently ignored")
	}
}
