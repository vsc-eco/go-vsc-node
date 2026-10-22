package main

import (
	"context"
	"math/big"
	"testing"
)

// B-R2: an eth_getLogs error, or an answer that is not a list of logs, skipped
// the token's deposits while the block was still marked scanned: an ERC-20
// deposit in it was never mapped. The block now fails and is retried.
func TestBR2_ALogsFailureHoldsTheBlock(t *testing.T) {
	for _, mode := range []string{"error", "garbage"} {
		l1 := newFakeL1()
		_, gql, rpc, sub, cfg, _ := bridgeWorld(t, l1, 101)
		cfg.Tokens = map[string]string{"0x00000000000000000000000000000000000000c0": "usdc"}
		l1.block(101)
		l1.logs = mode
		cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		if cp.LastScannedBlock != 100 {
			t.Errorf("eth_getLogs %s: block 101 marked scanned (checkpoint %d), want it retried", mode, cp.LastScannedBlock)
		}
		l1.logs = ""
		if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
			t.Fatal(err)
		}
		if cp.LastScannedBlock != 101 {
			t.Errorf("eth_getLogs %s, then answering: checkpoint %d, want 101", mode, cp.LastScannedBlock)
		}
	}
}

// B-R3: when the contract's height could not be read, the bot scanned up to
// L1 finalized, past the verifier's stored headers; a map for a block with no
// header fails on chain and is dead-lettered. It now scans nothing that tick.
func TestBR3_NoContractHeightNoScan(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 101)
	ethDeposit(t, l1.block(101), vault, big.NewInt(1e18), nil)
	l2.failState = map[string]bool{"vsc1verifier": true}
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 100 || len(l2.submitted) != 0 {
		t.Fatalf("with no contract height: checkpoint %d, %d L2 submissions; want no scan", cp.LastScannedBlock, len(l2.submitted))
	}
	l2.failState = nil
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 101 || len(l2.submitted) != 1 {
		t.Fatalf("height readable again: checkpoint %d, %d L2 submissions; want the deposit mapped", cp.LastScannedBlock, len(l2.submitted))
	}
}
