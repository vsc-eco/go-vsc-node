package safetyslash

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeReverse(slashTx, slashed string, action SafetySlashReverseAction, amt int64) SafetySlashReverseRecord {
	return SafetySlashReverseRecord{
		SlashTxID:      slashTx,
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: slashed,
		Action:         action,
		Amount:         amt,
	}
}

func TestMemoryPendingGovOps_ReverseFIFOAndDedup(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	require.True(t, p.EnqueueSlashReverse(makeReverse("tx1", "v1", ReverseActionCancel, 0)))
	require.True(t, p.EnqueueSlashReverse(makeReverse("tx1", "v1", ReverseActionReverse, 1000)))
	// same (tx, kind, slashed, action) — dedupe and replace amount.
	require.True(t, p.EnqueueSlashReverse(makeReverse("tx1", "v1", ReverseActionReverse, 2500)))

	rev := p.DrainSlashReverses(0)
	require.Len(t, rev, 2)
	require.Equal(t, ReverseActionCancel, rev[0].Action)
	require.Equal(t, ReverseActionReverse, rev[1].Action)
	require.Equal(t, int64(2500), rev[1].Amount, "replaced amount must persist")

	// Pool drained.
	require.Empty(t, p.DrainSlashReverses(0))
	require.Equal(t, 0, p.PendingCounts())
}

func TestMemoryPendingGovOps_ReverseDrainPartial(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	for i := 0; i < 5; i++ {
		require.True(t, p.EnqueueSlashReverse(
			makeReverse("tx"+strconv.Itoa(i), "validator", ReverseActionBoth, int64(i+1)*10),
		))
	}
	first := p.DrainSlashReverses(2)
	require.Len(t, first, 2)
	require.Equal(t, "tx0", first[0].SlashTxID)
	require.Equal(t, "tx1", first[1].SlashTxID)

	// Remaining 3 still in FIFO order.
	rest := p.DrainSlashReverses(0)
	require.Len(t, rest, 3)
	require.Equal(t, "tx2", rest[0].SlashTxID)
	require.Equal(t, "tx4", rest[2].SlashTxID)
}

func TestMemoryPendingGovOps_ReverseRejectsBadShape(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	bad := []SafetySlashReverseRecord{
		makeReverse("", "v1", ReverseActionCancel, 0), // missing tx
		makeReverse("tx", "", ReverseActionCancel, 0), // missing slashed
		{SlashTxID: "tx", EvidenceKind: "", SlashedAccount: "v1", // missing kind
			Action: ReverseActionCancel},
		makeReverse("tx", "v1", "", 0),           // missing action
		makeReverse("tx", "v1", "frobnicate", 0), // unknown action
	}
	for i, b := range bad {
		require.Falsef(t, p.EnqueueSlashReverse(b), "case %d should be rejected", i)
	}
	require.Equal(t, 0, p.PendingCounts())
}

func TestMemoryPendingGovOps_ReverseNormalizeAtEnqueue(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	require.True(t, p.EnqueueSlashReverse(SafetySlashReverseRecord{
		SlashTxID:      "  tx1  ",
		EvidenceKind:   " vsc_double_block_sign ",
		SlashedAccount: "validator1", // no hive: prefix
		Action:         ReverseActionCancel,
	}))
	got := p.DrainSlashReverses(0)
	require.Len(t, got, 1)
	require.Equal(t, "tx1", got[0].SlashTxID, "trim slash tx id")
	require.Equal(t, "hive:validator1", got[0].SlashedAccount, "slashed must be hive: prefixed")
	require.Equal(t, "vsc_double_block_sign", got[0].EvidenceKind, "trim evidence kind")
}

func TestMemoryPendingGovOps_ConcurrentEnqueueDrain(t *testing.T) {
	// Smoke test the mutex: 50 producers enqueue a unique reverse each
	// (unique SlashTxID so dedup never collapses them) while 4 drainers
	// race. Assert every reverse either lands in the pool or in some
	// drainer's batch — total = 50, no dupes.
	p := NewMemoryPendingGovernanceOps()
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.EnqueueSlashReverse(makeReverse(
				"tx-"+strconv.Itoa(i), "validator", ReverseActionBoth, int64(i+1)))
		}(i)
	}
	collected := make([]SafetySlashReverseRecord, 0, N)
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := p.DrainSlashReverses(7)
			mu.Lock()
			collected = append(collected, batch...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	collected = append(collected, p.DrainSlashReverses(0)...)
	require.Len(t, collected, N, "no reverse should be lost or duplicated")
	seen := make(map[string]bool, N)
	for _, r := range collected {
		require.False(t, seen[r.SlashTxID], "reverse %s appeared twice", r.SlashTxID)
		seen[r.SlashTxID] = true
	}
}

func TestMemoryPendingGovOps_ReverseRejectsHashInjection(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	// "#" is the ledger row-Id delimiter; reject it in the id-forming fields.
	require.False(t, p.EnqueueSlashReverse(makeReverse("tx#evil", "v1", ReverseActionCancel, 0)),
		"# in SlashTxID must be rejected")
	require.False(t, p.EnqueueSlashReverse(SafetySlashReverseRecord{
		SlashTxID: "tx", EvidenceKind: "kind#evil", SlashedAccount: "v1",
		Action: ReverseActionCancel}),
		"# in EvidenceKind must be rejected")
	require.Equal(t, 0, p.PendingCounts())
}

func TestMemoryPendingGovOps_ReversePoolCapped(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	// Fill to the cap with dedup-evading distinct keys (vary SlashTxID).
	for i := 0; i < maxPendingReverses; i++ {
		require.True(t, p.EnqueueSlashReverse(
			makeReverse("tx"+strconv.Itoa(i), "v1", ReverseActionCancel, 0)))
	}
	require.Equal(t, maxPendingReverses, p.PendingCounts())
	// One past the cap is rejected; existing entries remain.
	require.False(t, p.EnqueueSlashReverse(
		makeReverse("tx-overflow", "v1", ReverseActionCancel, 0)),
		"enqueue past the cap must be rejected")
	require.Equal(t, maxPendingReverses, p.PendingCounts())
}
