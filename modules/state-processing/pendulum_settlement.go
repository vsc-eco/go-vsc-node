package state_engine

import (
	"strings"

	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	"vsc-node/modules/incentive-pendulum/rewards"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	ledgerSystem "vsc-node/modules/ledger-system"

	pendulum_settlements_db "vsc-node/modules/db/vsc/pendulum_settlements"
)

// GetLatestSettledEpoch returns the highest epoch that has been finalized by
// applyPendulumSettlement (and persisted to pendulum_settlements). Used by
// the election proposer's `canHold` guard and by the `vsc.election_result`
// handler's "prior epoch must be settled" check.
//
// Returns 0 if no settlement has been recorded yet — that is "no epoch has
// closed cleanly" and matches the genesis state.
func (se *StateEngine) GetLatestSettledEpoch() uint64 {
	if se == nil || se.pendulumSettlementsDb == nil {
		return 0
	}
	epoch, _ := se.pendulumSettlementsDb.GetLatestSettledEpoch()
	return epoch
}

// applyPendulumSettlement applies a SettlementRecord that landed inside a VSC
// block. The record's bytes were already validated by the closing committee's
// 2/3 BLS aggregate at block-acceptance time; this function trusts the bytes
// and applies the per-account distributions through the existing ledger
// session (paired debit on pendulum:nodes + credit on each named account).
//
// Idempotency: the pendulum_settlements collection's epoch index gates
// re-application; a record for an already-settled epoch is a no-op.
//
// Chain-continuity: rec.PrevEpoch must equal the current latestSettledEpoch.
// Mismatches indicate a misordered or skipped settlement and are dropped.
//
// Bucket conservation: the function does not enforce that
// rec.TotalDistributedHBD equals the live bucket balance. The state engine
// trusts the BLS-signed bytes; if the closing committee signed off on a
// payload that under-distributes, the residual carries forward in the bucket
// as expected.
func (se *StateEngine) applyPendulumSettlement(rec pendulumsettlement.SettlementRecord, blockHeight uint64) {
	if se == nil || se.LedgerSystem == nil || se.pendulumSettlementsDb == nil {
		return
	}

	latest := se.GetLatestSettledEpoch()
	if rec.Epoch <= latest {
		log.Warn("pendulum settlement: duplicate or stale epoch dropped",
			"record_epoch", rec.Epoch, "latest_settled_epoch", latest)
		return
	}
	if rec.PrevEpoch != latest {
		log.Warn("pendulum settlement: prev_epoch mismatch — chain continuity broken",
			"record_epoch", rec.Epoch, "record_prev_epoch", rec.PrevEpoch,
			"latest_settled_epoch", latest)
		return
	}

	txID := pendulumSettlementTxID(rec.Epoch)

	for _, d := range rec.Distributions {
		if d.HBDAmt <= 0 {
			continue
		}
		acct := d.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		txKey := txID + "#" + acct
		res := se.LedgerSystem.PendulumDistribute(acct, d.HBDAmt, txKey, blockHeight)
		if !res.Ok {
			log.Warn("pendulum settlement: distribute failed",
				"account", acct, "amount_hbd", d.HBDAmt, "msg", res.Msg, "epoch", rec.Epoch)
			continue
		}
	}

	if remaining := se.LedgerSystem.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, blockHeight); remaining != rec.ResidualHBD {
		log.Debug("pendulum settlement: residual differs from record",
			"epoch", rec.Epoch, "record_residual", rec.ResidualHBD, "live_residual", remaining)
	}

	if err := se.pendulumSettlementsDb.SaveMarker(pendulum_settlements_db.SettlementMarker{
		Epoch:               rec.Epoch,
		BlockHeight:         blockHeight,
		SnapshotRangeFrom:   rec.SnapshotRangeFrom,
		SnapshotRangeTo:     rec.SnapshotRangeTo,
		TotalDistributedHBD: rec.TotalDistributedHBD,
		ResidualHBD:         rec.ResidualHBD,
	}); err != nil {
		log.Warn("pendulum settlement: marker persist failed", "epoch", rec.Epoch, "err", err)
		return
	}

	log.Info("pendulum settlement applied",
		"epoch", rec.Epoch, "prev_epoch", rec.PrevEpoch,
		"total_distributed_hbd", rec.TotalDistributedHBD,
		"residual_hbd", rec.ResidualHBD,
		"distributions", len(rec.Distributions),
		"reductions", len(rec.RewardReductions))
}

// pendulumSettlementTxID is the deterministic ledger-record id prefix used to
// pair the per-account credits with the pendulum:nodes bucket debit. Includes
// the closing epoch so two epochs' settlements never collide.
func pendulumSettlementTxID(epoch uint64) string {
	// Format chosen to match the prior tx ID convention while making epoch
	// the natural collision key. Plain string concat is fine — no nesting.
	return "pendulum:settlement:" + uintToDecString(epoch)
}

func uintToDecString(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// stateEngineEpochInputsProvider adapts the state engine to the
// rewards.EpochInputsProvider interface so settlement composition can pull
// per-tick L2 evidence directly from on-chain reads.
//
// All inputs come from MongoDB-backed on-chain caches (vsc_blocks,
// tss_commitments, schedule). Two honest nodes with identical chain state
// produce byte-equal inputs and therefore byte-equal reductions.
type stateEngineEpochInputsProvider struct {
	se *StateEngine
}

func (p *stateEngineEpochInputsProvider) TickInputsForRange(tickHeight uint64) (rewards.TickInputs, bool) {
	if p == nil || p.se == nil {
		return rewards.TickInputs{}, false
	}
	in := p.se.buildTickInputs(tickHeight)
	if len(in.Committee) == 0 {
		return rewards.TickInputs{}, false
	}
	return in, true
}

// PendulumEpochInputsProvider returns the EpochInputsProvider the block
// producer uses when composing a SettlementRecord. Exposed so the
// block-producer package can re-derive reductions deterministically without
// importing internal state-engine types.
func (se *StateEngine) PendulumEpochInputsProvider() rewards.EpochInputsProvider {
	return &stateEngineEpochInputsProvider{se: se}
}

// pendulumNodesBucketBalance is a thin shim used by the block producer to read
// the bucket balance at the slot it's composing for, without exposing the
// LedgerSystem reference.
func (se *StateEngine) PendulumNodesBucketBalance(blockHeight uint64) int64 {
	if se == nil || se.LedgerSystem == nil {
		return 0
	}
	return se.LedgerSystem.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, blockHeight)
}

// PendulumOracleTickInterval returns the per-tick interval used by the
// reward-reduction aggregator (oracle.DefaultTickIntervalBlocks). Re-exported
// for callers that don't already depend on the oracle package.
func (se *StateEngine) PendulumOracleTickInterval() uint64 {
	return uint64(pendulumoracle.DefaultTickIntervalBlocks)
}

// PendulumBalanceRecordReader returns the BalanceRecordReader the settlement
// composer uses to read HIVE_CONSENSUS bonds at the settlement slot. Reads
// directly from the balance DB to bypass GetBalance's op-type filter, which
// hides freshly-staked HIVE on some code paths (see ReadCommitteeBonds doc).
func (se *StateEngine) PendulumBalanceRecordReader() pendulumsettlement.BalanceRecordReader {
	if se == nil || se.balanceDb == nil {
		return nil
	}
	return se.balanceDb
}
