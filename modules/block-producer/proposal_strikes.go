package blockproducer

import (
	"sync"
	"time"

	"vsc-node/modules/db/vsc/transactions"
	transactionpool "vsc-node/modules/transaction-pool"
)

// LOAD-GOSSIP-1 (testnet 11-17): a node whose pool holds transactions its peers
// never got lost every block it proposed. A follower signs a proposal only if
// it knows every transaction in it ("invalid transaction" otherwise), the
// producer puts every pending transaction it holds into every proposal, and
// nothing ever took such a transaction out of its pool: our node produced no
// block for hours, until the records were deleted by hand. Anyone can bring
// that about with a burst of transactions to one node's API (peers check the
// burst out of nonce order and drop most of it).
//
// The producer now protects itself. A pool transaction that this node carried
// in proposals that were not signed gets a strike per such proposal; with
// proposalStrikeLimit strikes it is left out of this node's proposals until a
// retry time, and so is the rest of that account's nonce chain behind it. The
// wait doubles with every further strike (proposalStrikeFirstWait, up to
// proposalStrikeMaxWait). A signed proposal clears the transactions it
// carried. Other producers are not affected: one that holds the transaction
// includes it.
//
// Local and in memory: it changes only which of its own pending transactions
// this node proposes, which a node with a smaller pool does today. Followers
// take the producer's list as it is.
//
// A strike counts only while the chain is producing blocks (see
// chainProducedRecently): when nobody's proposals are signed the transactions
// are not what is wrong, and striking them would hold good ones back on every
// producer once the network is back.
const (
	proposalStrikeLimit       = 2
	proposalStrikeFirstWait   = 5 * time.Minute
	proposalStrikeMaxWait     = time.Hour
	proposalStrikeRecentSlots = 3
)

type txStrike struct {
	n       int
	retryAt time.Time
}

// proposalStrikes is the zero-value-ready strike book of a block producer.
type proposalStrikes struct {
	mu  sync.Mutex
	txs map[string]txStrike
	now func() time.Time // nil: time.Now
}

func (s *proposalStrikes) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// unsigned records one strike for each pool transaction of a proposal that was
// not signed.
func (s *proposalStrikes) unsigned(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txs == nil {
		s.txs = map[string]txStrike{}
	}
	now := s.clock()
	for _, id := range ids {
		st := s.txs[id]
		st.n++
		if st.n >= proposalStrikeLimit {
			wait := proposalStrikeFirstWait
			for i := proposalStrikeLimit; i < st.n && wait < proposalStrikeMaxWait; i++ {
				wait *= 2
			}
			if wait > proposalStrikeMaxWait {
				wait = proposalStrikeMaxWait
			}
			st.retryAt = now.Add(wait)
		}
		s.txs[id] = st
	}
}

// signed clears the transactions a signed proposal carried.
func (s *proposalStrikes) signed(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.txs, id)
	}
}

// heldBack reports whether id is left out of this node's proposals now.
func (s *proposalStrikes) heldBack(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.txs[id]
	return ok && st.n >= proposalStrikeLimit && s.clock().Before(st.retryAt)
}

// leaveOut returns pending without the transactions that are held back and
// without the later nonces of their accounts (a nonce chain cannot be included
// past a gap), and how many it left out. It also forgets the strikes of
// transactions that are no longer pending.
func (s *proposalStrikes) leaveOut(pending []transactions.TransactionRecord) ([]transactions.TransactionRecord, int) {
	s.mu.Lock()
	if len(s.txs) == 0 {
		s.mu.Unlock()
		return pending, 0
	}
	now := s.clock()
	still := make(map[string]txStrike, len(s.txs))
	// the lowest held-back nonce per account
	gap := map[string]int64{}
	for _, rec := range pending {
		st, ok := s.txs[rec.Id]
		if !ok {
			continue
		}
		still[rec.Id] = st
		if st.n >= proposalStrikeLimit && now.Before(st.retryAt) {
			key := transactionpool.HashKeyAuths(rec.RequiredAuths)
			if n, seen := gap[key]; !seen || rec.Nonce < n {
				gap[key] = rec.Nonce
			}
		}
	}
	s.txs = still
	s.mu.Unlock()
	if len(gap) == 0 {
		return pending, 0
	}
	kept := make([]transactions.TransactionRecord, 0, len(pending))
	for _, rec := range pending {
		if n, held := gap[transactionpool.HashKeyAuths(rec.RequiredAuths)]; held && rec.Nonce >= n {
			continue
		}
		kept = append(kept, rec)
	}
	return kept, len(pending) - len(kept)
}
