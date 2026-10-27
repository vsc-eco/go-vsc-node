package main

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// bridgeWorld is a bridge contract wired to a verifier at height h, a vault
// and a bot submitter, all on fakes.
func bridgeWorld(t *testing.T, l1 *fakeL1, h uint64) (*fakeL2, *vscGraphQL, *ethRPC, *l2Submitter, EVMBotConfig, common.Address) {
	t.Helper()
	vaultKey, _ := ethCrypto.GenerateKey()
	vault := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	l2 := newFakeL2()
	l2.set("vsc1bridge", "zkverifier", "vsc1verifier")
	l2.set("vsc1verifier", "h", big.NewInt(int64(h)).String())
	gql, rpc := l2.gql(t), l1.rpc(t)
	botKey, _ := ethCrypto.GenerateKey()
	cfg := EVMBotConfig{ContractID: "vsc1bridge", VaultAddress: strings.ToLower(vault.Hex()), NetID: "vsc-devnet", RcLimit: 10000, Tokens: map[string]string{}}
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql, cfg: cfg}
	return l2, gql, rpc, sub, cfg, vault
}

func ethDeposit(t *testing.T, c *fakeChain, to common.Address, value *big.Int, data []byte) *types.Transaction {
	key, _ := ethCrypto.GenerateKey()
	return c.addTx(t, key, &types.DynamicFeeTx{GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000 + uint64(16*len(data)), To: &to, Value: value, Data: data})
}

// B6: a deposit whose map payload is over the node's contract-payload cap (1 wei
// with 4 KB of calldata, which anyone can send) used to fail on every retry and
// hold its block, so no later deposit was ever mapped. It is dead-lettered and
// the scan moves on.
func TestB6_OversizeDepositDoesNotHoldLaterDeposits(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 102)
	l2.status = func(string) string { return "UNCONFIRMED" } // keep the map pending so the test can see it
	attack := ethDeposit(t, l1.block(101), vault, big.NewInt(1), make([]byte, 4000))
	user := ethDeposit(t, l1.block(102), vault, big.NewInt(1e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}

	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 102 {
		t.Fatalf("scan stopped at block %d, want 102 (the oversize deposit in 101 held it)", cp.LastScannedBlock)
	}
	pendingFor := func(tx string) bool {
		for _, pm := range cp.PendingMaps {
			if pm.DepositTx == tx {
				return true
			}
		}
		return false
	}
	if len(l2.submitted) != 1 || !pendingFor(user.Hash().Hex()) || pendingFor(attack.Hash().Hex()) {
		t.Fatalf("submitted %d L2 txs, pending maps %+v; want exactly the later deposit's map", len(l2.submitted), cp.PendingMaps)
	}
	if len(cp.DeadLetters) != 1 || cp.DeadLetters[0].DepositTx != attack.Hash().Hex() || !strings.Contains(cp.DeadLetters[0].Reason, "contract-payload cap") {
		t.Fatalf("dead letters %+v, want the oversize deposit with the payload-cap reason", cp.DeadLetters)
	}
}

// B4: a map the L2 pool accepted but that failed on chain left the deposit
// uncredited and nobody told. Its result is now read on the same or a later
// tick: FAILED dead-letters it, CONFIRMED clears it, no result within
// mapResultTimeout dead-letters it.
func TestB4_MapResultsAreReadAndFailuresDeadLettered(t *testing.T) {
	for _, c := range []struct {
		status, wantReason string
	}{
		{"FAILED", "map failed on chain"},
		{"CONFIRMED", ""},
		{"UNCONFIRMED", "no on-chain result"},
	} {
		l1 := newFakeL1()
		l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
		l2.status = func(string) string { return c.status }
		dep := ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
		cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
		if c.status == "UNCONFIRMED" {
			old := mapResultTimeout
			mapResultTimeout = time.Nanosecond
			t.Cleanup(func() { mapResultTimeout = old })
		}
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		if len(cp.PendingMaps) != 0 {
			t.Errorf("%s: %d maps still pending", c.status, len(cp.PendingMaps))
		}
		switch {
		case c.wantReason == "" && len(cp.DeadLetters) != 0:
			t.Errorf("%s: dead letters %+v, want none", c.status, cp.DeadLetters)
		case c.wantReason != "" && (len(cp.DeadLetters) != 1 || cp.DeadLetters[0].DepositTx != dep.Hash().Hex() || !strings.Contains(cp.DeadLetters[0].Reason, c.wantReason)):
			t.Errorf("%s: dead letters %+v, want the deposit with %q", c.status, cp.DeadLetters, c.wantReason)
		}
	}
}

// A version 1 checkpoint (before PendingMaps and DeadLetters) loads.
func TestCheckpointVersion1Loads(t *testing.T) {
	p := t.TempDir() + "/cp.json"
	if err := writeFileForTest(p, `{"last_scanned_block":7,"sent_withdrawals":{},"version":1}`); err != nil {
		t.Fatal(err)
	}
	cp := loadCheckpoint(p)
	if cp.LastScannedBlock != 7 || cp.PendingMaps == nil || cp.Version != checkpointVersion {
		t.Fatalf("loaded %+v", cp)
	}
}

// RP-9: the verifier stores a header only for blocks the bridge uses. A deposit
// whose block has no stored header is parked (the scan moves on past it and the
// block is asked for, NEEDS_FILE), and it is mapped once the header is stored;
// a later deposit block waits on the same proof, not a proof of its own.
func TestRP9_DepositWaitsForItsHeaderWithoutStoppingTheScan(t *testing.T) {
	l1 := newFakeL1()
	for n := uint64(101); n <= 140; n++ {
		l1.block(n)
	}
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 140)
	l2.onDemand = true
	l2.status = func(string) string { return "UNCONFIRMED" }
	needs := t.TempDir() + "/needs.json"
	t.Setenv("NEEDS_FILE", needs)
	dep := ethDeposit(t, l1.block(110), vault, big.NewInt(1e18), nil)
	dep2 := ethDeposit(t, l1.block(130), vault, big.NewInt(2e18), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}

	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 140 || len(l2.submitted) != 0 || len(cp.WaitingDeposits) != 2 {
		t.Fatalf("headers not stored: scanned to %d, submitted %d, waiting %v; want scanned to 140, both blocks parked, nothing mapped", cp.LastScannedBlock, len(l2.submitted), cp.WaitingDeposits)
	}
	if _, ok := cp.HeaderNeeds[110]; !ok {
		t.Fatalf("header needs %v, want block 110", cp.HeaderNeeds)
	}
	if _, ok := cp.HeaderNeeds[130]; !ok {
		t.Fatalf("header needs %v, want block 130", cp.HeaderNeeds)
	}

	// The headers are stored (attested from one proof): both deposits map.
	l2.set("vsc1verifier", "b-110", strings.Repeat("\x00", 137))
	l2.set("vsc1verifier", "b-130", strings.Repeat("\x00", 137))
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if len(cp.WaitingDeposits) != 0 || len(cp.PendingMaps) != 2 || len(cp.HeaderNeeds) != 0 || len(cp.DeadLetters) != 0 {
		t.Fatalf("headers stored: waiting %v, pending %+v, needs %v, dead %+v; want both deposits mapped", cp.WaitingDeposits, cp.PendingMaps, cp.HeaderNeeds, cp.DeadLetters)
	}
	got := map[string]bool{}
	for _, pm := range cp.PendingMaps {
		got[pm.DepositTx] = true
	}
	if !got[dep.Hash().Hex()] || !got[dep2.Hash().Hex()] {
		t.Fatalf("pending maps %+v, want both deposits", cp.PendingMaps)
	}
}
