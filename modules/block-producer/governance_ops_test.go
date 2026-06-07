package blockproducer

import (
	"testing"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"

	"github.com/stretchr/testify/require"
)

// makeReverseRec is a test helper that builds a SafetySlashReverseRecord.
func makeReverseRec(slashTx, slashed string, action safetyslash.SafetySlashReverseAction, amt int64) safetyslash.SafetySlashReverseRecord {
	return safetyslash.SafetySlashReverseRecord{
		SlashTxID:      slashTx,
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: slashed,
		Action:         action,
		Amount:         amt,
	}
}

// newTestProducer wires the minimum BlockProducer surface that the
// MakeSafetySlashReverses method touches — the producer hook is fully
// decoupled from the rest of the producer state (no DB reads, no RC,
// no oplog wiring), so a near-empty struct is sufficient for unit tests.
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

func TestMakeSafetySlashReverses_ReturnsNilOnEmptyPool(t *testing.T) {
	bp := newTestProducer()
	require.Nil(t, bp.MakeSafetySlashReverses(emptySession()))
}

func TestMakeSafetySlashReverses_ComposesEachAction(t *testing.T) {
	bp := newTestProducer()
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(
		makeReverseRec("tx1", "validator", safetyslash.ReverseActionCancel, 0)))
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(
		makeReverseRec("tx1", "validator", safetyslash.ReverseActionReverse, 500)))
	require.True(t, bp.PendingGovOps.EnqueueSlashReverse(
		makeReverseRec("tx2", "validator", safetyslash.ReverseActionBoth, 1000)))

	out := bp.MakeSafetySlashReverses(emptySession())
	require.Len(t, out, 3)
	for _, tx := range out {
		require.Equal(t, int(common.BlockTypeSafetySlashReverse), tx.Type,
			"every entry must be tagged BlockTypeSafetySlashReverse")
		require.NotEmpty(t, tx.Id, "CID must be populated")
	}
	require.NotEqual(t, out[0].Id, out[1].Id,
		"distinct reverse payloads must hash to distinct CIDs")

	// Pool drained.
	require.Equal(t, 0, bp.PendingGovOps.PendingCounts())
}

func TestMakeSafetySlashReverses_RespectsBatchCap(t *testing.T) {
	bp := newTestProducer()
	for i := 0; i < maxGovernanceOpsPerBlock+5; i++ {
		require.True(t, bp.PendingGovOps.EnqueueSlashReverse(makeReverseRec(
			"tx-"+itoa(i), "validator", safetyslash.ReverseActionBoth, int64(i+1))))
	}
	out := bp.MakeSafetySlashReverses(emptySession())
	require.Len(t, out, maxGovernanceOpsPerBlock,
		"the producer must cap inclusion at maxGovernanceOpsPerBlock")
	require.Equal(t, 5, bp.PendingGovOps.PendingCounts(),
		"remaining reverses must stay queued for next slot")
}

func TestMakeSafetySlashReverses_DeterministicCID(t *testing.T) {
	// Two producers with identical pools must hash identical bytes.
	bp1 := newTestProducer()
	bp2 := newTestProducer()
	rec := makeReverseRec("same-tx", "validator", safetyslash.ReverseActionBoth, 4242)
	require.True(t, bp1.PendingGovOps.EnqueueSlashReverse(rec))
	require.True(t, bp2.PendingGovOps.EnqueueSlashReverse(rec))

	out1 := bp1.MakeSafetySlashReverses(emptySession())
	out2 := bp2.MakeSafetySlashReverses(emptySession())
	require.Len(t, out1, 1)
	require.Len(t, out2, 1)
	require.Equal(t, out1[0].Id, out2[0].Id,
		"identical payloads must produce byte-identical DAG-CBOR and identical CIDs")
}

func TestMakeSafetySlashReverses_NoPanicWithNilPool(t *testing.T) {
	// Defensive: a producer constructed without going through New()
	// (e.g. test fixtures from older code) must not crash if the
	// hook fires. The method short-circuits on nil pool / nil session.
	bp := &BlockProducer{}
	require.Nil(t, bp.MakeSafetySlashReverses(emptySession()))

	bp2 := newTestProducer()
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
