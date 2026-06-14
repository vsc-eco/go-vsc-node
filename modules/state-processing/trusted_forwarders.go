package state_engine

import (
	"strings"

	common_types "vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	contract_session "vsc-node/modules/contract/session"
)

// trustedForwardersActiveKey is the state key the governance-trusted-
// forwarders contract uses for its active list. Stable on-chain
// protocol surface; changing this requires a contract + magi release
// in lockstep. Mirrors
// governance-trusted-forwarders-contract/contract/constants.ActiveListKey.
const trustedForwardersActiveKey = "active"

// trustedForwardersEntryDelim must match the contract's
// constants.EntryDelim. Pipe is the protocol-canonical delimiter.
const trustedForwardersEntryDelim = "|"

// resolveTrustedForwarders computes the per-tx trusted-forwarders allow-
// list seen by the execution context for a single tx execution. Three
// inputs are folded:
//
//  1. sysconfig.TrustedForwarders — legacy per-witness file (the only
//     source pre-activation). Stays authoritative for backwards-compat
//     and for emergency-ADD scenarios where the governance contract
//     can't be reached.
//
//  2. The governance contract's "active" state key — when the consensus
//     version is at-or-above
//     consensusversion.TrustedForwardersFromContractVersion AND
//     sysconfig.TrustedForwardersGovernanceContractId() is non-empty.
//     Read directly via callSession.GetStateStore(<governance-id>).Get
//     (no contract call — just a state read), so the per-tx cost is one
//     additional state lookup regardless of active-list size.
//
//  3. sysconfig.RevokedForwarders — subtract-only kill-switch. Entries
//     here are removed from the union of (1) and (2). Lets an operator
//     locally veto a forwarder during a compromise without
//     coordinating with the rest of the witness fleet.
//
// Pre-activation (consensus version below
// TrustedForwardersFromContractVersion) the function returns sysconfig.
// TrustedForwarders exactly — byte-identical to the previous
// implementation so binaries can roll forward independent of the
// consensus version flip.
//
// Output is always deduped (sysconfig + governance lists can overlap
// during the migration window; magi treats duplicates as harmless
// since IsTrustedForwarder() does linear scan, but downstream
// observability is cleaner with a unique list).
func resolveTrustedForwarders(
	se common_types.StateEngine,
	callSession *contract_session.CallSession,
	blockHeight uint64,
) []string {
	sc := se.SystemConfig()

	// Pre-activation: byte-identical to the legacy path. Single point
	// of truth at this gate so the rest of the function never runs
	// pre-activation.
	if !se.ActiveConsensusVersion(blockHeight).MeetsConsensusMin(
		consensusversion.TrustedForwardersFromContractVersion,
	) {
		return sc.TrustedForwarders()
	}

	// Activation reached. Pull the governance contract's active list
	// (one StateGetObject) if a contract id is configured.
	var fromContract []string
	if govId := sc.TrustedForwardersGovernanceContractId(); govId != "" {
		fromContract = readGovernanceActiveList(callSession, govId)
	}

	return mergeTrustedForwarders(
		sc.TrustedForwarders(),
		fromContract,
		sc.RevokedForwarders(),
	)
}

// mergeTrustedForwarders is the pure side of resolveTrustedForwarders:
// given sysconfig, governance, and revoke lists, returns the effective
// allow-list. Extracted as a separate function so the union+revoke
// semantics can be unit-tested without a real CallSession + state
// engine + DB. resolveTrustedForwarders's only contribution on top is
// the consensus-version gate + the live state read.
//
// Semantics:
//   - Union (sysconfigList, contractList) — deduplicated. Either side
//     can ADD an entry to the effective list.
//   - Subtract revokedList. Strictly removes; never adds. An entry in
//     revokedList that isn't in the union is a no-op (silent — local
//     kill-switch should never error just because a forwarder hasn't
//     been added yet).
//
// Allocation: O(union size); revokedList is small enough that
// indexing it into a set is a one-off win even at length 1.
func mergeTrustedForwarders(sysconfigList, contractList, revokedList []string) []string {
	if len(sysconfigList)+len(contractList) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(sysconfigList)+len(contractList))
	union := make([]string, 0, len(sysconfigList)+len(contractList))
	for _, e := range sysconfigList {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		union = append(union, e)
	}
	for _, e := range contractList {
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		union = append(union, e)
	}

	if len(revokedList) == 0 {
		return union
	}
	revokedSet := make(map[string]struct{}, len(revokedList))
	for _, r := range revokedList {
		revokedSet[r] = struct{}{}
	}
	out := union[:0] // safe to alias — never grows
	for _, e := range union {
		if _, banned := revokedSet[e]; banned {
			continue
		}
		out = append(out, e)
	}
	return out
}

// readGovernanceActiveList does the single state-key read against the
// governance contract. Empty / missing key → empty list (safe-default).
// Splits on the contract's documented delim; rejects empty fragments
// that would otherwise round-trip as the empty string match.
func readGovernanceActiveList(
	callSession *contract_session.CallSession,
	govContractId string,
) []string {
	ss := callSession.GetStateStore(govContractId)
	if ss == nil {
		return nil
	}
	raw := ss.Get(trustedForwardersActiveKey)
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), trustedForwardersEntryDelim)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
