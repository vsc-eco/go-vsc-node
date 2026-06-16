// Package islock_instruction holds the canonical instruction-string
// builders for the Dash IS-login feature. Both the IS service
// (cmd/is-service) and the dash-mapping-contract (utxo-mapping repo)
// must agree on the wire format — having a single source-of-truth
// package importable from both sides closes the audit-R3-09 / TC2-10
// drift surface that hand-mirrored test closures couldn't.
//
// The grammar matches dash-mapping-contract/contract/mapping/forwarder_integration.go's
// ParseInstructionV2 — drift here fails the cross-repo parity test
// (tests/current/parity_cross_repo_test.go, build tag cross_repo).
package islock_instruction

import "strconv"

// BuildAuthInstruction constructs the IS-login instruction for op=auth.
//
//	op=auth;sid=<sid>
//
// The deposit address is derived from sha256(instruction); the contract
// re-parses it via ParseInstructionV2.
func BuildAuthInstruction(sid string) string {
	return "op=auth;sid=" + sid
}

// BuildCallInstruction constructs the value-bearing or value-less
// contract-call instruction:
//
//	op=call;contract=<id>;method=<m>;args=<base64>;sid=<sid>[;amount=<n>]
//
// amountDuffs is in duffs (1e-8 DASH); pass 0 for value-less calls.
func BuildCallInstruction(contractId, method, argsB64, sid string, amountDuffs int64) string {
	out := "op=call;contract=" + contractId + ";method=" + method + ";args=" + argsB64 + ";sid=" + sid
	if amountDuffs > 0 {
		out += ";amount=" + strconv.FormatInt(amountDuffs, 10)
	}
	return out
}
