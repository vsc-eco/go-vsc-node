package main

import (
	"context"
	"math/big"
	"testing"
)

// DUST-1: the bot does not map a deposit below the bridge's minimum for its
// asset (the bridge refuses it, as THORChain's observer drops inbounds under
// its dust threshold): no map, no header need, no proof. A deposit whose
// amount or token floor is unknown is kept for the bridge to decide.
func TestDUST1_DepositsBelowTheFloorAreNotMapped(t *testing.T) {
	usdc := "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	below := new(big.Int).Sub(minETHDeposit, big.NewInt(1))
	mins := map[string]*big.Int{usdc: big.NewInt(100_000)}
	deps := []detectedDeposit{
		{TxHash: "0x01", DepositType: "eth", Amount: below},
		{TxHash: "0x02", DepositType: "eth", Amount: minETHDeposit},
		{TxHash: "0x03", DepositType: "erc20", TokenAddress: "0x1C7D4B196Cb0C7B01d743Fbc6116a902379C7238", Amount: big.NewInt(99_999)},
		{TxHash: "0x04", DepositType: "erc20", TokenAddress: usdc, Amount: big.NewInt(100_000)},
		{TxHash: "0x05", DepositType: "contract", Amount: below},                                  // MagiDeposit ETH
		{TxHash: "0x06", DepositType: "contract", TokenAddress: usdc, Amount: big.NewInt(99_999)}, // MagiDeposit USDC
		{TxHash: "0x07", DepositType: "erc20", TokenAddress: "0x00000000000000000000000000000000000000aa", Amount: big.NewInt(1)},
		{TxHash: "0x08", DepositType: "eth"}, // amount unknown (a checkpoint from before DUST-1)
	}
	got := map[string]bool{}
	for _, d := range creditable(deps, mins) {
		got[d.TxHash] = true
	}
	for tx, want := range map[string]bool{"0x01": false, "0x02": true, "0x03": false, "0x04": true, "0x05": false, "0x06": false, "0x07": true, "0x08": true} {
		if got[tx] != want {
			t.Errorf("deposit %s kept=%v, want %v", tx, got[tx], want)
		}
	}
}

// The token floor is a tenth of the token's registered min_withdrawal (the
// bridge's default 10 USDC when unset), read from its registry record.
func TestDUST1_TokenFloorsComeFromTheRegistry(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "chainid", "11155111")
	l2.set("vsc1bridge", "t-11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238", "usdc|6|1000000")
	l2.set("vsc1bridge", "t-11155111-00000000000000000000000000000000000000bb", "tok|18|0|0|0")
	cfg := EVMBotConfig{ContractID: "vsc1bridge", Tokens: map[string]string{
		"0x1c7d4b196cb0c7b01d743fbc6116a902379c7238": "usdc",
		"0x00000000000000000000000000000000000000bb": "tok",
		"0x00000000000000000000000000000000000000cc": "unregistered",
	}}
	mins := tokenDepositMins(context.Background(), l2.gql(t), cfg)
	if m := mins["0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"]; m == nil || m.Int64() != 100_000 {
		t.Fatalf("usdc floor %v, want 100000 (min_withdrawal 1000000 / 10)", m)
	}
	if m := mins["0x00000000000000000000000000000000000000bb"]; m == nil || m.Int64() != 1_000_000 {
		t.Fatalf("unset min_withdrawal floor %v, want the default 10 USDC / 10", m)
	}
	if _, ok := mins["0x00000000000000000000000000000000000000cc"]; ok {
		t.Fatal("an unregistered token got a floor")
	}
}

// A block holding only dust parks nothing and asks for no header; the scan
// moves past it.
func TestDUST1_ADustOnlyBlockAsksForNothing(t *testing.T) {
	l1 := newFakeL1()
	l2, gql, rpc, sub, cfg, vault := bridgeWorld(t, l1, 100)
	l2.onDemand = true
	ethDeposit(t, l1.block(101), vault, big.NewInt(1), nil)
	ethDeposit(t, l1.block(101), vault, new(big.Int).Sub(minETHDeposit, big.NewInt(1)), nil)
	cp := &Checkpoint{LastScannedBlock: 100, SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{}}
	if err := runOnce(context.Background(), rpc, gql, sub, cp, cfg, "vsc1bridge-primary"); err != nil {
		t.Fatal(err)
	}
	if cp.LastScannedBlock != 101 || len(cp.WaitingDeposits) != 0 || len(cp.HeaderNeeds) != 0 || len(l2.submitted) != 0 {
		t.Fatalf("dust-only block: scanned %d, waiting %v, header needs %v, %d L2 calls; want past it with nothing", cp.LastScannedBlock, cp.WaitingDeposits, cp.HeaderNeeds, len(l2.submitted))
	}
}
