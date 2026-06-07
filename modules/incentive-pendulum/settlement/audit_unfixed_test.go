package settlement

import (
	"strings"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/incentive-pendulum/rewards"
)

// TestAuditFix_52_ChainAdvancesOnFullReduction — Pendulum audit MEDIUM #52.
//
// Pre-fix: when every committee member earned PerEpochCapBps reduction, the
// total effective bond collapsed to 0 and ComposeRecord errored out. That
// error propagated through election-proposer and froze the chain
// deterministically on every honest node — a universal stall reachable
// purely from on-chain evidence.
//
// Post-fix: ComposeRecord emits a marker-only record with the full bucket
// rolled forward as ResidualHBD and the reductions preserved for audit. The
// chain advances past the fully-reduced epoch instead of stalling.
func TestAuditFix_52_ChainAdvancesOnFullReduction(t *testing.T) {
	bonds := map[string]int64{
		"hive:alice": 1_000_000,
		"hive:bob":   500_000,
		"hive:carol": 250_000,
	}
	reductions := map[string]int{
		"alice": rewards.PerEpochCapBps,
		"bob":   rewards.PerEpochCapBps,
		"carol": rewards.PerEpochCapBps,
	}

	const bucket int64 = 1_000_000
	rec, err := ComposeRecord(ComposeInputs{
		Epoch:               10,
		PrevEpoch:           9,
		EpochStartBh:        1000,
		SlotHeight:          2000,
		CommitteeBonds:      bonds,
		BucketBalanceHBD:    bucket,
		ReductionsByAccount: reductions,
	})

	if err != nil {
		t.Fatalf("audit #52: expected ComposeRecord to succeed with a marker, got err: %v", err)
	}
	if rec == nil {
		t.Fatal("audit #52: expected non-nil marker record")
	}
	if rec.TotalDistributedHBD != 0 {
		t.Errorf("audit #52: expected 0 distributed on fully-reduced epoch, got %d", rec.TotalDistributedHBD)
	}
	if rec.ResidualHBD != bucket {
		t.Errorf("audit #52: expected full bucket to roll forward as residual; got %d, want %d", rec.ResidualHBD, bucket)
	}
	if rec.BucketBalanceHBD != bucket {
		t.Errorf("audit #52: bucket balance must round-trip; got %d, want %d", rec.BucketBalanceHBD, bucket)
	}
	if len(rec.Distributions) != 0 {
		t.Errorf("audit #52: expected no distributions on fully-reduced epoch, got %d entries", len(rec.Distributions))
	}
	if len(rec.RewardReductions) != len(reductions) {
		t.Errorf("audit #52: expected all %d reductions preserved on the marker, got %d", len(reductions), len(rec.RewardReductions))
	}

	// silence the unused-import alarm if "strings" is removed from this test
	_ = strings.Contains
}

// TestAuditUnfixed_60_ReductionsDiscardedOnZeroBucket — Pendulum audit
// MEDIUM #60.
//
// Precondition: on an empty-activity epoch (BucketBalanceHBD == 0),
// ComposeRecord takes the marker-only fast path at compose.go:62-76:
//
//	if in.BucketBalanceHBD == 0 {
//	    rec := BuildSettlementRecord(
//	        in.Epoch, in.PrevEpoch, in.EpochStartBh, in.SlotHeight,
//	        0, 0, 0,
//	        nil, // reductions discarded
//	        nil, // distributions discarded
//	    )
//	    return &rec, nil
//	}
//
// Any non-zero reductions in ReductionsByAccount are silently dropped — the
// per-epoch penalty evidence (block_production / attestation / tss_* tick
// data) never makes it into the on-chain SettlementRecord, so the on-chain
// audit trail loses the misbehavior signal for that epoch.
//
// Post-fix the marker should still carry the reductions list (even if no HBD
// is distributed), so the misbehavior record is preserved.
func TestAuditUnfixed_60_ReductionsDiscardedOnZeroBucket(t *testing.T) {
	rec, err := ComposeRecord(ComposeInputs{
		Epoch:        7,
		PrevEpoch:    6,
		EpochStartBh: 1000,
		SlotHeight:   2000,
		// Bonds are irrelevant on the zero-bucket fast path, but we set them
		// to mirror a realistic call site.
		CommitteeBonds: map[string]int64{
			"hive:alice": 1_000_000,
			"hive:bob":   500_000,
		},
		BucketBalanceHBD: 0, // empty-activity epoch — triggers the fast path
		ReductionsByAccount: map[string]int{
			"alice": 1500, // 15% reduction earned this epoch
			"bob":   500,  // 5% reduction earned this epoch
		},
	})
	if err != nil {
		t.Fatalf("audit #60: unexpected err: %v", err)
	}
	if rec == nil {
		t.Fatal("audit #60: expected non-nil marker record")
	}

	// Current (buggy) behavior: the marker's RewardReductions slice is empty
	// even though ReductionsByAccount carried two non-zero entries.
	if len(rec.RewardReductions) != 0 {
		t.Fatalf("audit #60: expected zero reductions in marker (current buggy behavior), got %d: %+v",
			len(rec.RewardReductions), rec.RewardReductions)
	}

	// Sanity: the marker fields themselves are emitted (it's only the
	// reduction payload that's lost). This isolates the regression surface.
	if rec.Epoch != 7 || rec.PrevEpoch != 6 {
		t.Errorf("audit #60: marker chain fields wrong: epoch=%d prev=%d", rec.Epoch, rec.PrevEpoch)
	}
	if rec.BucketBalanceHBD != 0 || rec.TotalDistributedHBD != 0 || rec.ResidualHBD != 0 {
		t.Errorf("audit #60: marker HBD fields wrong: %+v", rec)
	}

	// Post-fix the assertion above should flip: the marker should carry both
	// reduction entries (alice@1500, bob@500) so the on-chain audit trail
	// preserves per-epoch misbehavior signals even when there is no HBD to
	// distribute. The test would then assert
	// len(rec.RewardReductions) == 2 with the bps values matching.
}

