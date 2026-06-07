package common

var CONSENSUS_SPECS = struct {
	SlotLength     uint64
	EpochLength    uint64
	ScheduleLength uint64
}{
	EpochLength:    7_200, // 6 hours - length between elections
	SlotLength:     10,    //10 * 3 seconds = 30 seconds - length between blocks
	ScheduleLength: 1_200, // 60 * 20 = 1 hour - length of a schedule before it's recalcualted
}

const HBD_UNSTAKE_BLOCKS = uint64(86400)

type BLOCKTXTYPE int

const (
	BlockTypeNull BLOCKTXTYPE = iota
	BlockTypeTransaction
	BlockTypeOutput
	BlockTypeAnchor
	BlockTypeOplog
	BlockTypeRcUpdate
	BlockTypePendulumSettlement
	// Tombstone: was BlockTypeRestitutionClaim (victim restitution, removed —
	// VSC safety slashing only fires on block-production/signing faults, which
	// have no fund victim: the offending block's txs never execute, so there is
	// never a loss to restitute). The slot is retained so the value of
	// BlockTypeSafetySlashReverse below stays stable; do not reuse it.
	_
	// BlockTypeSafetySlashReverse carries a SafetySlashReverseRecord into a
	// VSC block. The 2/3 BLS aggregate over the carrying block authorises
	// the cancel/reverse decision (witness supermajority is the timelock,
	// per the no-explicit-window auth model). The state engine validates
	// that the referenced safety_slash_consensus row exists and that the
	// requested action does not exceed the unfinalized portion before
	// invoking CancelPendingSafetySlashBurn / ReverseSafetySlashConsensusDebit.
	BlockTypeSafetySlashReverse
)
