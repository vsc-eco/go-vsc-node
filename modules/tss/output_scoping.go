package tss

import (
	"bytes"
	"encoding/hex"

	"vsc-node/lib/btcvault"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/btcsuite/btcd/chaincfg"
)

// S3 — node-side output-scoped retiring-key signing (Build Map non-negotiable
// #1, "don't sign blind"). The node signs a blind 32-byte sighash; a
// retiring/draining vault generation's key is the one we are rotating away from
// BECAUSE it may be reconstructed, so making it signable at all is a theft
// oracle unless every use is scoped to the successor vault. This gate refuses to
// contribute a retiring-gen share for anything but a migration sweep whose every
// output pays the successor's protocol-derived P2WSH, derived from the COMMITTED
// tss_keys pubkey (never a contract field — the C-C guard).
//
// Determinism (repo CLAUDE.md Constraints 1-3): the gate is a pure LOCAL
// pre-issuance decision at the top of the SignAction branch, before any
// session/dispatcher/party-list/Serialize/CID work (a keysign result is never
// even CID'd/BLS-collected — it is verified by direct ECDSA check), so it cannot
// perturb a party list or a commitment CID. Every input is read from consensus
// state pinned to bh (contract vault list + pending spends via
// readContractStateKey) or committed tss_keys, so all honest nodes reach the
// IDENTICAL verdict — either all issue or all skip, never a partial stall.

// btcVaultView is the deterministic snapshot of the BTC mapping contract's vault
// generations at a block height.
type btcVaultView struct {
	contractId         string
	successorKeyId     string                    // full node keyId of the single Active generation
	successorBackupHex string                    // that generation's backup pubkey (immutable/lineage-pinned)
	byKeyId            map[string]btcvault.Vault // every generation, keyed by full node keyId
}

// btcScopeDeps are the (mockable) dependencies the pure scoping logic needs. The
// contract-state reader and key finder are injected so the whole gate is unit-
// testable without a live datalayer / Mongo. In production btcSignRefused fills
// these from the TssManager.
type btcScopeDeps struct {
	contractId string
	// readKey reads a contract state key from a SINGLE already-resolved contract
	// output (production: TssManager.contractStateReaderAt resolves GetLastOutput
	// + the state databin ONCE, at the pinned bh, and every key read here hits
	// that one databin). Resolving once — rather than re-running GetLastOutput per
	// key — keeps the gate cheap under the global TSS lock (methodology F-1).
	readKey func(key string) ([]byte, bool)
	// findKey returns a keyId's COMMITTED tss_keys entry (production:
	// TssManager.tssKeys.FindKey — BLS-threshold-verified write path).
	findKey func(id string) (tss_db.TssKey, error)
	mainnet bool
}

// vaultResolveResult tri-states the vault registry read. The distinction
// between ABSENT and UNRESOLVABLE is fund-safety-critical (council B1/C-FS-1/
// A-F1): collapsing both to a single "not ok" fails the gate OPEN on a corrupt
// registry.
type vaultResolveResult int

const (
	// vaultAbsent: the "v" registry is absent/empty — pre-fold single generation,
	// or the contract not yet deployed/folded. Inert; the caller allows ONLY the
	// legacy gen-0 key (byte-identical to pre-v2).
	vaultAbsent vaultResolveResult = iota
	// vaultResolved: "v" present with exactly one Active generation — view valid.
	vaultResolved
	// vaultUnresolvable: "v" PRESENT but malformed, or without a single Active
	// generation (0 or ≥2). A corrupt/ambiguous registry must NEVER silently
	// disable output scoping → the caller refuses ALL BTC-vault keysigns (fail
	// closed, recoverable once the registry is valid).
	vaultUnresolvable
)

// resolveVaultView reads the BTC vault registry ("v") at bh and classifies it.
// The ABSENT vs UNRESOLVABLE distinction is load-bearing: an absent registry is
// the benign pre-fold case (allow gen-0 only), whereas a PRESENT-but-corrupt
// registry must fail closed — it is exactly the state a lying/compromised
// contract would write to try to disable NN#1.
func (d btcScopeDeps) resolveVaultView() (*btcVaultView, vaultResolveResult) {
	if d.contractId == "" {
		return nil, vaultAbsent
	}
	rawV, ok := d.readKey("v")
	if !ok || len(rawV) == 0 {
		return nil, vaultAbsent
	}
	// "v" is PRESENT from here on. ANY failure below is UNRESOLVABLE (fail
	// closed), NEVER treated as absent — otherwise a corrupt/ambiguous "v"
	// silently disables the output-scoping gate (the council fail-open).
	vaults, err := btcvault.UnmarshalVaultRegistry(rawV)
	if err != nil || len(vaults) == 0 {
		return nil, vaultUnresolvable
	}
	view := &btcVaultView{
		contractId: d.contractId,
		byKeyId:    make(map[string]btcvault.Vault, len(vaults)),
	}
	activeCount := 0
	for i := range vaults {
		v := vaults[i]
		keyId := d.contractId + "-" + btcvault.VaultKeyName(v.Generation)
		// Duplicate generation → the two entries alias onto the SAME map key.
		// Last-write-wins could hide a Retiring entry behind an Active one at the
		// same keyId, making evaluateScope return scopeAllow for the reconstructable
		// retiring key — a fail-open (methodology money-math HIGH). Honest state
		// never reuses a generation (monotonic, corr-F1); a duplicate = corrupt →
		// fail closed.
		if _, dup := view.byKeyId[keyId]; dup {
			return nil, vaultUnresolvable
		}
		view.byKeyId[keyId] = v
		if v.Status == btcvault.VaultStatusActive {
			view.successorKeyId = keyId
			view.successorBackupHex = hex.EncodeToString(v.Backup)
			activeCount++
		}
	}
	// Contract invariant: exactly one Active generation (enforced at activation).
	// A read state that violates it (0 or ≥2) has no unambiguous successor → not
	// resolvable → fail closed.
	if activeCount != 1 {
		return nil, vaultUnresolvable
	}
	return view, vaultResolved
}

