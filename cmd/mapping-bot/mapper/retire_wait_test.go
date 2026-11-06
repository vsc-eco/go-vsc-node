package mapper

import (
	"encoding/hex"
	"errors"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/lib/btcvault"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
)

// I-26 (BTC side of the EVM testnet loss, 11-04): gen 1 is Inactive and empty,
// gen 2 Active, so the driver's step is retireVault, which purges gen 1 once
// the contract height is past its grace. A deposit to gen 1 in a block the
// contract can already map, but the bot has not, would be lost to that purge:
// the retire waits until the bot has mapped every mappable block.
func TestI26_RetireWaitsForMappableBlocks(t *testing.T) {
	contractId := l2TestBotConfig{}.ContractId()
	reset := func() {
		vaultOpMu.Lock()
		delete(lastVaultOpAt, contractId)
		vaultOpMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
	bot := keyNode(t, map[string]string{
		contractinterface.VaultRegistryKey: hex.EncodeToString(append(
			vaultEntry(1, btcvault.VaultStatusInactive), vaultEntry(2, btcvault.VaultStatusActive)...)),
	})
	gql := &mockGraphQL{txStatuses: map[string]string{"mock-tx-id": "CONFIRMED"}, keyCommitment: map[string]string{}, readErr: map[string]error{}}
	caller := &mockContractCaller{}
	state := newMockStateStore()
	bot.Gql, bot.Caller, bot.StateDB = gql, caller, state
	bot.Chain = &chain.ChainConfig{Client: newMockChainClient(), ConfirmationsRequired: 4}
	retires := func() int {
		n := 0
		for _, c := range caller.getCalls() {
			if c.Action == "retireVault" {
				n++
			}
		}
		return n
	}
	cycle := func(next uint64, height string, want int, why string) {
		t.Helper()
		reset() // minutes pass between cycles
		state.blockHeight, gql.lastHeight = next, height
		bot.HandleVaultRotation()
		if got := retires(); got != want {
			t.Fatalf("retireVault issued %d times, want %d (%s)", got, want, why)
		}
	}
	cycle(95, "100", 0, "block 95 is mappable at contract height 100 (95+4 <= 100) and not mapped")
	cycle(96, "100", 0, "block 96 is mappable at 100 (96+4 = 100)")
	cycle(97, "100", 1, "every mappable block is mapped (97 needs 101)")
	gql.readErr["FetchLastHeight"] = errors.New("node unreachable")
	cycle(97, "100", 1, "an unreadable contract height waits")
	delete(gql.readErr, "FetchLastHeight")

	// no Inactive generation: retireVault cannot purge, the cursor does not matter
	bot2 := keyNode(t, map[string]string{
		contractinterface.VaultRegistryKey: hex.EncodeToString(append(
			vaultEntry(1, btcvault.VaultStatusDraining), vaultEntry(2, btcvault.VaultStatusActive)...)),
	})
	caller2 := &mockContractCaller{}
	bot2.Gql, bot2.Caller, bot2.StateDB = gql, caller2, state
	bot2.Chain = bot.Chain
	reset()
	state.blockHeight, gql.lastHeight = 90, "100"
	bot2.HandleVaultRotation()
	n := 0
	for _, c := range caller2.getCalls() {
		if c.Action == "retireVault" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("retireVault issued %d times with no Inactive generation, want 1 (inactivation loses nothing)", n)
	}
}
