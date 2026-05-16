package state_engine

import (
	"bytes"
	"fmt"
	"strings"

	"vsc-node/modules/common"
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

// seedPendulumSettlement bootstraps the settlement chain on a network that
// pre-dates inlined settlement. Run once at state-engine Init, before block
// consumption.
//
// On an in-place upgrade the pendulum_settlements collection is empty, so
// GetLatestSettledEpoch() returns 0 and the proposer's canHold gate
// (latestSettled >= closingEpoch-1) can never be satisfied — elections halt.
// Seeding a single marker for the network-wide PendulumSeedEpoch constant
// fixes that: every node (in-place or reindexed) arrives at the identical
// latestSettledEpoch, so composed settlement PrevEpoch values agree and the
// gate passes for the first post-rollout election.
//
// No-ops when: PendulumSeedEpoch is 0 (fresh chain built with settlement
// already present — latestSettledEpoch advances naturally from genesis), or a
// marker already exists (already seeded, or the chain has advanced past it —
// must not clobber real state with the seed value).
func (se *StateEngine) seedPendulumSettlement() {
	if se == nil || se.pendulumSettlementsDb == nil || se.sconf == nil {
		return
	}
	seedEpoch := se.sconf.ConsensusParams().PendulumSeedEpoch
	if seedEpoch == 0 {
		return
	}
	if _, found := se.pendulumSettlementsDb.GetLatestSettledEpoch(); found {
		return
	}
	if err := se.pendulumSettlementsDb.SaveMarker(pendulum_settlements_db.SettlementMarker{
		Epoch: seedEpoch,
	}); err != nil {
		log.Error("pendulum settlement: seed marker persist failed", "seed_epoch", seedEpoch, "err", err)
		return
	}
	log.Info("pendulum settlement: seeded bootstrap marker", "seed_epoch", seedEpoch)
}

// applyPendulumSettlement applies a SettlementRecord that landed inside a VSC
// block. The record's bytes were already validated by the closing committee's
// 2/3 BLS aggregate at block-acceptance time; this function adds two layers
// of defense-in-depth before touching the ledger:
//
//  1. Structural validation (validatePendulumSettlement) — pure-arithmetic
//     conservation + committee-membership check. Cheap, no flush-timing
//     risk. Hard-fails the apply on violation.
//  2. Re-derivation (rederivePendulumSettlement) — re-runs ComposeRecord
//     against the same on-chain inputs at the snapshot heights and byte-
//     compares the CBOR encoding. Hard-fails on byte mismatch; transient
//     inability to run the re-derivation falls through to the structural
//     check we already passed.
//
// Per-distribution failures inside the loop are logged and skipped (matches
// the codebase-wide ledger-write convention — recovery via restart-replay
// + idempotent upserts, not transactional rollback). Real atomicity should
// come from a future refactor that wraps multi-record writes in
// mongo.Session.WithTransaction at the LedgerDb level.
//
// Idempotency: the pendulum_settlements collection's epoch index gates
// re-application; a record for an already-settled epoch is a no-op.
//
// Chain-continuity: rec.PrevEpoch must equal the current latestSettledEpoch.
// Mismatches indicate a misordered or skipped settlement and are dropped.
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

	// Defense-in-depth pass 1: structural safety. Catches paying outsiders,
	// over-spending the bucket, and sum/distribution conservation breaks
	// regardless of any other state. Pure-arithmetic over the payload plus
	// a committee-membership lookup at the snapshot height; cannot false-
	// positive due to per-node DB flush timing.
	if err := se.validatePendulumSettlement(rec); err != nil {
		log.Error("pendulum settlement: structural validation failed; refusing to apply",
			"epoch", rec.Epoch, "err", err)
		return
	}

	// Defense-in-depth pass 2: re-derive the expected payload from on-chain
	// reads at the snapshot heights and byte-compare via CBOR. The 2/3 BLS
	// aggregate is the primary check; this is the second line — if our own
	// re-derivation disagrees with what the chain attested, refuse rather
	// than silently apply a payload we would not have signed.
	if mismatchErr, ran := se.rederivePendulumSettlement(rec); ran && mismatchErr != nil {
		log.Error("pendulum settlement: re-derivation mismatch; refusing to apply",
			"epoch", rec.Epoch, "err", mismatchErr)
		return
	} else if !ran {
		log.Warn("pendulum settlement: re-derivation could not run; relying on structural validation only",
			"epoch", rec.Epoch)
	}

	txID := pendulumSettlementTxID(rec.Epoch)

	// Per-distribution apply: log and continue on failure rather than abort.
	// This matches every other multi-record ledger-write site in the
	// codebase (the slot-end ExecuteBatch flush, IngestOplog, IndexActions,
	// ClaimHBDInterest, Deposit) — none of them wrap their writes in a
	// Mongo transaction or roll back on a per-record failure. The shared
	// recovery story is "deterministic record IDs + idempotent upserts +
	// restart-replay reconciles partial state."
	//
	// Validation layer 1 already guarantees TotalDistributed ≤ Bucket, so
	// PendulumDistribute can't fail from insufficient funds. The remaining
	// failure surface is a transient Mongo write error, which StoreLedger
	// silently swallows today anyway — meaning the !res.Ok branch is
	// effectively unreachable in current code. A future refactor that
	// wraps the apply path (and the rest of the codebase's ledger writes)
	// in mongo.Session.WithTransaction is the right place to introduce
	// real atomicity; doing it just here would be inconsistent with how
	// every other write site behaves.
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

// validatePendulumSettlement performs structural safety checks that hold
// regardless of state-engine flush timing — pure arithmetic over the
// payload plus a committee-membership lookup at the snapshot height. Even
// though the carrying VSC block is BLS-attested by 2/3 of the committee,
// these checks catch the most exploitable abuse classes (paying non-
// members, over-spending the bucket, sum/distribution mismatch) that a
// malicious leader plus complicit signers could attempt.
//
// Returns an error on any structural violation; the caller refuses the
// apply. A transient electionDb error during the membership check is
// downgraded to a warning so a single-node DB blip doesn't cause this
// node to drop a legitimate settlement — the arithmetic invariants above
// still bound the worst case.
func (se *StateEngine) validatePendulumSettlement(rec pendulumsettlement.SettlementRecord) error {
	if rec.BucketBalanceHBD < 0 {
		return fmt.Errorf("negative BucketBalanceHBD %d", rec.BucketBalanceHBD)
	}
	if rec.TotalDistributedHBD < 0 {
		return fmt.Errorf("negative TotalDistributedHBD %d", rec.TotalDistributedHBD)
	}
	// Over-spend check fires before negative-residual so the more exploit-
	// relevant message wins when both are violated by the same payload.
	if rec.TotalDistributedHBD > rec.BucketBalanceHBD {
		return fmt.Errorf("TotalDistributedHBD %d exceeds BucketBalanceHBD %d",
			rec.TotalDistributedHBD, rec.BucketBalanceHBD)
	}
	if rec.ResidualHBD < 0 {
		return fmt.Errorf("negative ResidualHBD %d", rec.ResidualHBD)
	}
	if rec.TotalDistributedHBD+rec.ResidualHBD != rec.BucketBalanceHBD {
		return fmt.Errorf("conservation broken: distributed %d + residual %d != bucket %d",
			rec.TotalDistributedHBD, rec.ResidualHBD, rec.BucketBalanceHBD)
	}

	var sum int64
	for _, d := range rec.Distributions {
		if d.HBDAmt < 0 {
			return fmt.Errorf("negative distribution amount %d for %s", d.HBDAmt, d.Account)
		}
		sum += d.HBDAmt
	}
	if sum != rec.TotalDistributedHBD {
		return fmt.Errorf("sum(distributions)=%d != TotalDistributedHBD=%d",
			sum, rec.TotalDistributedHBD)
	}

	if se.electionDb == nil {
		return nil
	}
	election, err := se.electionDb.GetElectionByHeight(rec.SnapshotRangeTo)
	if err != nil {
		log.Warn("pendulum settlement: election lookup failed during validation; membership check skipped",
			"epoch", rec.Epoch, "snapshot_to", rec.SnapshotRangeTo, "err", err)
		return nil
	}
	if len(election.Members) == 0 {
		return fmt.Errorf("election at snapshot_to=%d has no members", rec.SnapshotRangeTo)
	}
	members := make(map[string]struct{}, len(election.Members))
	for _, m := range election.Members {
		acct := m.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		members[acct] = struct{}{}
	}
	for _, d := range rec.Distributions {
		acct := d.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		if _, ok := members[acct]; !ok {
			return fmt.Errorf("distribution to non-committee account %s at epoch=%d",
				d.Account, rec.Epoch)
		}
	}
	for _, r := range rec.RewardReductions {
		acct := r.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		if _, ok := members[acct]; !ok {
			return fmt.Errorf("reward reduction for non-committee account %s at epoch=%d",
				r.Account, rec.Epoch)
		}
	}
	return nil
}

// rederivePendulumSettlement re-runs ComposeRecord against the same on-chain
// inputs the leader used and byte-compares the CBOR encoding with rec. The
// snapshot heights (SnapshotRangeFrom/To) are the contract that lets every
// honest node — at any apply time, regardless of when its consumer caught
// up — read identical inputs and produce a byte-equal record.
//
// Returns:
//   - (nil, true) if the re-derivation ran and matched the payload,
//   - (mismatchErr, true) if it ran and the bytes diverged (caller refuses apply),
//   - (nil, false) if it could not run because of a transient input
//     unavailability (caller falls back to structural validation only).
func (se *StateEngine) rederivePendulumSettlement(rec pendulumsettlement.SettlementRecord) (mismatchErr error, ran bool) {
	if se == nil || se.electionDb == nil || se.balanceDb == nil || se.LedgerSystem == nil {
		return nil, false
	}
	election, err := se.electionDb.GetElectionByHeight(rec.SnapshotRangeTo)
	if err != nil || len(election.Members) == 0 {
		return nil, false
	}

	members := make([]string, 0, len(election.Members))
	for _, m := range election.Members {
		acct := m.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		members = append(members, acct)
	}

	bonds := pendulumsettlement.ReadCommitteeBonds(se.balanceDb, members, rec.SnapshotRangeTo)
	bucket := se.LedgerSystem.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, rec.SnapshotRangeTo)
	// bucket==0 is a real (empty-activity) state ComposeRecord must
	// re-derive against; bonds==0 only matters when bucket>0 and the
	// composer would have errored. Both are passed through and let
	// ComposeRecord decide.
	if bucket < 0 {
		return nil, false
	}

	reductions := rewards.ComputeReductionsForEpoch(
		se.PendulumEpochInputsProvider(),
		rec.SnapshotRangeFrom,
		rec.SnapshotRangeTo,
		uint64(pendulumoracle.DefaultTickIntervalBlocks),
	)

	expected, err := pendulumsettlement.ComposeRecord(pendulumsettlement.ComposeInputs{
		Epoch:               rec.Epoch,
		PrevEpoch:           rec.PrevEpoch,
		EpochStartBh:        rec.SnapshotRangeFrom,
		SlotHeight:          rec.SnapshotRangeTo,
		CommitteeBonds:      bonds,
		BucketBalanceHBD:    bucket,
		ReductionsByAccount: reductions,
	})
	if err != nil || expected == nil {
		return nil, false
	}

	expectedBytes, err := common.EncodeDagCbor(*expected)
	if err != nil {
		return nil, false
	}
	actualBytes, err := common.EncodeDagCbor(rec)
	if err != nil {
		return nil, false
	}
	if !bytes.Equal(expectedBytes, actualBytes) {
		return fmt.Errorf("settlement byte mismatch (expected %d bytes, got %d)",
			len(expectedBytes), len(actualBytes)), true
	}
	return nil, true
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
