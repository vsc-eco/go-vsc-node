package state_engine

import (
	"strings"

	common_types "vsc-node/modules/common/common_types"
	contract_session "vsc-node/modules/contract/session"
)

// trustedForwarderStateKey is the dash-mapping-contract state key that
// names the trusted dash-forwarder-contract id. Set once via the
// mapping's setForwarderContractId action and locked thereafter ("pause
// + clear required to change"). To change in production: update the
// dash-mapping-contract via vsc.update_contract, which goes through the
// network-baked ContractUpdateTimelockBlocks window.
//
// MUST match
// utxo-mapping/dash-mapping-contract/contract/constants.ForwarderContractIdStateKey.
const trustedForwarderStateKey = "forwarder"

// resolveTrustedForwarders returns the trusted-forwarders allow-list
// magi's execution context honours for call_as gating, for a single tx
// execution. The list has at most one entry: the dash-forwarder-
// contract id named in the dash-mapping-contract's "forwarder" state
// key, prefixed with "contract:" so it matches the
// `"contract:" + ctx.env.ContractId` check inside IsTrustedForwarder().
//
// Resolution:
//
//  1. sysconfig.DashMappingContractId() — the dash-mapping-contract id
//     for this network. Empty → no Dash IS-login feature on this
//     network → empty trusted-forwarders → call_as aborts. Safe default.
//
//  2. callSession.GetStateStore(mappingId).Get("forwarder") — one
//     StateGetObject. Empty / missing → mapping was deployed but the
//     admin hasn't yet wired the forwarder via setForwarderContractId
//     → call_as still aborts. Safe default.
//
//  3. Trim + prefix with "contract:" so the magi-side
//     IsTrustedForwarder() string-compares directly.
//
// To change the trusted forwarder on a live network the operator-of-
// record runs vsc.update_contract on the dash-mapping-contract — the
// existing contract-update governance (single admin key, 48h timelock,
// community-visible diff during the window) is also the trusted-
// forwarder governance. No separate admin surface.
//
// Operators retain local emergency response by setting sysconfig.
// DashMappingContractId to "" on their node — that node refuses every
// call_as via the mapping, falling out of consensus until the operator
// re-enables it. Coarser than the previous RevokedForwarders kill-
// switch but matches the simpler one-source design.
func resolveTrustedForwarders(
	se common_types.StateEngine,
	callSession *contract_session.CallSession,
	blockHeight uint64,
) []string {
	mappingId := se.SystemConfig().DashMappingContractId()
	if mappingId == "" {
		return nil
	}
	ss := callSession.GetStateStore(mappingId)
	if ss == nil {
		return nil
	}
	raw := ss.Get(trustedForwarderStateKey)
	if len(raw) == 0 {
		return nil
	}
	forwarderId := strings.TrimSpace(string(raw))
	if forwarderId == "" {
		return nil
	}
	return []string{"contract:" + forwarderId}
}
