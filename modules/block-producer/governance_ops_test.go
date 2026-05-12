package blockproducer

import (
	"testing"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"

	"github.com/stretchr/testify/require"
)

// makeClaimRec is a test helper that builds a RestitutionClaimRecord.
func makeClaimRec(claimID, victim, slashTx, slashed string, loss int64) safetyslash.RestitutionClaimRecord {
	return safetyslash.RestitutionClaimRecord{
		ClaimID:        claimID,
		VictimAccount:  victim,
		SlashTxID:      slashTx,
		SlashedAccount: slashed,
		EvidenceKind:   "vsc_double_block_sign",
		LossHive:       loss,
	}
}

// newTestProducer wires the minimum BlockProducer surface that the
// MakeRestitutionClaims / MakeSafetySlashReverses methods touch — the
// producer hooks are fully decoupled from the rest of the producer
// state (no DB reads, no RC, no oplog wiring), so a near-empty struct
// is sufficient for unit tests.
func newTestProducer() *BlockProducer {
	return &BlockProducer{
		PendingGovOps: safetyslash.NewMemoryPendingGovernanceOps(),
	}
}

// emptySession returns a datalayer.Session that accepts Put calls
// without persisting anything. Tests inspect Put calls indirectly by
// observing the returned VscBlockTx CIDs.
func emptySession() *datalayer.Session {
	return datalayer.NewSession(nil)
}

func TestMakeRestitutionClaims_ReturnsNilOnEmptyPool(t *testing.T) {
	bp := newTestProducer()
	require.Nil(t, bp.MakeRestitutionClaims(emptySession()))
}

func TestMakeRestitutionClaims_ComposesAndAssignsBlockType(t *testing.T) {
	bp := newTestProducer()
	require.True(t, bp.PendingGovOps.EnqueueRestitutionClaim(
		makeClaimRec("c1", "alice", "tx1", "validator", 1000)))
	require.True(t, bp.PendingGovOps.EnqueueRestitutionClaim(
		makeClaimRec("c2", "bob", "tx1", "validator", 2000)))

	out := bp.MakeRestitutionClaims(emptySession())
	require.Len(t, out, 2)
	for _, tx := range out {
		require.Equal(t, int(common.BlockTypeRestitutionClaim), tx.Type,
			"every entry must be tagged BlockTypeRestitutionClaim")
		require.NotEmpty(t, tx.Id, "CID must be populated")
	}
	require.NotEqual(t, out[0].Id, out[1].Id,
		"distinct claim payloads must hash to distinct CIDs")

	// Pool drained.
	c, _ := bp.PendingGovOps.PendingCounts()
	require.Equal(t, 0, c)
}

func TestMakeRestitutionClaims_RespectsBatchCap(t *testing.T) {
	bp := newTestProducer()
	for i := 0; i < maxGovernanceOpsPerBlock+5; i++ {
		require.True(t, bp.PendingGovOps.EnqueueRestitutionClaim(makeClaimRec(
			"id-"+itoa(i), "victim", "tx", "validator", int64(i+1))))
	}
	out := bp.MakeRestitutionClaims(emptySession())
	require.Len(t, out, maxGovernanceOpsPerBlock,
		"the producer must cap inclusion at maxGovernanceOpsPerBlock")
	c, _ := bp.PendingGovOps.PendingCounts()
	require.Equal(t, 5, c, "remaining claims must stay queued for next slot")
}

func TestMakeRestitutionClaims_DeterministicCID(t *testing.T) {
	// Two producers with identical pools must hash identical bytes.
	bp1 := newTestProducer()
	bp2 := newTestProducer()
	rec := makeClaimRec("same", "alice", "tx", "validator", 4242)
	require.True(t, bp1.PendingGovOps.EnqueueRestitutionClaim(rec))
	require.True(t, bp2.PendingGovOps.EnqueueRestitutionClaim(rec))

	out1 := bp1.MakeRestitutionClaims(emptySession())
	out2 := bp2.MakeRestitutionClaims(emptySession())
	require.Len(t, out1, 1)
	require.Len(t, out2, 1)
	require.Equal(t, out1[0].Id, out2[0].Id,
		"identical payloads must produce byte-identical DAG-CBOR and identical CIDs")
}

func TestMakeSafetySlashReverses_ReturnsNilOnEmptyPool(t *testing.T) {
	bp := newTestProducer()
	require.Nil(t, bp.MakeSafetySlashReverses(emptySession()))
}

func TestMakeSafetySlashReverses_ComposesEachAction(t *testing.T) {
	bp := newTestProducer()
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "tx1",
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: "validator",
		Action:         safetyslash.ReverseActionCancel,
	}))
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "tx1",
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: "validator",
		Action:         safetyslash.ReverseActionReverse,
		Amount:         500,
	}))
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "tx2",
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: "validator",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         1000,
	}))

	out := bp.MakeSafetySlashReverses(emptySession())
	require.Len(t, out, 3)
	for _, tx := range out {
		require.Equal(t, int(common.BlockTypeSafetySlashReverse), tx.Type)
		require.NotEmpty(t, tx.Id)
	}
}

func TestMakeRestitutionClaims_NoPanicWithNilPool(t *testing.T) {
	// Defensive: a producer constructed without going through New()
	// (e.g. test fixtures from older code) must not crash if the
	// hooks fire. The methods short-circuit on nil pool / nil
	// session.
	bp := &BlockProducer{}
	require.Nil(t, bp.MakeRestitutionClaims(emptySession()))
	require.Nil(t, bp.MakeSafetySlashReverses(emptySession()))

	bp2 := newTestProducer()
	require.Nil(t, bp2.MakeRestitutionClaims(nil))
	require.Nil(t, bp2.MakeSafetySlashReverses(nil))
}

// itoa is a tiny helper to keep imports lean in this test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
