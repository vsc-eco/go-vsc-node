package safetyslash

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeClaim(id, victim, slashTx, slashed string, loss int64) RestitutionClaimRecord {
	return RestitutionClaimRecord{
		ClaimID:        id,
		VictimAccount:  victim,
		SlashTxID:      slashTx,
		SlashedAccount: slashed,
		EvidenceKind:   "vsc_double_block_sign",
		LossHive:       loss,
	}
}

func makeReverse(slashTx, slashed string, action SafetySlashReverseAction, amt int64) SafetySlashReverseRecord {
	return SafetySlashReverseRecord{
		SlashTxID:      slashTx,
		EvidenceKind:   "vsc_double_block_sign",
		SlashedAccount: slashed,
		Action:         action,
		Amount:         amt,
	}
}

func TestMemoryPendingGovOps_EnqueueClaimsFIFO(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	require.True(t, p.EnqueueRestitutionClaim(makeClaim("c1", "alice", "tx1", "validator1", 100)))
	require.True(t, p.EnqueueRestitutionClaim(makeClaim("c2", "bob", "tx1", "validator1", 200)))
	require.True(t, p.EnqueueRestitutionClaim(makeClaim("c3", "carol", "tx1", "validator1", 300)))

	claims := p.DrainRestitutionClaims(0)
	require.Len(t, claims, 3)
	require.Equal(t, "c1", claims[0].ClaimID)
	require.Equal(t, "c2", claims[1].ClaimID)
	require.Equal(t, "c3", claims[2].ClaimID)

	// Pool drained.
	require.Empty(t, p.DrainRestitutionClaims(0))
	c, _ := p.PendingCounts()
	require.Equal(t, 0, c)
}

func TestMemoryPendingGovOps_DrainPartial(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	for i := 0; i < 5; i++ {
		require.True(t, p.EnqueueRestitutionClaim(
			makeClaim("c"+strconv.Itoa(i), "victim", "tx", "validator", int64(i+1)*10),
		))
	}
	first := p.DrainRestitutionClaims(2)
	require.Len(t, first, 2)
	require.Equal(t, "c0", first[0].ClaimID)
	require.Equal(t, "c1", first[1].ClaimID)

	// Remaining 3 still in FIFO order.
	rest := p.DrainRestitutionClaims(0)
	require.Len(t, rest, 3)
	require.Equal(t, "c2", rest[0].ClaimID)
	require.Equal(t, "c4", rest[2].ClaimID)
}

func TestMemoryPendingGovOps_ClaimIDIdempotent(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	require.True(t, p.EnqueueRestitutionClaim(makeClaim("dup", "alice", "tx", "validator", 100)))
	require.True(t, p.EnqueueRestitutionClaim(makeClaim("dup", "alice", "tx", "validator", 250))) // replace

	claims := p.DrainRestitutionClaims(0)
	require.Len(t, claims, 1, "duplicate ClaimID must not inflate pool")
	require.Equal(t, int64(250), claims[0].LossHive, "later submission must overwrite earlier one")
}

func TestMemoryPendingGovOps_EnqueueRejectsBadShape(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	cases := []RestitutionClaimRecord{
		makeClaim("", "alice", "tx", "validator", 100),       // missing claim id
		makeClaim("c", "", "tx", "validator", 100),           // missing victim
		makeClaim("c", "alice", "", "validator", 100),        // missing slash tx
		makeClaim("c", "alice", "tx", "", 100),               // missing slashed account
		makeClaim("c", "alice", "tx", "validator", 0),        // non-positive loss
		makeClaim("c", "alice", "tx", "validator", -1),       // negative loss
		{ClaimID: "c", VictimAccount: "a", SlashTxID: "tx",   // missing evidence kind
			SlashedAccount: "validator", LossHive: 50},
	}
	for i, bad := range cases {
		require.Falsef(t, p.EnqueueRestitutionClaim(bad), "case %d should be rejected", i)
	}
	c, _ := p.PendingCounts()
	require.Equal(t, 0, c)
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
}

func TestMemoryPendingGovOps_ReverseRejectsBadShape(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	bad := []SafetySlashReverseRecord{
		makeReverse("", "v1", ReverseActionCancel, 0),                // missing tx
		makeReverse("tx", "", ReverseActionCancel, 0),                // missing slashed
		{SlashTxID: "tx", EvidenceKind: "", SlashedAccount: "v1",     // missing kind
			Action: ReverseActionCancel},
		makeReverse("tx", "v1", "", 0),                               // missing action
		makeReverse("tx", "v1", "frobnicate", 0),                     // unknown action
	}
	for i, b := range bad {
		require.Falsef(t, p.EnqueueSlashReverse(b), "case %d should be rejected", i)
	}
	_, r := p.PendingCounts()
	require.Equal(t, 0, r)
}

func TestMemoryPendingGovOps_NormalizeAtEnqueue(t *testing.T) {
	p := NewMemoryPendingGovernanceOps()
	require.True(t, p.EnqueueRestitutionClaim(RestitutionClaimRecord{
		ClaimID:        "  c1  ",
		VictimAccount:  "alice", // no hive: prefix
		SlashTxID:      "tx1",
		SlashedAccount: "validator1", // no hive: prefix
		EvidenceKind:   " vsc_double_block_sign ",
		LossHive:       100,
	}))
	got := p.DrainRestitutionClaims(0)
	require.Len(t, got, 1)
	require.Equal(t, "c1", got[0].ClaimID, "trim claim id")
	require.Equal(t, "hive:alice", got[0].VictimAccount, "victim must be hive: prefixed")
	require.Equal(t, "hive:validator1", got[0].SlashedAccount, "slashed must be hive: prefixed")
	require.Equal(t, "vsc_double_block_sign", got[0].EvidenceKind, "trim evidence kind")
}

func TestMemoryPendingGovOps_ConcurrentEnqueueDrain(t *testing.T) {
	// Smoke test the mutex: 50 producers enqueue a unique claim each
	// while 4 drainers race. Assert every claim either lands in the
	// pool or in some drainer's batch — total = 50, no dupes.
	p := NewMemoryPendingGovernanceOps()
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.EnqueueRestitutionClaim(makeClaim(
				"id-"+strconv.Itoa(i), "v", "tx", "validator", int64(i+1)))
		}(i)
	}
	collected := make([]RestitutionClaimRecord, 0, N)
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := p.DrainRestitutionClaims(7)
			mu.Lock()
			collected = append(collected, batch...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	collected = append(collected, p.DrainRestitutionClaims(0)...)
	require.Len(t, collected, N, "no claim should be lost or duplicated")
	seen := make(map[string]bool, N)
	for _, c := range collected {
		require.False(t, seen[c.ClaimID], "claim %s appeared twice", c.ClaimID)
		seen[c.ClaimID] = true
	}
}
