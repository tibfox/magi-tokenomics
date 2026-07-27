// Hostile relay contract — NOT part of the framework. Deployed by an ATTACKER in
// tests to prove that calling our contracts *from a contract* grants no power an
// ordinary account lacks. msg.caller becomes "contract:<hostile-id>", which is a
// namespace disjoint from any hive: account and from every configured role.
package main

import (
	"magi_token/sdk"
)

func main() {}

// relay forwards an arbitrary call: {"target","method","payload"}.
// Used to attempt impersonation / privilege escalation from a contract context.
//
//go:wasmexport relay
func Relay(payload *string) *string {
	target := f(payload, "target")
	method := f(payload, "method")
	inner := f(payload, "payload")
	if target == "" || method == "" {
		sdk.Abort("target+method required")
	}
	res := sdk.ContractCall(target, method, inner, nil)
	if res == nil {
		return str(`{"relayed":true,"res":""}`)
	}
	return str(`{"relayed":true,"res":"ok"}`)
}

// reenter calls a target twice in one tx — an attempt to exploit the unguarded
// reentrancy of the runtime (depth <= 20).
//
//go:wasmexport reenter
func Reenter(payload *string) *string {
	target := f(payload, "target")
	method := f(payload, "method")
	inner := f(payload, "payload")
	sdk.ContractCall(target, method, inner, nil)
	sdk.ContractCall(target, method, inner, nil)
	return str(`{"reentered":true}`)
}

func str(s string) *string { return &s }

func f(payload *string, name string) string {
	if payload == nil {
		return ""
	}
	s := *payload
	needle := `"` + name + `"`
	from := 0
	for {
		rel := indexOf(s[from:], needle)
		if rel < 0 {
			return ""
		}
		i := from + rel + len(needle)
		from = i
		k := i
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k >= len(s) || s[k] != ':' {
			continue
		}
		k++
		for k < len(s) && s[k] == ' ' {
			k++
		}
		if k < len(s) && s[k] == '"' {
			k++
			j := k
			depth := 0
			for j < len(s) {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '{' {
					depth++
				}
				if s[j] == '}' {
					depth--
				}
				if s[j] == '"' && depth <= 0 {
					break
				}
				j++
			}
			return unescape(s[k:j])
		}
		j := k
		for j < len(s) && s[j] != ',' && s[j] != '}' && s[j] != ' ' {
			j++
		}
		return s[k:j]
	}
}

func unescape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		out = append(out, s[i])
	}
	return string(out)
}

func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		k := 0
		for k < m && s[i+k] == sub[k] {
			k++
		}
		if k == m {
			return i
		}
	}
	return -1
}
