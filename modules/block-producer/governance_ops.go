package blockproducer

import (
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"

	"github.com/multiformats/go-multicodec"
)

// maxGovernanceOpsPerBlock caps how many of each governance op kind a
// single block can carry. Generous enough to clear normal DAO traffic
// in a few slots, conservative enough to bound block size in a poison-
// pool attack scenario. Tunable per network.
const maxGovernanceOpsPerBlock = 32

// MakeRestitutionClaims drains up to maxGovernanceOpsPerBlock entries
// from PendingGovOps, encodes each as DAG-CBOR, pins the bytes to the
// supplied DataLayer session, and returns the resulting block-tx
// stubs. The carrying block's BLS aggregate is the auth gate; signers
// fetch the bytes from the DataLayer and replay the apply path
// (which independently re-validates against on-chain state and silently
// drops invalid entries).
//
// The pool is process-local. Different leaders may carry different
// (or zero) governance ops per slot; this is fine because invalid
// payloads cannot mutate state. The chain-op surface treats every
// included payload as a *suggestion* — only the apply path's checks
// against the canonical ledger control whether the suggestion lands.
//
// Returns nil if the pool is empty so the caller can skip the append.
func (bp *BlockProducer) MakeRestitutionClaims(session *datalayer.Session) []vscBlocks.VscBlockTx {
	if bp == nil || bp.PendingGovOps == nil || session == nil {
		return nil
	}
	claims := bp.PendingGovOps.DrainRestitutionClaims(maxGovernanceOpsPerBlock)
	if len(claims) == 0 {
		return nil
	}
	out := make([]vscBlocks.VscBlockTx, 0, len(claims))
	for _, rec := range claims {
		// Re-normalise inside the producer so the bytes that hit the
		// DataLayer are the canonical form regardless of how the
		// submitter spelled the strings. Cheap; the apply path
		// normalises again as defense-in-depth.
		rec = rec.Normalize()

		cborBytes, err := common.EncodeDagCbor(rec)
		if err != nil {
			vlog.Warn("MakeRestitutionClaims: cbor encode failed",
				"claim_id", rec.ClaimID, "err", err)
			continue
		}
		cidObj, err := common.HashBytes(cborBytes, multicodec.DagCbor)
		if err != nil {
			vlog.Warn("MakeRestitutionClaims: cid hash failed",
				"claim_id", rec.ClaimID, "err", err)
			continue
		}
		session.Put(cborBytes, cidObj)

		vlog.Info("MakeRestitutionClaims: composed claim",
			"claim_id", rec.ClaimID,
			"victim", rec.VictimAccount,
			"slash_tx", rec.SlashTxID,
			"loss_hive", rec.LossHive)

		out = append(out, vscBlocks.VscBlockTx{
			Id:   cidObj.String(),
			Type: int(common.BlockTypeRestitutionClaim),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MakeSafetySlashReverses mirrors MakeRestitutionClaims for the
// vsc.safety_slash_reverse payload. Drains the pool, encodes each
// record, pins bytes, and returns the block-tx stubs. The leader's
// inclusion is the authoring step; the 2/3 BLS aggregate is the auth
// gate; the apply path is the policy gate.
func (bp *BlockProducer) MakeSafetySlashReverses(session *datalayer.Session) []vscBlocks.VscBlockTx {
	if bp == nil || bp.PendingGovOps == nil || session == nil {
		return nil
	}
	reverses := bp.PendingGovOps.DrainSlashReverses(maxGovernanceOpsPerBlock)
	if len(reverses) == 0 {
		return nil
	}
	out := make([]vscBlocks.VscBlockTx, 0, len(reverses))
	for _, rec := range reverses {
		rec = rec.Normalize()

		cborBytes, err := common.EncodeDagCbor(rec)
		if err != nil {
			vlog.Warn("MakeSafetySlashReverses: cbor encode failed",
				"slash_tx", rec.SlashTxID, "action", rec.Action, "err", err)
			continue
		}
		cidObj, err := common.HashBytes(cborBytes, multicodec.DagCbor)
		if err != nil {
			vlog.Warn("MakeSafetySlashReverses: cid hash failed",
				"slash_tx", rec.SlashTxID, "action", rec.Action, "err", err)
			continue
		}
		session.Put(cborBytes, cidObj)

		vlog.Info("MakeSafetySlashReverses: composed reverse",
			"slash_tx", rec.SlashTxID,
			"action", rec.Action,
			"slashed", rec.SlashedAccount,
			"amount", rec.Amount)

		out = append(out, vscBlocks.VscBlockTx{
			Id:   cidObj.String(),
			Type: int(common.BlockTypeSafetySlashReverse),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Compile-time guard: the producer relies on PendingGovOps satisfying
// the interface so deployments can substitute a persistent backing
// pool without touching this file.
var _ safetyslash.PendingGovernanceOps = (*safetyslash.MemoryPendingGovernanceOps)(nil)
