package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// rcptCounter forwards JSON-RPC to target and counts the calls per method.
type rcptCounter struct {
	target string
	mu     sync.Mutex
	calls  map[string]int
}

func (p *rcptCounter) url(t *testing.T) string {
	t.Helper()
	p.calls = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		p.mu.Lock()
		p.calls[req.Method]++
		p.mu.Unlock()
		resp, err := http.Post(p.target, "application/json", bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// SCAN-AMPLIFY-1 (audit 11-18): one transaction carrying n Transfer logs to the
// vault is n deposits, but its receipt is downloaded once per block scan, not
// once per log (400 logs cost 400 downloads, 96 MB, before).
func TestSCANAMPLIFY1_OneReceiptDownloadPerTransaction(t *testing.T) {
	const n = 60
	key, _ := ethCrypto.GenerateKey()
	sender := ethCrypto.PubkeyToAddress(key.PublicKey)
	vault := common.HexToAddress("0x00000000000000000000000000000000000ab1e5")
	token := common.HexToAddress("0x00000000000000000000000000000000000000c0")
	tr := common.HexToHash(TransferEventSig)
	logs := make([]*types.Log, n)
	for i := range logs {
		logs[i] = &types.Log{Address: token, Topics: []common.Hash{tr, common.BytesToHash(sender.Bytes()), common.BytesToHash(vault.Bytes())},
			Data: common.LeftPadBytes(big.NewInt(int64(1_000_000+i)).Bytes(), 32)}
	}
	c := &fakeChain{}
	c.addTx(t, key, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 30_000_000, To: &token, Value: big.NewInt(0)}, logs...)
	p := &rcptCounter{target: c.rpc(t).urls[0]}
	rpc := newEthRPC(p.url(t))
	res, err := scanBlockSafe(rpc, 101, []string{"0x00000000000000000000000000000000000ab1e5"}, map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deposits) != n {
		t.Fatalf("%d deposits found, want %d", len(res.Deposits), n)
	}
	seen := map[int]bool{}
	for _, d := range res.Deposits {
		if seen[d.LogIndex] {
			t.Fatalf("two deposits share per-receipt log index %d", d.LogIndex)
		}
		seen[d.LogIndex] = true
	}
	if got := p.calls["eth_getTransactionReceipt"]; got != 1 {
		t.Fatalf("%d receipt downloads for one transaction's %d logs, want 1", got, n)
	}
}

// RESTART-VAULTS-1 (audit 11-18): until the vault set is known after a start
// (VaultsUnknown), a tick scans no block: a deposit to an older generation would
// otherwise be scanned past with only the configured active address watched.
// Once it is known, the same block is scanned.
func TestRESTARTVAULTS1_NoDepositScanUntilTheVaultSetIsKnown(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 100)
	l2.onDemand = true
	ethDeposit(t, l1.block(101), vault, minETHDeposit, nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	cfg.VaultsUnknown = true
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 100 || len(cp.WaitingDeposits) != 0 {
		t.Fatalf("scanned to %d (waiting %v) before the vault set was known", cp.LastScannedBlock, cp.WaitingDeposits)
	}
	cfg.VaultsUnknown = false
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 101 {
		t.Fatalf("scanned to %d once the vault set is known, want 101", cp.LastScannedBlock)
	}
}

// RESTART-VAULTS-1: when the vault set counts as known. A read error, a
// registry with no active generation, or "no registry" from a node after a
// registry was once seen (a node behind its creation, or one that misses the
// key locally) keep the deposit scan on hold.
func TestRESTARTVAULTS1_WhenTheVaultSetIsKnown(t *testing.T) {
	active := &vaultGen{Gen: 3, Address: "0x00000000000000000000000000000000000ab1e5"}
	cp := &Checkpoint{}
	if vaultSetKnown(vaultView{}, errors.New("node down"), cp) {
		t.Fatal("a read error counted as known")
	}
	if !vaultSetKnown(vaultView{Registry: false}, nil, cp) {
		t.Fatal("no registry on a bridge that never had one: the configured address is its vault set")
	}
	if vaultSetKnown(vaultView{Registry: true}, nil, cp) || !cp.RegistrySeen {
		t.Fatalf("a registry with no active generation counted as known (seen=%v)", cp.RegistrySeen)
	}
	if vaultSetKnown(vaultView{Registry: false}, nil, cp) {
		t.Fatal("'no registry' after one was seen counted as known")
	}
	cp2 := &Checkpoint{}
	if !vaultSetKnown(vaultView{Registry: true, Active: active}, nil, cp2) || !cp2.RegistrySeen {
		t.Fatalf("a registry with an active generation: known=false or seen=%v", cp2.RegistrySeen)
	}
}

// SCAN-AMPLIFY-1: ETH transfers to the vault below the bridge's minimum
// deposit are never credited, so their receipts are not fetched; a real
// deposit in the same block still is.
func TestSCANAMPLIFY1_DustETHTransfersCostNoReceipt(t *testing.T) {
	c := &fakeChain{}
	vault := common.HexToAddress("0x00000000000000000000000000000000000ab1e5")
	for i := 0; i < 40; i++ {
		k, _ := ethCrypto.GenerateKey()
		c.addTx(t, k, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 21000, To: &vault, Value: big.NewInt(1)})
	}
	k, _ := ethCrypto.GenerateKey()
	c.addTx(t, k, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1), Gas: 21000, To: &vault, Value: minETHDeposit})
	p := &rcptCounter{target: c.rpc(t).urls[0]}
	res, err := scanBlockSafe(newEthRPC(p.url(t)), 101, []string{"0x00000000000000000000000000000000000ab1e5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deposits) != 1 || res.Deposits[0].Amount.Cmp(minETHDeposit) != 0 {
		t.Fatalf("deposits %+v, want the one at the minimum", res.Deposits)
	}
	if got := p.calls["eth_getTransactionReceipt"]; got != 1 {
		t.Fatalf("%d receipt downloads for 40 dust transfers and 1 deposit, want 1", got)
	}
}