// TestAuditFix_122_BondReaderMinAcrossWindow — Pendulum audit MEDIUM #122
// (flash-stake snapshot, now TWAB-style min).
//
// Pre-fix: ReadCommitteeBonds did a single point-in-time read at slotHeight.
// A flash-stake at slotHeight−1 got the full pro-rata distribution weight
// as if it had been bonded the whole epoch.
//
// Post-fix: ReadCommitteeBonds samples bondSampleCount points across
// (epochStartBh, slotHeight] and returns min(HIVE_CONSENSUS) per account.
// alice's flash-stake at slotHeight stays unfunded for the whole interior
// of the window, so the minimum is the pre-flash balance (0) and she is
// omitted from the bonds map.
func TestAuditFix_122_BondReaderMinAcrossWindow(t *testing.T) {
	reader := &auditFlashStakeReader{
		flashAccount:    "hive:alice",
		preFlashBalance: 0,
		flashBalance:    1_000_000,
		flashBlock:      2000, // == slotHeight
	}

	const epochStartBh uint64 = 1000
	const slotHeight uint64 = 2000

	bonds, _ := ReadCommitteeBonds(reader, []string{"hive:alice"}, epochStartBh, slotHeight)

	if got, present := bonds["hive:alice"]; present {
		t.Fatalf("audit #122: expected flash-staker to be omitted (min-bond = 0), got %d", got)
	}

	// Sanity: the stub returns 1,000,000 if asked only at slotHeight, so
	// a regression that drops the window-aware sampling would resurface
	// the flash-stake.
	preRec, _ := reader.GetBalanceRecord("hive:alice", slotHeight)
	if preRec == nil || preRec.HIVE_CONSENSUS != 1_000_000 {
		t.Fatalf("audit #122: stub precondition wrong; at-slot record should be 1M, got %+v", preRec)
	}
	if preRec2, _ := reader.GetBalanceRecord("hive:alice", epochStartBh+1); preRec2 == nil || preRec2.HIVE_CONSENSUS != 0 {
		t.Fatalf("audit #122: stub precondition wrong; pre-flash record should be 0, got %+v", preRec2)
	}
}

// TestAuditFix_122_BondReaderHonestStakerKeepsBond ensures a witness that
// was bonded throughout the window keeps full weight — the TWAB-style min
// only discounts capital that wasn't actually at risk for the epoch.
func TestAuditFix_122_BondReaderHonestStakerKeepsBond(t *testing.T) {
	reader := &auditFlashStakeReader{
		flashAccount:    "hive:alice",
		preFlashBalance: 1_000_000,
		flashBalance:    1_000_000,
		flashBlock:      0, // bonded since before the window
	}
	const epochStartBh uint64 = 1000
	const slotHeight uint64 = 2000
	bonds, _ := ReadCommitteeBonds(reader, []string{"hive:alice"}, epochStartBh, slotHeight)
	if got := bonds["hive:alice"]; got != 1_000_000 {
		t.Fatalf("audit #122: honest staker must keep full bond, got %d", got)
	}
}

// auditFlashStakeReader is the minimum BalanceRecordReader surface needed to
// demonstrate the point-in-time read. Flash-stake semantics: the account has
// balance preFlashBalance at every block strictly before flashBlock and
// flashBalance at flashBlock and after. Any TWAB-aware reader would have to
// integrate across the queried range — this reader does not, mirroring the
// production code path.
type auditFlashStakeReader struct {
	flashAccount    string
	preFlashBalance int64
	flashBalance    int64
	flashBlock      uint64
}

func (r *auditFlashStakeReader) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	if account != r.flashAccount {
		return nil, nil
	}
	bal := r.preFlashBalance
	if blockHeight >= r.flashBlock {
		bal = r.flashBalance
	}
	return &ledgerDb.BalanceRecord{
		Account:        account,
		BlockHeight:    blockHeight,
		HIVE_CONSENSUS: bal,
	}, nil
}
