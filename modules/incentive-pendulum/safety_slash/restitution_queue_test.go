package safetyslash

import (
	"testing"

	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/require"
)

func TestMemoryRestitutionQueue_FIFOAndPartial(t *testing.T) {
	q := NewMemoryRestitutionQueue()
	q.Enqueue(ledgerSystem.SlashRestitutionClaim{ClaimID: "c1", VictimAccount: "alice", LossHive: 30_000})
	q.Enqueue(ledgerSystem.SlashRestitutionClaim{ClaimID: "c2", VictimAccount: "bob", LossHive: 50_000})

	pay, burn := q.AllocateHive(30_000, 1, "tx", "k", "hive:slashed")
	require.Len(t, pay, 1)
	require.Equal(t, int64(30_000), pay[0].Amount)
	require.Equal(t, "hive:alice", pay[0].VictimAccount)
	require.Equal(t, int64(0), burn)

	// Second claim still has 50k; drain 40k to victims, 10k burn in one slash pool.
	pay2, burn2 := q.AllocateHive(40_000, 2, "tx2", "k", "hive:slashed")
	require.Len(t, pay2, 1)
	require.Equal(t, int64(40_000), pay2[0].Amount)
	require.Equal(t, "hive:bob", pay2[0].VictimAccount)
	require.Equal(t, int64(0), burn2)

	pay3, burn3 := q.AllocateHive(100_000, 3, "tx3", "k", "hive:slashed")
	require.Len(t, pay3, 1)
	require.Equal(t, int64(10_000), pay3[0].Amount)
	require.Equal(t, int64(90_000), burn3)
}

func TestMemoryRestitutionQueue_EnqueueIgnoresInvalid(t *testing.T) {
	q := NewMemoryRestitutionQueue()
	q.Enqueue(ledgerSystem.SlashRestitutionClaim{VictimAccount: "x", LossHive: 0})
	q.Enqueue(ledgerSystem.SlashRestitutionClaim{VictimAccount: "", LossHive: 100})
	pay, burn := q.AllocateHive(1_000, 0, "t", "e", "a")
	require.Empty(t, pay)
	require.Equal(t, int64(1_000), burn)
}
