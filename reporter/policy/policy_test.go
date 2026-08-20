package policy

import (
	"reflect"
	"testing"
)

type inner struct {
	Bps         int      `json:"bps"`
	Apps        []string `json:"apps"`
	Beneficiary string   `json:"beneficiary"`
}

type sample struct {
	Tags         []string `json:"tags"`
	ExcludedTags []string `json:"excluded_tags"`
	Limit        int      `json:"limit"`
	Weight       string   `json:"weight"`
	Flag         bool     `json:"flag"`
	AppTax       inner    `json:"app_tax"`
}

func mustDigest(t *testing.T, v ...any) string {
	t.Helper()
	d, err := Digest(v...)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	return d
}

// Reordering a list whose order cannot change the book must NOT change the digest.
//
// hivesrc sorts posts canonically before truncating precisely so two reporters
// listing the same tags in a different order agree. If the digest rejected them for
// that reordering it would manufacture the failure it exists to prevent — and an
// operator who hits one spurious refusal turns the check off, which costs more than
// the check ever bought.
func TestDigest_OrderIndependentListsDoNotChangeIt(t *testing.T) {
	a := sample{
		Tags:         []string{"magi", "hive", "vsc"},
		ExcludedTags: []string{"nsfw", "spam"},
		AppTax:       inner{Apps: []string{"peakd", "ecency"}},
	}
	b := sample{
		Tags:         []string{"vsc", "magi", "hive"},
		ExcludedTags: []string{"spam", "nsfw"},
		AppTax:       inner{Apps: []string{"ecency", "peakd"}},
	}
	if mustDigest(t, a) != mustDigest(t, b) {
		t.Fatal("reordering an order-independent list changed the digest: honest reporters " +
			"listing the same tags differently would be refused")
	}
}

// Adding a tag IS a policy change and must change the digest — the test above
// must not have been satisfied by ignoring the field altogether.
func TestDigest_ListMembershipStillChangesIt(t *testing.T) {
	a := sample{Tags: []string{"magi", "hive"}}
	b := sample{Tags: []string{"magi", "hive", "vsc"}}
	if mustDigest(t, a) == mustDigest(t, b) {
		t.Fatal("adding a tag did not change the digest — the sort is swallowing membership")
	}
}

// THE test. Every field of every covered section must move the digest.
//
// A digest that silently omits a field is worse than no digest: it reports
// agreement between two reporters that will score the epoch differently. Hand-
// listing the fields would make that the default failure the next time somebody
// adds a setting, so canonical() walks by reflection and this walks the same
// structs to prove nothing is missed.
func TestDigest_EveryFieldChangesTheDigest(t *testing.T) {
	base := sample{
		Tags:         []string{"magi"},
		ExcludedTags: []string{"nsfw"},
		Limit:        1000,
		Weight:       "hive_rshares",
		Flag:         false,
		AppTax:       inner{Bps: 500, Apps: []string{"peakd"}, Beneficiary: "hive:treasury"},
	}
	want := mustDigest(t, base)

	// Collect every leaf field as an index path, then mutate each one in a FRESH
	// copy. Mutating in place and restoring would let one missed restore mask every
	// later field.
	var paths [][]int
	var collect func(prefix []int, t reflect.Type)
	collect = func(prefix []int, rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.PkgPath != "" {
				continue
			}
			p := append(append([]int{}, prefix...), i)
			if f.Type.Kind() == reflect.Struct {
				collect(p, f.Type)
				continue
			}
			paths = append(paths, p)
		}
	}
	collect(nil, reflect.TypeOf(base))

	if len(paths) == 0 {
		t.Fatal("no fields were found — the walk proves nothing")
	}
	for _, p := range paths {
		cp := base // copy; sample holds slices, so mutate() must not append in place
		v := reflect.ValueOf(&cp).Elem()
		name := ""
		for _, i := range p {
			name += "." + v.Type().Field(i).Name
			v = v.Field(i)
		}
		mutate(t, v)
		if got := mustDigest(t, cp); got == want {
			t.Errorf("%s does not affect the digest: two reporters differing only in "+
				"this field would pass the check and then score the epoch differently", name[1:])
		}
	}
	t.Logf("%d fields exercised, every one moves the digest", len(paths))
}

// mutate changes a value to something a config could plausibly hold. It only has to
// produce a DIFFERENT value, not a meaningful one.
func mutate(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Slice:
		// Build a NEW slice rather than appending: the caller mutates a shallow copy,
		// so appending into shared spare capacity could scribble on the pristine base
		// and make later fields compare against a corrupted baseline.
		grown := reflect.MakeSlice(v.Type(), 0, v.Len()+1)
		grown = reflect.AppendSlice(grown, v)
		grown = reflect.Append(grown, reflect.ValueOf("added-by-the-test"))
		v.Set(grown)
	default:
		t.Fatalf("the test cannot mutate a %s — extend mutate() rather than skipping it, "+
			"or a field of this kind silently escapes the guarantee", v.Kind())
	}
}
