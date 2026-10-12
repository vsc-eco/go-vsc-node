package mapper

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/lib/btcvault"

	contractinterface "vsc-node/cmd/mapping-bot/contract-interface"
)

// Testnet 2026-10-11: the gen-2 key was made in epoch 1368 (block 7276300) and the
// bot built the first migration sweep in that same epoch, so the funds moved to a
// key that had never been reshared; its first reshare came in epoch 1369 (block
// 7278600). The check-signature only proves the key signs with the committee that
// made it. Until it has been reshared once, members leaving the election can strand
// everything moved to it, so the driver must not start a migration before the
// ACTIVE key's newest keygen/reshare commitment is a reshare.
func TestHandleVaultRotation_NoMigrationBeforeTheNewKeyReshared(t *testing.T) {
	contractId := l2TestBotConfig{}.ContractId()
	activeKey := contractId + "-" + btcvault.VaultKeyName(2)
	retiringKey := contractId + "-" + btcvault.VaultKeyName(1)
	resetDriverGlobals := func() {
		vaultOpMu.Lock()
		delete(lastVaultOpAt, contractId)
		vaultOpMu.Unlock()
	}
	resetDriverGlobals()
	t.Cleanup(resetDriverGlobals)

	// Gen 1 retiring with one coin, gen 2 active, fee reserve funded, nothing in
	// flight: the driver's next step is migrateVault.
	utxo := make([]byte, 32+4+8)
	utxo = append(utxo, 0, 0)
	utxo = binary.BigEndian.AppendUint32(utxo, 1)
	supply := make([]byte, 32)
	binary.BigEndian.PutUint64(supply[16:], 21950)
	bot := keyNode(t, map[string]string{
		contractinterface.VaultRegistryKey: hex.EncodeToString(append(
			vaultEntry(1, btcvault.VaultStatusRetiring), vaultEntry(2, btcvault.VaultStatusActive)...)),
		contractinterface.UtxoRegistryKey:  "0007000000000000",
		contractinterface.UtxoPrefix + "7": hex.EncodeToString(utxo),
		contractinterface.SupplyKey:        hex.EncodeToString(supply),
	})
	gql := &mockGraphQL{
		txStatuses:    map[string]string{"mock-tx-id": "CONFIRMED"},
		keyCommitment: map[string]string{},
		readErr:       map[string]error{},
	}
	caller := &mockContractCaller{}
	bot.Gql = gql
	bot.Caller = caller
	bot.StateDB = newMockStateStore()
	bot.Chain = &chain.ChainConfig{Client: newMockChainClient()}

	migrations := func() int {
		n := 0
		for _, c := range caller.getCalls() {
			if c.Action == "migrateVault" {
				n++
			}
		}
		return n
	}
	cycle := func(want int, why string) {
		t.Helper()
		vaultOpMu.Lock()
		delete(lastVaultOpAt, contractId) // minutes pass between cycles
		vaultOpMu.Unlock()
		bot.HandleVaultRotation()
		if got := migrations(); got != want {
			t.Fatalf("migrateVault issued %d times, want %d (%s)", got, want, why)
		}
	}

	gql.keyCommitment[retiringKey] = "reshare"
	gql.keyCommitment[activeKey] = "keygen"
	cycle(0, "the active key has only its keygen (the retiring key's reshares do not count)")
	cycle(0, "still only its keygen on the next cycle")

	delete(gql.keyCommitment, activeKey)
	cycle(0, "the node reports no keygen/reshare commitment for the active key")

	gql.keyCommitment[activeKey] = "reshare"
	gql.readErr["FetchLatestKeyCommitmentType"] = errors.New("node unreachable")
	cycle(0, "the commitments cannot be read")

	delete(gql.readErr, "FetchLatestKeyCommitmentType")
	cycle(1, "the active key has been reshared: the migration starts")
}
