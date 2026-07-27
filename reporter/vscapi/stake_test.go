package vscapi

import (
	"fmt"
	"math/big"
	"strconv"
	"testing"
)

// fakeState is an in-memory stand-in for a contract's KV state.
type fakeState struct {
	kv    map[string]string
	reads int
}

func (f *fakeState) StateGetOne(contractID, key string) (string, bool, error) {
	f.reads++
	v, ok := f.kv[key]
	if !ok || v == "" {
		return "", false, nil
	}
	return v, true, nil
}

// buildHist writes an account history exactly the way C1's appendHist does.
func buildHist(acct string, entries [][2]uint64) *fakeState {
	f := &fakeState{kv: map[string]string{}}
	for i, e := range entries {
		f.kv[fmt.Sprintf("hist|%s|%d", acct, i)] = fmt.Sprintf("%d:%d", e[0], e[1])
	}
	f.kv["hist_n|"+acct] = strconv.Itoa(len(entries))
	return f
}

// bruteForce is the SPEC: rightmost entry with height <= target, else 0.
func bruteForce(entries [][2]uint64, target uint64) uint64 {
	var out uint64
	for _, e := range entries {
		if e[0] <= target {
			out = e[1]
		}
	}
	return out
}

// The binary search must agree with the linear spec for EVERY height across many
// history shapes. A single disagreement is a silent mispayment, so this is the
// most important test in the package.
func TestStakeAtHeight_MatchesLinearSpecExhaustively(t *testing.T) {
	shapes := [][][2]uint64{
		{},                                       // never staked
		{{10, 500}},                              // single entry
		{{10, 500}, {20, 0}},                     // staked then fully unstaked
		{{10, 100}, {10, 200}},                   // two writes at the SAME height
		{{5, 1}, {6, 2}, {7, 3}, {8, 4}, {9, 5}}, // dense
		{{1, 9}, {50, 8}, {51, 7}, {900, 6}},     // sparse + clustered
		{{2, 1}, {2, 2}, {2, 3}, {100, 4}},       // repeated height at the head
		{{7, 42}, {7, 42}, {7, 42}, {7, 42}},     // all identical
	}
	for si, shape := range shapes {
		f := buildHist("hive:a", shape)
		s := NewStakeSource(f, "vsc1c1")
		for h := uint64(0); h <= 1000; h++ {
			got, err := s.StakeAtHeight("hive:a", h)
			if err != nil {
				t.Fatalf("shape %d h=%d: %v", si, h, err)
			}
			want := bruteForce(shape, h)
			if got.Uint64() != want {
				t.Fatalf("shape %d h=%d: got %s want %d (history %v)", si, h, got, want, shape)
			}
		}
	}
}

// Long history: confirm it really is a binary search (log2 reads), not a scan.
// A linear walk would be ~1024 state reads per lookup, which at one HTTP round
// trip each would make the reporter unusable on a real chain.
func TestStakeAtHeight_IsLogarithmic(t *testing.T) {
	const n = 1024
	var shape [][2]uint64
	for i := 0; i < n; i++ {
		shape = append(shape, [2]uint64{uint64(i) * 10, uint64(i)})
	}
	f := buildHist("hive:a", shape)
	s := NewStakeSource(f, "vsc1c1")
	if _, err := s.StakeAtHeight("hive:a", 5000); err != nil {
		t.Fatal(err)
	}
	// 1 read for hist_n + ~log2(1024)=10 probes + 1 final fetch
	if f.reads > 20 {
		t.Fatalf("expected ~log2(1024) reads, got %d — search degraded to a scan", f.reads)
	}
	t.Logf("reads for n=1024: %d", f.reads)
}

func TestStakeAtHeight_CachesRepeatVoters(t *testing.T) {
	f := buildHist("hive:a", [][2]uint64{{10, 100}, {20, 200}})
	s := NewStakeSource(f, "vsc1c1")
	if _, err := s.StakeAtHeight("hive:a", 25); err != nil {
		t.Fatal(err)
	}
	after := f.reads
	for i := 0; i < 10; i++ {
		if _, err := s.StakeAtHeight("hive:a", 25); err != nil {
			t.Fatal(err)
		}
	}
	if f.reads != after {
		t.Fatalf("repeat lookups should be cached: %d -> %d reads", after, f.reads)
	}
	// a DIFFERENT height must not be served from the cache
	v, _ := s.StakeAtHeight("hive:a", 15)
	if v.Uint64() != 100 {
		t.Fatalf("cache keyed only by account would return 200; got %s", v)
	}
}

func TestStakeAtHeight_UnknownAccountIsZeroNotError(t *testing.T) {
	f := buildHist("hive:a", [][2]uint64{{10, 100}})
	s := NewStakeSource(f, "vsc1c1")
	v, err := s.StakeAtHeight("hive:nobody", 999)
	if err != nil {
		t.Fatalf("an account that never staked is normal, not an error: %v", err)
	}
	if v.Sign() != 0 {
		t.Fatalf("want 0, got %s", v)
	}
}

func TestTotalAtHeight_ReadsGlobalCheckpoints(t *testing.T) {
	f := &fakeState{kv: map[string]string{
		"ckpt|0": "10:1000", "ckpt|1": "20:1500", "ckpt|2": "30:900",
		"ckpt_n": "3",
	}}
	s := NewStakeSource(f, "vsc1c1")
	for _, tc := range []struct{ h, want uint64 }{{5, 0}, {10, 1000}, {19, 1000}, {20, 1500}, {35, 900}} {
		got, err := s.TotalAtHeight(tc.h)
		if err != nil {
			t.Fatal(err)
		}
		if got.Uint64() != tc.want {
			t.Fatalf("h=%d: got %s want %d", tc.h, got, tc.want)
		}
	}
}

func TestStakeAtHeight_MalformedStateIsAnError(t *testing.T) {
	f := &fakeState{kv: map[string]string{"hist_n|hive:a": "abc"}}
	s := NewStakeSource(f, "vsc1c1")
	if _, err := s.StakeAtHeight("hive:a", 1); err == nil {
		t.Fatal("a non-numeric hist_n must be reported, not silently treated as 0")
	}

	f2 := buildHist("hive:a", [][2]uint64{{10, 1}})
	f2.kv["hist|hive:a|0"] = "notaheight:5"
	s2 := NewStakeSource(f2, "vsc1c1")
	if _, err := s2.StakeAtHeight("hive:a", 20); err == nil {
		t.Fatal("a malformed history entry must be reported")
	}
}

// Guard the type contract: StakeSource must satisfy hivesrc.StakeReader. Asserted
// structurally here to avoid an import cycle in the test.
func TestStakeSource_SatisfiesStakeReaderShape(t *testing.T) {
	var _ interface {
		StakeAtHeight(string, uint64) (*big.Int, error)
	} = NewStakeSource(&fakeState{kv: map[string]string{}}, "vsc1c1")
}