// scopeVerdict is the output of the S3 scoping evaluation for a BTC-vault sign.
type scopeVerdict int

const (
	scopeAllow          scopeVerdict = iota // active/inert-gen sign: proceed (subject to halt)
	scopeRefuse                             // retiring-gen sign that is NOT a successor sweep, or an unknown/non-fund-holding gen
	scopeSuccessorSweep                     // retiring-gen sign PROVEN to pay only the successor (V-8: exempt from the halt)
)

// evaluateScope applies S3 output scoping for a BTC-vault keyId whose sign
// request carries sighash, at bh. It is the pure, deterministic core of
// btcSignRefused, split out so it is unit-testable via btcScopeDeps.
func (d btcScopeDeps) evaluateScope(keyId string, sighash []byte) scopeVerdict {
	view, res := d.resolveVaultView()
	switch res {
	case vaultAbsent:
		// Pre-fold / not-yet-folded: the ONLY legitimate BTC vault key is the
		// legacy gen-0 "main" (byte-identical to pre-v2 signing). A higher-
		// generation keyId ("mainv<N>") cannot exist without a vault registry, so
		// a sign for one under an absent registry is anomalous → fail closed.
		if keyId == d.contractId+"-"+btcvault.VaultKeyName(0) {
			return scopeAllow
		}
		return scopeRefuse
	case vaultUnresolvable:
		// Registry PRESENT but corrupt / no single Active successor. We cannot
		// scope safely AND must not let a bad "v" disable the gate → refuse ALL
		// BTC-vault keysigns (fail closed; recoverable when "v" is valid again).
		return scopeRefuse
	case vaultResolved:
		v, known := view.byKeyId[keyId]
		switch {
		case !known:
			return scopeRefuse // a BTC vault keyId not present in the registry
		case v.Status == btcvault.VaultStatusActive:
			// The active successor generation signs ordinary user withdrawals,
			// whose destinations are arbitrary (the contract is the authority on
			// where a user's own funds go) — deliberately NOT output-scoped.
			return scopeAllow
		case v.Status == btcvault.VaultStatusRetiring || v.Status == btcvault.VaultStatusDraining:
			if d.retiringSignPaysSuccessor(sighash, view) {
				return scopeSuccessorSweep
			}
			return scopeRefuse
		default:
			// pending / inactive / purged: not a fund-holding, signable generation.
			return scopeRefuse
		}
	}
	return scopeRefuse // unreachable; fail closed
}

// btcSignGateDecision combines the S3 scope verdict with the M1.1a solvency
// halt into a final skip-or-issue decision (returns true = skip issuance). Pure,
// so the V-8 evacuation exemption is unit-testable without a live datalayer.
//   - scopeRefuse            → always skip (output scoping refused it).
//   - halted, not a sweep    → skip (the solvency halt freezes issuance).
//   - halted, scopeSuccessorSweep → ISSUE (V-8: the honest evacuation to the new
//     vault must complete even during a halt; it is output-proven successor-only).
//   - not halted, allow/sweep → issue.
func btcSignGateDecision(verdict scopeVerdict, halted bool) bool {
	if verdict == scopeRefuse {
		return true
	}
	if halted && verdict != scopeSuccessorSweep {
		return true
	}
	return false
}

// btcSignRefused is the deterministic pre-issuance gate for a BTC-vault keysign:
// S3 output scoping + the M1.1a solvency halt (with the V-8 evacuation
// exemption). It returns true when THIS node must skip issuing the keysign.
func (tssMgr *TssManager) btcSignRefused(keyId string, sighash []byte, bh uint64) bool {
	if !tssMgr.isBtcVaultKey(keyId) {
		return false // not a BTC vault key: this gate does not apply
	}

	// S3.2 — output scoping. Only under the (inert-until-pinned) rotation flag;
	// otherwise the verdict is scopeAllow (M1.1a-only behaviour, byte-identical
	// to pre-S3).
	verdict := scopeAllow
	if tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		btcContract := tssMgr.sconf.OracleParams().ContractId("BTC")
		deps := btcScopeDeps{
			contractId: btcContract,
			// Resolve the contract's committed output at bh ONCE; every "v"/"va"/
			// "p"/"d-<txid>" read reuses that one databin (F-1: no per-key
			// GetLastOutput, bounded work under the TSS lock).
			readKey: tssMgr.contractStateReaderAt(btcContract, bh),
			findKey: tssMgr.tssKeys.FindKey,
			mainnet: tssMgr.sconf.OnMainnet(),
		}
		verdict = deps.evaluateScope(keyId, sighash)
	}

	// Combine with the M1.1a solvency halt (FLAG) + the V-8 evacuation exemption.
	if btcSignGateDecision(verdict, tssMgr.btcKeysignFrozen(keyId)) {
		if verdict == scopeRefuse {
			log.Warn("BTC keysign refused by output scoping (S3 NN#1)", "keyId", keyId, "bh", bh)
		} else {
			log.Warn("BTC keysign frozen by solvency gate; skipping issuance", "keyId", keyId, "bh", bh)
		}
		return true
	}
	return false
}

