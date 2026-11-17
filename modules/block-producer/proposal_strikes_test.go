package blockproducer

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
)

func txRec(id, account string, nonce int64) transactions.TransactionRecord {
	return transactions.TransactionRecord{Id: id, RequiredAuths: []string{account}, Nonce: nonce}
}

func ids(recs []transactions.TransactionRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Id)
	}
	return out
}

func sameIds(got []transactions.TransactionRecord, want ...string) bool {
	g := ids(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// LOAD-GOSSIP-1 (testnet 11-17): our node held 197 transactions its peers never
// got; every proposal carried them, no follower could sign, and it produced no
// block until the records were deleted by hand. After two unsigned proposals a
// transaction is left out of this node's proposals, with the rest of its
// account's nonce chain; other accounts and earlier nonces go on.
func TestProposalStrikes_TwoUnsignedProposalsLeaveATransactionOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := proposalStrikes{now: func() time.Time { return now }}
	pool := []transactions.TransactionRecord{
		txRec("a4", "did:a", 4), txRec("a5", "did:a", 5), txRec("a6", "did:a", 6), txRec("b1", "did:b", 1),
	}
	if got, left := s.leaveOut(pool); left != 0 || len(got) != 4 {
		t.Fatalf("no strikes: kept %v, left out %d; want all four", ids(got), left)
	}
	// a4 confirmed in between; the peers lack a5: two proposals carrying a5, a6
	// and b1 go unsigned.
	carried := []string{"a5", "a6", "b1"}
	s.unsigned(carried)
	if got, left := s.leaveOut(pool); left != 0 || len(got) != 4 {
		t.Fatalf("one strike: kept %v, left out %d; want all four (one unsigned proposal proves nothing)", ids(got), left)
	}
	s.unsigned(carried)
	got, left := s.leaveOut(pool)
	if left != 3 || !sameIds(got, "a4") {
		t.Fatalf("two strikes: kept %v, left out %d; want only a4", ids(got), left)
	}
	// b1 gets through in another producer's block and its proposal is signed
	// there: here it is simply no longer pending. a5 stays out and takes a6
	// with it; a new transaction of another account is proposed.
	pool = []transactions.TransactionRecord{txRec("a5", "did:a", 5), txRec("a6", "did:a", 6), txRec("c1", "did:c", 1)}
	got, left = s.leaveOut(pool)
	if left != 2 || !sameIds(got, "c1") {
		t.Fatalf("kept %v, left out %d; want only c1", ids(got), left)
	}
	if s.heldBack("b1") || len(s.txs) != 2 {
		t.Fatalf("strike book %v, want only a5 and a6 (b1 is no longer pending)", s.txs)
	}
}

// A struck transaction is proposed again after its wait, so a good one that got
// its strikes for another reason is not held for ever. If it goes unsigned again
// the wait doubles, up to proposalStrikeMaxWait: a node that holds transactions
// nobody else has loses a slot per wait, not every slot.
func TestProposalStrikes_ProposedAgainAfterTheWaitWhichDoubles(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := proposalStrikes{now: func() time.Time { return now }}
	pool := []transactions.TransactionRecord{txRec("x", "did:a", 1)}
	s.unsigned([]string{"x"})
	s.unsigned([]string{"x"})
	wait := proposalStrikeFirstWait
	for round := 0; round < 8; round++ {
		now = now.Add(wait - time.Second)
		if _, left := s.leaveOut(pool); left != 1 {
			t.Fatalf("round %d: proposed again %v before its %v wait was over", round, time.Second, wait)
		}
		now = now.Add(time.Second)
		if _, left := s.leaveOut(pool); left != 0 {
			t.Fatalf("round %d: still left out after its %v wait", round, wait)
		}
		s.unsigned([]string{"x"}) // it went unsigned again
		if wait *= 2; wait > proposalStrikeMaxWait {
			wait = proposalStrikeMaxWait
		}
	}
	if wait != proposalStrikeMaxWait {
		t.Fatalf("the wait never reached its cap: %v", wait)
	}
}

// A signed proposal clears what it carried: one strike followed by a signed
// proposal starts from zero.
func TestProposalStrikes_ASignedProposalClears(t *testing.T) {
	s := proposalStrikes{}
	pool := []transactions.TransactionRecord{txRec("x", "did:a", 1)}
	s.unsigned([]string{"x"})
	s.signed([]string{"x"})
	s.unsigned([]string{"x"})
	if _, left := s.leaveOut(pool); left != 0 {
		t.Fatal("left out after one strike, a signed proposal and one more strike: the signed one must clear the first")
	}
}

type stubBlocks struct {
	vscBlocks.VscBlocks
	rec *vscBlocks.VscHeaderRecord
	err error
}

func (b stubBlocks) GetBlockByHeight(uint64) (*vscBlocks.VscHeaderRecord, error) { return b.rec, b.err }

// A strike counts only while the chain is producing. When nobody's proposals
// are signed (too many nodes down), the transactions are not what is wrong;
// striking them on every producer would hold good transactions back after the
// network returns.
func TestProposalStrikes_NoStrikeWhileTheChainIsNotProducing(t *testing.T) {
	slot := uint64(1000)
	span := proposalStrikeRecentSlots * common.CONSENSUS_SPECS.SlotLength
	for _, c := range []struct {
		name   string
		blocks vscBlocks.VscBlocks
		want   bool
	}{
		{"a block in the previous slot", stubBlocks{rec: &vscBlocks.VscHeaderRecord{SlotHeight: int(slot - common.CONSENSUS_SPECS.SlotLength)}}, true},
		{"the last block is exactly the span back", stubBlocks{rec: &vscBlocks.VscHeaderRecord{SlotHeight: int(slot - span)}}, true},
		{"the last block is older than the span", stubBlocks{rec: &vscBlocks.VscHeaderRecord{SlotHeight: int(slot - span - 1)}}, false},
		{"no block yet", stubBlocks{}, false},
		{"the block store cannot be read", stubBlocks{err: errors.New("db down")}, false},
		{"no block store", nil, false},
	} {
		bp := &BlockProducer{VscBlocks: c.blocks}
		if got := bp.chainProducedRecently(slot); got != c.want {
			t.Errorf("%s: chain producing = %v, want %v", c.name, got, c.want)
		}
		for i := 0; i < proposalStrikeLimit; i++ {
			bp.noteUnsignedProposal(slot, []string{"x"})
		}
		if got := bp.strikes.heldBack("x"); got != c.want {
			t.Errorf("%s: transaction held back = %v after %d unsigned proposals, want %v", c.name, got, proposalStrikeLimit, c.want)
		}
	}
	// a proposal without pool transactions has nothing to strike
	bp := &BlockProducer{VscBlocks: stubBlocks{rec: &vscBlocks.VscHeaderRecord{SlotHeight: int(slot)}}}
	bp.noteUnsignedProposal(slot, nil)
	if len(bp.strikes.txs) != 0 {
		t.Fatalf("strikes %v for a proposal without pool transactions", bp.strikes.txs)
	}
}

// The strike book only helps if the producer uses it: the pending transactions
// pass through leaveOut before sequencing, both unsigned outcomes of
// ProduceBlock record strikes, and a signed proposal clears. (A source check, as
// RT-13's in this package: these paths need a full node to run.)
func TestProposalStrikes_AreWiredIntoTheProducer(t *testing.T) {
	src, err := os.ReadFile("blockProducer.go")
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]int{
		"prefilteredTxs, leftOut := bp.strikes.leaveOut(prefilteredTxs)": 1,
		"bp.noteUnsignedProposal(bh, transactions)":                      2,
		"bp.strikes.signed(transactions)":                                1,
	} {
		if got := strings.Count(string(src), text); got != want {
			t.Errorf("blockProducer.go has %q %d time(s), want %d", text, got, want)
		}
	}
}
