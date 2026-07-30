// Package auth provides configurable multi-party authorization for privileged
// contract actions (e.g. the C2 guardian passthrough, the C3/C5 reporter push).
//
// Three modes (see plan §20), all verified feasible on VSC:
//
//   - Single    — one authorized account. (That account may itself be a Hive
//     multisig account at L1; transparent to the contract.)
//   - Cosigned  — M-of-N in ONE tx: passes if msg.required_auths contains >= M
//     of the N authorities. Atomic; needs off-chain co-signing.
//   - Attest    — M-of-N ASYNC: each authority submits independently from its own
//     machine; the action commits only once >= M authorities have
//     submitted a BYTE-IDENTICAL payload for the same actionKey.
//     M==N gives unanimous. Pure in-contract, no signature coordination.
//
// The package is stateless with respect to configuration — each contract stores
// its own auth Config and reconstructs it. Only Attest mode touches contract
// state (namespaced under a caller-supplied prefix).
package auth

import (
	"magi_token/sdk"
	"strconv"
)

type Mode uint8

const (
	ModeSingle   Mode = 0
	ModeCosigned Mode = 1
	ModeAttest   Mode = 2
)

// Config is the authorization policy for one use-site. Immutable after init
// except via a higher-tier governed rotation (see plan D2/R18).
type Config struct {
	Mode        Mode
	Authorities []string // the N authorized accounts (hive:... or contract:...)
	Threshold   int      // M; for Single implicitly 1 (Authorities[0])
}

// Validate rejects an incoherent policy at init time.
func Validate(cfg Config) {
	if len(cfg.Authorities) == 0 {
		sdk.Abort("auth: no authorities")
	}
	for i, a := range cfg.Authorities {
		if a == "" {
			sdk.Abort("auth: empty authority")
		}
		// '|' is the reserved state-key delimiter across the framework.
		for j := 0; j < len(a); j++ {
			if a[j] == '|' {
				sdk.Abort("auth: '|' not allowed in authority")
			}
		}
		// Reject duplicates — a repeated authority would let ONE signer satisfy an
		// M-of-N Cosigned threshold (countPresent counts positions). (MED-1)
		for j := 0; j < i; j++ {
			if cfg.Authorities[j] == a {
				sdk.Abort("auth: duplicate authority")
			}
		}
	}
	switch cfg.Mode {
	case ModeSingle:
		// threshold is implicitly 1 over Authorities[0] — more than one authority
		// means the operator expected M-of-N and mis-set the mode (MED-1).
		if len(cfg.Authorities) > 1 {
			sdk.Abort("auth: single mode takes exactly one authority")
		}
	case ModeCosigned, ModeAttest:
		if cfg.Threshold < 1 || cfg.Threshold > len(cfg.Authorities) {
			sdk.Abort("auth: threshold must be 1..N")
		}
	default:
		sdk.Abort("auth: unknown mode")
	}
}

// Authorize checks a privileged action.
//   - Single/Cosigned: returns true when authorized, aborts otherwise (the action
//     proceeds in the same tx).
//   - Attest: records this caller's attestation of `payload` and returns true ONLY
//     on the tx that reaches the threshold (idempotent — commits once per actionKey).
//
// statePrefix namespaces this use-site's attest state (e.g. "c3reporter").
// actionKey namespaces one logical action (e.g. "submitShares:5:<pageHash>").
// requiredAuths is env's msg.required_auths (for Cosigned).
func Authorize(cfg Config, statePrefix, actionKey, payload, caller string, requiredAuths []string) bool {
	// CRIT: the VSC runtime derives msg.caller from RequiredPostingAuths[0] when no
	// active auth is present, so a POSTING key alone would otherwise satisfy every
	// privileged role. Demand the caller appear in the ACTIVE auth list for all
	// modes. (VSC itself refuses posting auth for create/update_contract.)
	RequireActive(caller, requiredAuths)
	switch cfg.Mode {
	case ModeSingle:
		if len(cfg.Authorities) == 0 || caller != cfg.Authorities[0] {
			sdk.Abort("auth: not authorized (single)")
		}
		return true
	case ModeCosigned:
		// The caller itself must be an authority. VSC forwards the tx's
		// RequiredAuths verbatim into nested contract calls, so without this a
		// contract invoked by an authority-signed tx could relay in and pass the
		// threshold (confused deputy, CRIT-2).
		if !contains(cfg.Authorities, caller) {
			sdk.Abort("auth: caller not an authority (cosigned)")
		}
		if countPresent(cfg.Authorities, requiredAuths) < cfg.Threshold {
			sdk.Abort("auth: threshold not met (cosigned)")
		}
		return true
	case ModeAttest:
		return attest(cfg, statePrefix, actionKey, payload, caller)
	}
	sdk.Abort("auth: unknown mode")
	return false
}