// retiringSignPaysSuccessor reports whether sighash is the BIP143 sighash of an
// input of a pending migration sweep whose EVERY output pays the successor
// vault's protocol-derived P2WSH. The successor PRIMARY pubkey comes from
// COMMITTED tss_keys (BLS-threshold-verified write path), NEVER a contract field
// (the C-C guard); the backup is the vault's immutable/lineage-pinned backup.
// The untrusted template is bound to the signature by independently recomputing
// its sighash and requiring it equal `sighash` — a SigHashAll digest commits to
// the outputs, so a signature over it can only ever attach to a successor-paying
// tx.
//
// Fails CLOSED on any uncertainty (missing committed key, unreadable pending
// spends, absent input amount, parse error, no matching template): a retiring-gen
// sign we cannot PROVE is a successor sweep must not be issued. Worst case is a
// liveness freeze of a legitimate-but-unverifiable sweep (recoverable), never a
// theft.
func (d btcScopeDeps) retiringSignPaysSuccessor(sighash []byte, view *btcVaultView) bool {
	// Successor primary from COMMITTED tss_keys (C-C guard) — must be present +
	// active (the check-signature gate / activation guarantees an active
	// successor can actually sign).
	successorKey, err := d.findKey(view.successorKeyId)
	if err != nil || successorKey.PublicKey == "" || successorKey.Status != tss_db.TssKeyActive {
		log.Warn("output-scoping: successor key not committed/active — refusing sweep", "successorKeyId", view.successorKeyId, "err", err)
		return false
	}
	net := btcNetParams(d.mainnet)
	csv := btcvault.CSVBlocksForNet(net)
	_, successorPk, err := btcvault.SuccessorPkScript(successorKey.PublicKey, view.successorBackupHex, csv, net)
	if err != nil {
		log.Warn("output-scoping: successor pkScript derivation failed — refusing sweep", "err", err)
		return false
	}

	// Pending spends live in consensus contract state ("p" registry + "d-"<txid>).
	rawP, ok := d.readKey("p")
	if !ok {
		return false
	}
	txids, err := btcvault.UnmarshalTxSpendsRegistry(rawP)
	if err != nil {
		return false
	}
	for _, txid := range txids {
		rawSD, ok := d.readKey("d-" + txid)
		if !ok {
			continue
		}
		sd, err := btcvault.DecodeSigningData(rawSD)
		if err != nil {
			continue
		}
		for _, uh := range sd.UnsignedSigHashes {
			if !bytes.Equal(uh.SigHash, sighash) {
				continue
			}
			// The contract stored this SigHash for this input. BIND it: recompute
			// independently and require the tx's real sighash equals what we sign,
			// so a lying/corrupt template (stored SigHash ≠ its own tx) is rejected.
			if !uh.HasAmount {
				// Amount not carried (pre-S3.5 contract) → cannot recompute → fail
				// closed. S3.5 adds the amount to the contract's UnsignedSigHash.
				log.Warn("output-scoping: pending spend lacks input amount; cannot bind sighash — refusing", "txid", txid)
				return false
			}
			recomputed, err := btcvault.RecomputeSegwitV0Sighash(sd.Tx, int(uh.Index), uh.WitnessScript, uh.Amount)
			if err != nil || !bytes.Equal(recomputed, sighash) {
				log.Warn("output-scoping: recomputed sighash != signed digest (corrupt/lying template) — refusing", "txid", txid)
				return false
			}
			tx, err := btcvault.ParseTx(sd.Tx)
			if err != nil {
				return false
			}
			// Every output must pay the committed successor vault.
			return btcvault.AllOutputsPay(tx, successorPk)
		}
	}
	// The digest belongs to no pending migration sweep → a retiring key asked to
	// sign something outside its authorized successor sweep → refuse.
	return false
}

// btcNetParams maps the node's network mode to btcd chain params. Only mainnet
// vs not matters for the successor script (the P2WSH scriptPubKey itself is
// network-independent; the CSV timelock is 4320 on mainnet and 2 otherwise,
// mirroring the contract).
func btcNetParams(mainnet bool) *chaincfg.Params {
	if mainnet {
		return &chaincfg.MainNetParams
	}
	return &chaincfg.TestNet3Params
}
