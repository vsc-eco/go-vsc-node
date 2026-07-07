package tss

import (
	"context"
	"strconv"
	"strings"
	"time"

	"vsc-node/lib/btcclient"
	"vsc-node/lib/datalayer"

	"github.com/ipfs/go-cid"
)

// btcKeysignFrozen reports whether THIS node should refuse to ISSUE a BTC TSS
// keysign for keyId (Build Map §5b — solvency auto-halt).
//
// M1.1a scope: the gate reads ONLY the deterministic governance FLAG
// (vsc.tss_halt, persisted in consensus_state, read via GetScheduler). It is a
// pure LOCAL issue-or-skip, evaluated at the top of the SignAction branch
// before any session/commitment work, so it cannot affect party lists or
// commitment CIDs (repo CLAUDE.md TSS Constraints 1-3). Because the FLAG is
// DETERMINISTIC — every node converges on the same consensus-state value by
// processing the same Hive op — ALL nodes freeze together: a clean,
// network-wide liveness stop, never a partial-participation stall.
//
// The local L1-vs-Supply SIGNAL is deliberately NOT a gate input. Adversarial
// review established that a UNILATERAL local drop is the wrong mechanism: btss
// requires 100% of the on-chain-listed parties (Constraint 1), so a SINGLE node
// that locally dropped out would stall the ENTIRE keysign for everyone (a
// griefing/DoS vector — the party list is on-chain-derived, so the dropping
// node stays listed and every other node blocks on it), and a shared-endpoint
// fail-open SIGNAL is suppressible by a capable thief. The solvency observation
// therefore belongs in the M1.1b AUTO-TRIP, where it TRIPS THE DETERMINISTIC
// FLAG (consensus-wide) rather than causing a local drop.
// observeBtcSolvencyInsolvent below is that observation, staged for M1.1b.
func (tssMgr *TssManager) btcKeysignFrozen(keyId string) bool {
	// Scope: only ever gate the BTC vault key.
	if !tssMgr.isBtcVaultKey(keyId) {
		return false
	}

	// V-8 (OPEN — resolve before relying on halt-then-evacuate): when a dedicated
	// migration/evacuation keysign key exists, whitelist it here so funds can
	// still be swept to a successor vault while the halt is up. None exists at
	// cc069b3f, so a halt currently also freezes an emergency evacuation keysign.

	// FLAG — deterministic governance freeze (the only live M1.1a gate input).
	return tssMgr.scheduler != nil && tssMgr.scheduler.BtcKeysignHalted()
}

// isBtcVaultKey reports whether keyId is the BTC vault's TSS key for the
// configured BTC mapping contract. keyIds are "<contractId>-<name>", so a prefix
// match on "<btcContractId>-" identifies BTC vault keys deterministically
// (config-derived, identical on all nodes). An empty BTC contract id (a network
// with no BTC oracle, or a TSS-only test) => not a BTC vault key. Shared by the
// M1.1a solvency gate and the M1.3 vault-rotation-v2 reshare gate.
func (tssMgr *TssManager) isBtcVaultKey(keyId string) bool {
	btcContract := tssMgr.sconf.OracleParams().ContractId("BTC")
	return btcContract != "" && strings.HasPrefix(keyId, btcContract+"-")
}

// observeBtcSolvencyInsolvent compares the vault's real L1 balance against the
// contract-claimed Supply and reports (insolvent, ok). STAGED FOR M1.1b — it is
// NOT wired to the gate. When M1.1b wires it, per the council it MUST:
//  1. feed the AUTO-TRIP that sets the deterministic FLAG (consensus-wide) —
//     NEVER cause a unilateral local drop (Constraint 1 griefing);
//  2. run OFF the TSS lock (background poller + cached verdict) — never
//     synchronous HTTP inside RunActions, which holds tssMgr.lock and would
//     cause other TSS action batches to be dropped;
//  3. read L1 from a per-node TRUSTED full node, not a shared public endpoint a
//     thief can rate-limit into a fail-open.
//
// It fails OPEN (ok=false) on any uncertainty — empty config, unreadable
// supply, unknown encoding, or an L1 read error — so a wrong assumption can
// never wrongly conclude "insolvent". Inert until BtcVaultAddresses is set.
func (tssMgr *TssManager) observeBtcSolvencyInsolvent(bh uint64) (insolvent bool, ok bool) {
	op := tssMgr.sconf.OracleParams()
	addrs := op.BtcVaultAddresses
	if len(addrs) == 0 || op.BtcL1BaseURL == "" || tssMgr.contractState == nil || tssMgr.da == nil {
		return false, false
	}
	btcContract := op.ContractId("BTC")
	if btcContract == "" {
		return false, false
	}
	rawSupply, found := tssMgr.readContractStateKey(btcContract, bh, "s")
	if !found {
		return false, false
	}
	supplySats, parsed := parseSupplySats(rawSupply)
	if !parsed {
		return false, false
	}
	client := btcclient.New(op.BtcL1BaseURL, 8*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var l1 int64
	for _, a := range addrs {
		bal, err := client.AddressBalanceSats(ctx, a)
		if err != nil {
			log.Warn("solvency observation: L1 read failed, failing open", "addr", a, "err", err)
			return false, false
		}
		l1 += bal
	}
	gap := int64(tssMgr.sconf.TssParams().SolvencyGapSats)
	return l1 < int64(supplySats)-gap, true
}

// readContractStateKey resolves a single contract state key at blockHeight,
// pinned to the contract's most-recent ContractOutput at/before bh — the same
// deterministic path as pendulum_geometry.liveContractStateKeyReader and the
// GraphQL getStateByKeys resolver. Returns (raw, false) on any miss/error.
func (tssMgr *TssManager) readContractStateKey(contractID string, bh uint64, key string) ([]byte, bool) {
	if tssMgr.contractState == nil || tssMgr.da == nil {
		return nil, false
	}
	output, err := tssMgr.contractState.GetLastOutput(contractID, bh)
	if err != nil || output.StateMerkle == "" {
		return nil, false
	}
	stateCid, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return nil, false
	}
	databin := datalayer.NewDataBinFromCid(tssMgr.da, stateCid)
	keyCid, err := databin.Get(key)
	if err != nil || keyCid == nil {
		return nil, false
	}
	raw, err := tssMgr.da.GetRaw(*keyCid)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// parseSupplySats interprets the contract's Supply value ("s") as an integer
// number of satoshis. The exact on-chain encoding lives in the BTC mapping
// contract (not in this repo), so this is intentionally conservative: it
// accepts a decimal integer (optionally JSON-quoted) and returns false on
// ANYTHING it cannot parse with confidence, so the observation fails OPEN rather
// than acting on a wrong assumption. Confirm the encoding against the contract
// before wiring the M1.1b auto-trip.
func parseSupplySats(raw []byte) (uint64, bool) {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
