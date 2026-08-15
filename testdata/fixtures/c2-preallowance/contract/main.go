// A stand-in for a C2 deployed BEFORE the allowance model.
//
// It exists for one test: proving that the current C2, installed over such a
// deployment by vsc.update_contract, aborts loudly instead of silently starving.
//
// The only thing that matters about a pre-allowance instance is the STATE it
// leaves: initialised (so assertInit passes) and carrying an epoch schedule, but
// with no cfg_source, because that key did not exist when it was written. init is
// one-shot in the real contract, so a genuine pre-allowance deployment can never
// acquire one afterwards — which is exactly why the new code has to say so rather
// than pull from the empty address and report success forever.
//
// This is NOT a copy of the old contract and does not try to be. It reproduces the
// state shape and nothing else; the code swapped in over it is the real one.
package main

import (
	"magi_token/sdk"
)

func set(k, v string) { sdk.StateSetObject(k, v) }

func f(payload *string, name string) string {
	if payload == nil {
		return ""
	}
	s := *payload
	needle := `"` + name + `":"`
	i := indexOf(s, needle)
	if i < 0 {
		return ""
	}
	rest := s[i+len(needle):]
	j := indexOf(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// init writes the pre-allowance state: a schedule, a token, buckets — and
// deliberately NO cfg_source.
//
//go:wasmexport init
func Init(payload *string) *string {
	set("cfg_token", f(payload, "token"))
	set("cfg_genesis", f(payload, "genesis"))
	set("cfg_epochLen", f(payload, "epochLen"))
	set("cfg_baseAnnual", f(payload, "baseAnnual"))
	set("cfg_blocksPerYear", f(payload, "blocksPerYear"))
	set("cfg_maxSupply", "1000000000")
	set("init", "1")
	r := `{"preallowance":true}`
	return &r
}

// distributeEpoch reports success while doing nothing — the silent-starvation
// behaviour the new code replaces. Present so the test can show the contract was
// live and answering before the swap.
//
//go:wasmexport distributeEpoch
func DistributeEpoch(payload *string) *string {
	r := `{"distributed":"0","preallowance":true}`
	return &r
}
