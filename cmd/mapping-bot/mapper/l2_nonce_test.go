package mapper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vsc-node/modules/common"
	transactionpool "vsc-node/modules/transaction-pool"
)

// poolGraphQL answers the account nonce and submissions as the node does
// (transactionpool.IngestTx, the block producer, InvalidateCompetingTransactions):
// the account nonce moves only when produce() includes transactions; a nonce
// below it, or above it with nothing pending at the one before, is refused;
// produce() includes one transaction per nonce and deletes the rest.
type poolGraphQL struct {
	mockGraphQL
	pmu       sync.Mutex
	confirmed uint64
	pending   map[uint64][]string // nonce -> tx ids, in submission order
	status    map[string]string
	nonces    []uint64 // the nonce of each submission
}

func newPoolGraphQL() *poolGraphQL {
	return &poolGraphQL{pending: map[uint64][]string{}, status: map[string]string{}}
}

func (p *poolGraphQL) FetchAccountNonce(ctx context.Context, account string) (uint64, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.confirmed, nil
}

func (p *poolGraphQL) SubmitTransactionV1(ctx context.Context, txB64, sigB64 string) (string, error) {
	raw, err := base64.URLEncoding.DecodeString(txB64)
	if err != nil {
		return "", err
	}
	var shell transactionpool.VSCTransactionShell
	if err := common.DecodeCbor(raw, &shell); err != nil {
		return "", err
	}
	n := shell.Headers.Nonce
	p.pmu.Lock()
	defer p.pmu.Unlock()
	switch {
	case n < p.confirmed:
		return "", fmt.Errorf("graphql error: nonce too low: %d < %d", n, p.confirmed)
	case n > p.confirmed && len(p.pending[n-1]) == 0:
		return "", fmt.Errorf("graphql error: missing previous nonce %d in transaction pool", n-1)
	}
	id := fmt.Sprintf("l2-%d", len(p.nonces))
	p.nonces = append(p.nonces, n)
	p.pending[n] = append(p.pending[n], id)
	p.status[id] = "UNCONFIRMED"
	return id, nil
}

func (p *poolGraphQL) FetchTransactionStatus(ctx context.Context, txId string) (string, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if s, ok := p.status[txId]; ok {
		return s, nil
	}
	return "", fmt.Errorf("transaction %s not found", txId)
}

func (p *poolGraphQL) produce() {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	for ids := p.pending[p.confirmed]; len(ids) > 0; ids = p.pending[p.confirmed] {
		p.status[ids[0]] = "CONFIRMED"
		for _, lost := range ids[1:] {
			delete(p.status, lost) // the node deletes the transactions that lost the nonce
		}
		delete(p.pending, p.confirmed)
		p.confirmed++
	}
}

// A map and a confirmSpend before the next block (HandleMap and
// HandleConfirmations run at the same time) both took the account nonce, and
// the node deleted one of them. They now take consecutive nonces, and both
// are included.
func TestL2CallsBeforeABlockTakeConsecutiveNonces(t *testing.T) {
	gql := newPoolGraphQL()
	bot, _ := buildBotForL2Test(t, gql)
	ctx := context.Background()
	a, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"a"}`), "map")
	if err != nil {
		t.Fatal(err)
	}
	b, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"b"}`), "confirmSpend")
	if err != nil {
		t.Fatal(err)
	}
	if len(gql.nonces) != 2 || gql.nonces[0] != 0 || gql.nonces[1] != 1 {
		t.Fatalf("nonces %v, want [0 1]", gql.nonces)
	}
	gql.produce()
	for _, id := range []string{a, b} {
		if s, _ := gql.FetchTransactionStatus(ctx, id); s != "CONFIRMED" {
			t.Fatalf("%s: %q after the block, want CONFIRMED (the node deleted it)", id, s)
		}
	}
}

// When the node lacks the bot's previous transaction, the bot keeps its nonce
// through l2NonceHoleGrace (gossip from another node), then refills from the
// account nonce.
func TestL2NonceHoleIsRefilledAfterTheGrace(t *testing.T) {
	gql := newPoolGraphQL()
	bot, _ := buildBotForL2Test(t, gql)
	ctx := context.Background()
	if _, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"a"}`), "map"); err != nil {
		t.Fatal(err)
	}
	gql.pmu.Lock()
	gql.pending = map[uint64][]string{} // the node lost nonce 0
	gql.pmu.Unlock()
	if _, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"b"}`), "map"); err == nil || !strings.Contains(err.Error(), "missing previous nonce") {
		t.Fatalf("inside the grace: %v, want the missing-previous-nonce refusal", err)
	}
	old := l2NonceHoleGrace
	l2NonceHoleGrace = 0
	t.Cleanup(func() { l2NonceHoleGrace = old })
	time.Sleep(time.Millisecond)
	if _, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"b"}`), "map"); err == nil {
		t.Fatal("past the grace the refusal resets the nonce; the call itself still fails")
	}
	id, err := bot.callContractL2(ctx, json.RawMessage(`{"tx_data":"b"}`), "map")
	if err != nil {
		t.Fatal(err)
	}
	if last := gql.nonces[len(gql.nonces)-1]; last != 0 {
		t.Fatalf("nonces %v, want the refill at 0", gql.nonces)
	}
	gql.produce()
	if s, _ := gql.FetchTransactionStatus(ctx, id); s != "CONFIRMED" {
		t.Fatalf("refilled call: %q", s)
	}
}