// attest implements the async M-of-N identical-payload scheme.
func attest(cfg Config, statePrefix, actionKey, payload, caller string) bool {
	if !contains(cfg.Authorities, caller) {
		sdk.Abort("auth: caller not an authority")
	}
	doneKey := statePrefix + "|adone|" + actionKey
	if present(doneKey) {
		// Already committed for this action — treat further pushes as no-ops for
		// the caller, but do not double-commit.
		sdk.Abort("auth: action already committed")
	}
	if len(payload) > 4096 {
		sdk.Abort("auth: attested payload too large")
	}
	payloadHash := Hash(payload)
	// Byte-identity within the hash bucket. FNV is NOT collision-resistant, so a
	// distinct payload could share a hash; bind the bucket to the exact bytes of
	// its first attestation and reject any mismatch (defeats collision injection).
	canonKey := statePrefix + "|acanon|" + actionKey + "|" + payloadHash
	if v := sdk.StateGetObject(canonKey); v != nil && *v != "" {
		if *v != payload {
			sdk.Abort("auth: payload mismatch for this action")
		}
	} else {
		sdk.StateSetObject(canonKey, payload)
	}
	// One vote per authority PER ACTION (not per payload) — otherwise an
	// equivocator could back every candidate payload and turn M-of-N into a
	// front-runnable race (MED-1).
	seenKey := statePrefix + "|aseen|" + actionKey + "|" + caller
	if present(seenKey) {
		sdk.Abort("auth: caller already attested this action")
	}
	sdk.StateSetObject(seenKey, "1")

	tallyKey := statePrefix + "|atally|" + actionKey + "|" + payloadHash
	tally := 1
	if v := sdk.StateGetObject(tallyKey); v != nil && *v != "" {
		if t, err := strconv.Atoi(*v); err == nil {
			tally = t + 1
		}
	}
	sdk.StateSetObject(tallyKey, strconv.Itoa(tally))

	if tally >= cfg.Threshold {
		sdk.StateSetObject(doneKey, "1")
		// GC the working state — the commit flag is the lasting record (MED-2).
		sdk.StateDeleteObject(canonKey)
		sdk.StateDeleteObject(tallyKey)
		return true
	}
	return false
}

// Hash is a deterministic FNV-1a 64-bit hash (no external deps, identical across
// machines) used to compare pushed payloads in Attest mode.
// Hash is a 128-bit digest (two independent FNV-1a lanes with a per-byte rotation
// to break the pure-multiplicative structure). The exact-byte canon compare is the
// real safety; this only needs to make constructing a COLLIDING page-bucket
// infeasible so a rogue can't grief an honest coalition without front-running (MED-2).
func Hash(s string) string {
	var h1 uint64 = 1469598103934665603
	var h2 uint64 = 14695981039346656037 // distinct offset lane
	for i := 0; i < len(s); i++ {
		c := uint64(s[i])
		h1 = (h1 ^ c) * 1099511628211
		h2 = (h2 ^ c) * 1099511628219
		h2 = (h2 << 13) | (h2 >> 51) // rotate: defeats simple algebraic collision
	}
	return strconv.FormatUint(h1, 16) + "-" + strconv.FormatUint(h2, 16)
}

// RequireActive aborts unless `caller` is present in the tx's ACTIVE required_auths.
// Privileged actions must never be reachable with a Hive posting key.
func RequireActive(caller string, requiredAuths []string) {
	if !contains(requiredAuths, caller) {
		sdk.Abort("auth: active authority required (posting key not accepted)")
	}
}

// RequireActiveUnlessContract gates a caller-relative, fund-moving entrypoint on
// ACTIVE authority, exempting contract callers.
//
// The exemption is not a convenience — it is required for correctness. On a nested
// call the runtime sets Caller to "contract:<id>" while forwarding the OUTER
// transaction's required_auths verbatim, so a contract caller can never appear in
// that list and an unconditional RequireActive would abort every nested call. For C1
// that would strand any stake held by a contract permanently: it could stake but
// never unstake.
//
// It fails CLOSED for every other namespace. An earlier draft whitelisted "hive:"
// instead, which silently skips the guard for any namespace nobody thought of; did:
// callers do land in required_auths and pass the strict check.
//
// Use this for entrypoints that move the CALLER'S OWN funds. Do not use it where the
// caller is being checked for membership of an authority set — auth.Authorize already
// calls the strict RequireActive there, and a relay in that position is a genuine
// confused deputy.
func RequireActiveUnlessContract(caller string, requiredAuths []string) {
	const contractPrefix = "contract:"
	if len(caller) >= len(contractPrefix) && caller[:len(contractPrefix)] == contractPrefix {
		return
	}
	RequireActive(caller, requiredAuths)
}

// present reports whether a state key holds a non-empty value. The VSC runtime
// returns a non-nil pointer to "" for a MISSING key, so a bare `!= nil` check
// gives false positives — presence must test the value.
func present(k string) bool {
	v := sdk.StateGetObject(k)
	return v != nil && *v != ""
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

func countPresent(authorities, requiredAuths []string) int {
	n := 0
	for _, a := range authorities {
		if contains(requiredAuths, a) {
			n++
		}
	}
	return n
}
