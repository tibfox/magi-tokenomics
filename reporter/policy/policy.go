// Package policy turns the settings that decide an epoch's share book into one
// digest, so two reporters cannot silently score the same epoch differently.
//
// sharecore already guarantees that the same INPUT produces the same bytes — see
// TestDeterminism_ShuffledInputSameBytes and the integer-only curve arithmetic in
// curve.go, which exists precisely so no float rounding can make two machines
// differ in the last digit. What nothing guaranteed is that two reporters feed the
// same input at all.
//
// verifyChainConfig checked three things against the chain: genesis, epochLen and
// role. Everything else that changes the numbers — which tags count, the dust
// cutoff, the reward curves, the vote-mana budget, the app tax — was local config
// checked against nothing. Two HONEST reporters, one with min_share_bps 5 and one
// with 10, produce different books forever and neither can tell. In Attest mode
// that is not a wrong payout, it is a deadlock: the tally is per payload hash and
// anti-equivocation gives each authority one vote per action, so both burn their
// vote in a different bucket and the page can never reach threshold.
//
// The digest is anchored on-chain per epoch (policy|<ch>|<ep>, snapshotted when the
// epoch is funded) and every reporter compares its own before it computes anything.
// A divergent reporter refuses to run and names the field. That leaves exactly one
// way for two reporters to disagree — one of them deliberately running patched
// code — which is the case Attest mode is built to contain, and does.
//
// # What the digest deliberately does NOT cover
//
// Endpoints. Two reporters may use different Hive API nodes or VSC endpoints and
// still agree, because those serve the same chain data; forcing byte-equal URLs
// would reject two honest mirrors.
//
// The indexer URL is the uncomfortable one, and it is excluded for the same reason
// while NOT being as safe. LP shares are rebuilt from the indexer's add_liq/rem_liq
// log, and magi-mongo-indexer's ingestion can legitimately assign the same event
// different heights across two instances (it falls back between a transaction's L1
// anchored height and its state-output height when the transaction_pool lookup
// misses), which lands the event in different epochs. Same indexer, same answer;
// different indexers, no guarantee — and no digest computed here can fix that,
// because the divergence is in the data source rather than in the config. LP
// quorums must share an indexer. That is a real residual gap, and the fix for it
// belongs upstream in the indexer, not here.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Digest returns the canonical digest of the book-affecting settings.
//
// `sections` are the config sub-structs that decide the book — pass them in the
// order the caller declares them; the digest is order-sensitive across sections
// but not within an order-independent list (see canonical below).
//
// Callers should use DigestOf rather than calling this directly.
func Digest(sections ...any) (string, error) {
	parts := make([]json.RawMessage, 0, len(sections))
	for _, s := range sections {
		c, err := canonical(reflect.ValueOf(s))
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(c)
		if err != nil {
			return "", err
		}
		parts = append(parts, b)
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// orderIndependent lists the fields whose ORDER cannot change the resulting book,
// so a digest must not treat a reordering as a different policy.
//
// This is not cosmetic. hivesrc sorts its posts canonically BEFORE truncating to
// the limit, specifically so two Attest machines listing the same tags in a
// different order agree — if the digest then rejected them for that same
// reordering, it would manufacture the exact failure it exists to prevent. A false
// mismatch is as damaging as a missed one, because an operator who hits one
// spurious refusal turns the check off.
//
// Membership is by field NAME, matched at any depth. A field listed here must be a
// slice whose element order is genuinely ignored downstream; adding one that is
// order-SENSITIVE would hide a real divergence, which is why the test asserts each
// name below still exists in the config.
var orderIndependent = map[string]bool{
	"Tags":         true, // hivesrc merges then sorts; a post matching several pays once
	"ExcludedTags": true, // membership test
	"Exclude":      true, // membership test
	"Muted":        true, // membership test
	"Apps":         true, // app_tax designated apps; matched on the part before "/"
}

// canonical rewrites a value into something json.Marshal renders identically for
// any two configs that mean the same thing: order-independent lists sorted, maps
// rendered by sorted key (json.Marshal already does this), and every other value
// left exactly as it is.
//
// Struct fields are walked by reflection rather than listed by hand, so a field
// added to a covered section is included automatically. That is the difference
// between a guarantee that holds and one that quietly stops holding the next time
// someone adds a setting — and TestDigest_EveryFieldChangesTheDigest asserts it.
func canonical(v reflect.Value) (any, error) {
	if !v.IsValid() {
		return nil, nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil, nil
		}
		return canonical(v.Elem())

	case reflect.Struct:
		out := make(map[string]any, v.NumField())
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported: cannot affect a JSON-decoded config
			}
			c, err := canonicalField(f.Name, v.Field(i))
			if err != nil {
				return nil, err
			}
			out[f.Name] = c
		}
		return out, nil

	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			c, err := canonical(v.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil

	case reflect.Map:
		// json.Marshal sorts map keys, so a map is already canonical — but only for
		// string keys, which is all a JSON config can produce.
		out := make(map[string]any, v.Len())
		for _, k := range v.MapKeys() {
			if k.Kind() != reflect.String {
				return nil, fmt.Errorf("policy: map key of kind %s cannot come from a JSON config", k.Kind())
			}
			c, err := canonical(v.MapIndex(k))
			if err != nil {
				return nil, err
			}
			out[k.String()] = c
		}
		return out, nil

	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return nil, fmt.Errorf("policy: %s cannot appear in a config", v.Kind())

	default:
		return v.Interface(), nil
	}
}

func canonicalField(name string, v reflect.Value) (any, error) {
	c, err := canonical(v)
	if err != nil {
		return nil, err
	}
	if !orderIndependent[name] {
		return c, nil
	}
	items, ok := c.([]any)
	if !ok {
		if c == nil {
			return c, nil
		}
		return nil, fmt.Errorf("policy: %q is listed as order-independent but is %T, "+
			"not a list — the entry is wrong and would hide a real divergence", name, c)
	}
	strs := make([]string, len(items))
	for i, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, fmt.Errorf("policy: %q is listed as order-independent but holds %T, "+
				"and only string lists can be safely sorted here", name, it)
		}
		strs[i] = s
	}
	sort.Strings(strs)
	out := make([]any, len(strs))
	for i, s := range strs {
		out[i] = s
	}
	return out, nil
}
