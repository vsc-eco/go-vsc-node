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
	// readKey resolves a single contract state key at bh, deterministically
	// (production: TssManager.readContractStateKey → GetLastOutput pinned to bh).
	readKey func(contractID string, bh uint64, key string) ([]byte, bool)
	// findKey returns a keyId's COMMITTED tss_keys entry (production:
	// TssManager.tssKeys.FindKey — BLS-threshold-verified write path).
	findKey func(id string) (tss_db.TssKey, error)
	mainnet bool
}

// resolveVaultView reads the BTC vault registry ("v"/"va") at bh. Returns
// (nil,false) when there is no enforceable v2 vault state — an absent/empty
// registry (pre-fold single generation, or the contract not yet deployed) is the
// inert case and behaves byte-identically to pre-v2 (the caller then applies no
// scoping). A registry present but without exactly one resolvable Active
// generation also returns false, so the gate fails CLOSED for any retiring-gen
// sign (see btcSignRefused).
func (d btcScopeDeps) resolveVaultView(bh uint64) (*btcVaultView, bool) {
	if d.contractId == "" {
		return nil, false
	}
	rawV, ok := d.readKey(d.contractId, bh, "v")
	if !ok || len(rawV) == 0 {
		return nil, false
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(rawV)
	if err != nil || len(vaults) == 0 {
		return nil, false
	}
	view := &btcVaultView{
		contractId: d.contractId,
		byKeyId:    make(map[string]btcvault.Vault, len(vaults)),
	}
	activeCount := 0
	for _, v := range vaults {
		keyId := d.contractId + "-" + btcvault.VaultKeyName(v.Generation)
		view.byKeyId[keyId] = v
		if v.Status == btcvault.VaultStatusActive {
			view.successorKeyId = keyId
			view.successorBackupHex = hex.EncodeToString(v.Backup)
			activeCount++
		}
	}
	// Contract invariant: exactly one Active generation. If the read state does
	// not satisfy it (0 or >1), no unambiguous successor can be named → fail
	// closed. (A 0-active state means no vault can receive a sweep; a >1-active
	// state is impossible per the contract; either way refusing retiring-gen
	// signs is the safe outcome.)
	if activeCount != 1 {
		return nil, false
	}
	return view, true
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
func (d btcScopeDeps) evaluateScope(keyId string, sighash []byte, bh uint64) scopeVerdict {
	view, ok := d.resolveVaultView(bh)
	if !ok {
		// No resolvable v2 vault state → inert / pre-fold → no scoping applied.
		return scopeAllow
	}
	v, known := view.byKeyId[keyId]
	switch {
	case !known:
		return scopeRefuse // a BTC vault keyId not present in the registry
	case v.Status == btcvault.VaultStatusActive:
		// The active successor generation signs ordinary user withdrawals, whose
		// destinations are arbitrary (the contract is the authority on where a
		// user's own funds go) — deliberately NOT output-scoped.
		return scopeAllow
	case v.Status == btcvault.VaultStatusRetiring || v.Status == btcvault.VaultStatusDraining:
		if d.retiringSignPaysSuccessor(sighash, view, bh) {
			return scopeSuccessorSweep
		}
		return scopeRefuse
	default:
		// pending / inactive / purged: not a fund-holding, signable generation.
		return scopeRefuse
	}
}

// btcSignRefused is the deterministic pre-issuance gate for a BTC-vault keysign:
// S3 output scoping + the M1.1a solvency halt (with the V-8 evacuation
// exemption). It returns true when THIS node must skip issuing the keysign.
func (tssMgr *TssManager) btcSignRefused(keyId string, sighash []byte, bh uint64) bool {
	if !tssMgr.isBtcVaultKey(keyId) {
		return false // not a BTC vault key: this gate does not apply
	}

	// S3.2 — output scoping. Only under the (inert-until-pinned) rotation flag.
	isSuccessorScopedSweep := false
	if tssMgr.sconf.ConsensusParams().VaultRotationV2Enabled(bh) {
		deps := btcScopeDeps{
			contractId: tssMgr.sconf.OracleParams().ContractId("BTC"),
			readKey:    tssMgr.readContractStateKey,
			findKey:    tssMgr.tssKeys.FindKey,
			mainnet:    tssMgr.sconf.OnMainnet(),
		}
		switch deps.evaluateScope(keyId, sighash, bh) {
		case scopeRefuse:
			log.Warn("BTC keysign refused by output scoping (S3 NN#1)", "keyId", keyId, "bh", bh)
			return true
		case scopeSuccessorSweep:
			isSuccessorScopedSweep = true
		case scopeAllow:
			// proceed to the halt check
		}
	}

	// M1.1a solvency halt (deterministic FLAG) + V-8 evacuation whitelist: freeze
	// BTC keysign issuance when the FLAG is up, EXCEPT a proven successor-scoped
	// sweep — the honest evacuation to the new vault must proceed even during a
	// solvency halt (it moves funds within the protocol's custody, independently
	// output-checked against the committed successor).
	if tssMgr.btcKeysignFrozen(keyId) && !isSuccessorScopedSweep {
		log.Warn("BTC keysign frozen by solvency gate; skipping issuance", "keyId", keyId, "bh", bh)
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
func (d btcScopeDeps) retiringSignPaysSuccessor(sighash []byte, view *btcVaultView, bh uint64) bool {
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
	rawP, ok := d.readKey(view.contractId, bh, "p")
	if !ok {
		return false
	}
	txids, err := btcvault.UnmarshalTxSpendsRegistry(rawP)
	if err != nil {
		return false
	}
	for _, txid := range txids {
		rawSD, ok := d.readKey(view.contractId, bh, "d-"+txid)
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
