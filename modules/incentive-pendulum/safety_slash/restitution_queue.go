package safetyslash

import (
	"strings"
	"sync"

	ledgerSystem "vsc-node/modules/ledger-system"
)

// MemoryRestitutionQueue is a FIFO allocator for safety-slash proceeds.
// It is safe for concurrent Enqueue and AllocateHive (same block executor).
//
// Consensus warning: queue contents are not replicated by the chain. Every node
// must see the same Enqueue ordering or SafetySlashConsensusBond will diverge.
// Do not call Enqueue from local/off-chain paths until claims are backed by
// consensus-visible ops (or ledger-derived replay).
type MemoryRestitutionQueue struct {
	mu    sync.Mutex
	queue []ledgerSystem.SlashRestitutionClaim
}

func NewMemoryRestitutionQueue() *MemoryRestitutionQueue {
	return &MemoryRestitutionQueue{queue: make([]ledgerSystem.SlashRestitutionClaim, 0)}
}

func normalizeRestitutionVictim(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "hive:") {
		return a
	}
	return "hive:" + a
}

// Enqueue appends a claim; LossHive is the remaining amount still owed.
func (q *MemoryRestitutionQueue) Enqueue(c ledgerSystem.SlashRestitutionClaim) {
	if q == nil || c.LossHive <= 0 {
		return
	}
	v := normalizeRestitutionVictim(c.VictimAccount)
	if v == "" {
		return
	}
	c.VictimAccount = v
	q.mu.Lock()
	q.queue = append(q.queue, c)
	q.mu.Unlock()
}

// AllocateHive implements ledgerSystem.SlashRestitutionAllocator.
func (q *MemoryRestitutionQueue) AllocateHive(slashAmt int64, blockHeight uint64, txID, evidenceKind, slashedAccount string) ([]ledgerSystem.SlashRestitutionPayment, int64) {
	_ = blockHeight
	_ = txID
	_ = evidenceKind
	_ = slashedAccount
	if q == nil || slashAmt <= 0 {
		return nil, slashAmt
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	remaining := slashAmt
	var out []ledgerSystem.SlashRestitutionPayment
	i := 0
	for remaining > 0 && i < len(q.queue) {
		c := &q.queue[i]
		if c.LossHive <= 0 {
			i++
			continue
		}
		victim := normalizeRestitutionVictim(c.VictimAccount)
		if victim == "" {
			i++
			continue
		}
		pay := remaining
		if c.LossHive < pay {
			pay = c.LossHive
		}
		out = append(out, ledgerSystem.SlashRestitutionPayment{
			ClaimID:       c.ClaimID,
			VictimAccount: victim,
			Amount:        pay,
		})
		c.LossHive -= pay
		remaining -= pay
		if c.LossHive == 0 {
			i++
		}
	}
	write := 0
	for j := 0; j < len(q.queue); j++ {
		if q.queue[j].LossHive > 0 {
			q.queue[write] = q.queue[j]
			write++
		}
	}
	q.queue = q.queue[:write]
	return out, remaining
}
