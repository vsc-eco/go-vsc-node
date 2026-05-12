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
	// BlockTypeRestitutionClaim carries a RestitutionClaimRecord into a VSC
	// block. The 2/3 BLS aggregate over the carrying block authorises the
	// claim (witnesses gate inclusion); the state engine independently
	// validates that the referenced safety_slash_consensus row exists and,
	// when VictimTxID is supplied, that the harmed transaction was anchored
	// at the slashed slot. Calls LedgerSystem.EnqueueRestitutionClaim.
	BlockTypeRestitutionClaim
	// BlockTypeSafetySlashReverse carries a SafetySlashReverseRecord into a
	// VSC block. The 2/3 BLS aggregate over the carrying block authorises
	// the cancel/reverse decision (witness supermajority is the timelock,
	// per the no-explicit-window auth model). The state engine validates
	// that the referenced safety_slash_consensus row exists and that the
	// requested action does not exceed the unfinalized portion before
	// invoking CancelPendingSafetySlashBurn / ReverseSafetySlashConsensusDebit.
	BlockTypeSafetySlashReverse
)
