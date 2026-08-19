package itest_test

import (
	"regexp"
	"strings"
	"testing"
)

// An attested payload must not be read from chain state that another entrypoint can
// move while the vote is open.
//
// This has now been the same bug three times:
//
//   - finalizeEpoch bound to totalShares|, which submitShares moves. Two honest
//     reporters attesting at different page-completion points produced different
//     payloads, each burned their single vote in a different tally, and the epoch
//     became permanently unfinalizable.
//   - the policy digest was written by pullFunding and read by submitRoot, so on the
//     reporter's own call order the check never fired at all.
//   - sweepUnallocated bound to unalloc|, which cancelEpoch ADDS to. A guardian
//     voting before a cancellation and one voting after split the same way.
//
// The mechanism is always identical. auth's tally is per payload HASH while
// anti-equivocation allows one vote per (action, authority) — so if the payload can
// change between two honest votes, both votes are spent in different buckets and the
// threshold becomes unreachable. Nothing recovers it: the votes cannot be withdrawn.
//
// THE SAFE SHAPES, both of which appear in the contracts today:
//
//   - a CONSTANT payload derived only from the action itself
//     (finalizeEpoch: "fin:"+ch+":"+ep)
//   - a CALLER-SUPPLIED payload, identical for honest callers by construction
//     (submitShares: entries; submitRoot: root+":"+totalShares; sweepUnallocated:
//     the DECLARED amount)
//
// And one shape that looks unsafe but is not: cancelTokenOp's payload embeds
// qseq|<opKey>, a chain value — but that same value is also in the ACTION KEY, so a
// change starts a NEW round rather than splitting an existing one. That is the
// distinction this guard encodes: a chain read is only dangerous when it can move
// underneath a single action key.

var attestChainRead = regexp.MustCompile(`\b(getBig|getStr|getU|present|StateGetObject)\s*\(`)

// authorizeCalls returns the (actionKey, payload) argument text of every
// auth.Authorize call in a contract source.
func authorizeCalls(src string) [][2]string {
	var out [][2]string
	for _, idx := range regexp.MustCompile(`auth\.Authorize\s*\(`).FindAllStringIndex(src, -1) {
		depth, start := 0, idx[1]
		i := start
		for ; i < len(src); i++ {
			switch src[i] {
			case '(':
				depth++
			case ')':
				if depth == 0 {
					goto done
				}
				depth--
			}
		}
	done:
		args := splitTopLevelArgs(src[start:i])
		// Authorize(cfg, statePrefix, actionKey, payload, caller, requiredAuths)
		if len(args) >= 4 {
			out = append(out, [2]string{strings.TrimSpace(args[2]), strings.TrimSpace(args[3])})
		}
	}
	return out
}

// splitTopLevelArgs splits a Go argument list on commas that are not nested inside
// parens, brackets or string literals.
func splitTopLevelArgs(s string) []string {
	var args []string
	depth, inStr := 0, byte(0)
	last := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr != 0 {
			if c == '\\' {
				i++
			} else if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '`':
			inStr = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, s[last:i])
				last = i + 1
			}
		}
	}
	args = append(args, s[last:])
	return args
}

func TestAttestDrift_NoAttestedPayloadReadsMutableChainState(t *testing.T) {
	srcs := contractSourcesByName(t)
	checked := 0
	for name, src := range srcs {
		for _, call := range authorizeCalls(src) {
			actionKey, payload := call[0], call[1]
			checked++
			if !attestChainRead.MatchString(payload) {
				continue // constant or caller-supplied: safe
			}
			// A chain read is tolerable ONLY if the same read is in the action key,
			// so a change starts a new round instead of splitting the current one.
			if attestChainRead.MatchString(actionKey) {
				continue
			}
			t.Errorf("%s: auth.Authorize attests over chain state that is not part of its "+
				"action key.\n  actionKey: %s\n  payload:   %s\n"+
				"If another entrypoint can move that value while the vote is open, two honest "+
				"authorities attest different payloads, each burns its one vote per action in a "+
				"different tally, and the threshold becomes permanently unreachable. Attest over "+
				"a constant, over a caller-DECLARED value checked at commit, or put the value in "+
				"the action key so a change starts a new round.", name, actionKey, payload)
		}
	}
	if checked == 0 {
		t.Fatal("no auth.Authorize calls found — the scan is not reading the contracts, so " +
			"this guard proves nothing")
	}
	t.Logf("%d attestation sites checked across %d contracts", checked, len(srcs))
}
