package mapper

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// BOT-ADDR-2 / BOT-DEAD-1 (testnet 2026-10-22): the bot mapped a deposit to a PURGED
// generation's address (the contract credited nothing and reported success) and
// never saw deposits to the live generations of an instruction it knew. These pin
// the rule it now follows: map exactly what the contract credits.

const depositTestInstr = "deposit_to=hive:magi.contracts"

func depositKey(b byte, prefix byte) []byte {
	k := make([]byte, 33)
	k[0] = prefix
	for i := 1; i < 33; i++ {
		k[i] = b
	}
	return k
}

func genSet(gen uint32, status btcvault.VaultStatus, b byte) DepositKeySet {
	return DepositKeySet{
		Generation: gen, Status: status,
		Primary: depositKey(b, 0x02), Backup: depositKey(0xBB, 0x03),
		Creditable: btcvault.IsFundHoldingStatus(status),
	}
}

func setsOf(fp string, sets ...DepositKeySet) *DepositKeySets {
	return &DepositKeySets{Fingerprint: fp, HasRegistry: true, Sets: sets}
}

// depositBot is a mocked bot that derives real testnet4 deposit addresses.
func depositBot(t *testing.T) (*Bot, *mockGraphQL, *mockContractCaller, *mockStateStore, *mockAddressStore, *mockInstructionStore) {
	t.Helper()
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	bot.Chain.AddressGen = &chain.BTCAddressGenerator{Params: &chaincfg.TestNet4Params, BackupCSVBlocks: 2}
	return bot, gql, caller, state, addr, bot.InstrDB.(*mockInstructionStore)
}

func addrFor(t *testing.T, bot *Bot, s DepositKeySet, instr string) string {
	t.Helper()
	a, _, err := bot.Chain.AddressGen.GenerateDepositAddress(hex.EncodeToString(s.Primary), hex.EncodeToString(s.Backup), instr)
	require.NoError(t, err)
	return a
}

func matchedIndices(results []*MappingInputData) map[uint64][]string {
	out := map[uint64][]string{}
	for _, r := range results {
		out[r.TxData.TxIndex] = r.Instructions
	}
	return out
}

// Every generation the contract credits (active, retiring, draining, inactive) is
// watched for a known instruction; the purged one is not.
func TestDepositIndex_MapsEveryCreditableGenerationOnly(t *testing.T) {
	bot, gql, _, _, _, instrs := depositBot(t)
	g1 := genSet(1, btcvault.VaultStatusPurged, 0x11)
	g2 := genSet(2, btcvault.VaultStatusInactive, 0x22)
	g3 := genSet(3, btcvault.VaultStatusRetiring, 0x33)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	g5 := genSet(5, btcvault.VaultStatusDraining, 0x55)
	gql.keySets = setsOf("f1", g1, g2, g3, g4, g5)
	_, err := instrs.Touch(context.Background(), depositTestInstr)
	require.NoError(t, err)

	p := &chaincfg.TestNet4Params
	block := buildBlock(t, []*wire.MsgTx{
		makeCoinbaseTx(),
		makePayToAddrTx(t, addrFor(t, bot, g4, depositTestInstr), p), // 1 active
		makePayToAddrTx(t, addrFor(t, bot, g3, depositTestInstr), p), // 2 retiring
		makePayToAddrTx(t, addrFor(t, bot, g2, depositTestInstr), p), // 3 inactive
		makePayToAddrTx(t, addrFor(t, bot, g1, depositTestInstr), p), // 4 purged
		makePayToAddrTx(t, addrFor(t, bot, g5, depositTestInstr), p), // 5 draining
	})
	results, err := bot.ParseBlock(context.Background(), block, 100)
	require.NoError(t, err)
	got := matchedIndices(results)
	assert.Len(t, got, 4, "active, retiring, inactive and draining are credited")
	for _, i := range []uint64{1, 2, 3, 5} {
		assert.Equal(t, []string{depositTestInstr}, got[i], "tx %d", i)
	}
	_, purged := got[4]
	assert.False(t, purged, "a purged generation's deposit is never submitted")
}

// The live 10-22 case: the address store still holds gen 1's address from when gen 1
// was active. Gen 1 is purged now: the deposit must not be mapped.
func TestDepositIndex_StoredAddressOfAPurgedGenerationIsNotMapped(t *testing.T) {
	bot, gql, caller, state, addr, _ := depositBot(t)
	g1 := genSet(1, btcvault.VaultStatusPurged, 0x11)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	gql.keySets = setsOf("f1", g1, g4)
	gql.lastHeight = "200"
	gen1Addr := addrFor(t, bot, g1, depositTestInstr)
	addr.instructions[gen1Addr] = depositTestInstr // old row; instruction store empty (pre-backfill)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))

	block := buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(), makePayToAddrTx(t, gen1Addr, &chaincfg.TestNet4Params)})
	assert.True(t, bot.HandleMap(block, 100), "the block is processed")
	assert.Empty(t, caller.getCalls(), "no map is submitted for a purged generation")
	h, err := state.GetBlockHeight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(101), h, "the block advances: the deposit is reported, not retried")
}

// The other 10-22 case: 43,000 sats sent to the gen-4 and gen-3 addresses of an
// instruction the bot knew, but whose addresses it never handed out. Mapped now.
func TestDepositIndex_KnownInstructionIsMappedOnEveryLiveGeneration(t *testing.T) {
	bot, gql, caller, state, _, instrs := depositBot(t)
	g3 := genSet(3, btcvault.VaultStatusRetiring, 0x33)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	gql.keySets = setsOf("f1", g3, g4)
	gql.lastHeight = "200"
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	_, err := instrs.Touch(context.Background(), depositTestInstr)
	require.NoError(t, err)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))

	block := buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(),
		makePayToAddrTx(t, addrFor(t, bot, g3, depositTestInstr), &chaincfg.TestNet4Params)})
	assert.True(t, bot.HandleMap(block, 100))
	calls := caller.getCalls()
	require.Len(t, calls, 1, "the retiring generation's deposit is mapped")
	assert.Equal(t, "map", calls[0].Action)
	assert.Equal(t, 1, instrs.deposits[depositTestInstr], "a landed map marks the instruction used (never pruned)")
}

// A rotation changes the registry fingerprint: the index is rebuilt and the new
// active generation is watched for every known instruction.
func TestDepositIndex_RotationRebuildsTheWatchedSet(t *testing.T) {
	bot, gql, _, _, _, instrs := depositBot(t)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	gql.keySets = setsOf("before", g4)
	_, err := instrs.Touch(context.Background(), depositTestInstr)
	require.NoError(t, err)
	p := &chaincfg.TestNet4Params
	_, err = bot.ParseBlock(context.Background(), buildBlock(t, []*wire.MsgTx{makeCoinbaseTx()}), 100)
	require.NoError(t, err)

	g4r := genSet(4, btcvault.VaultStatusRetiring, 0x44)
	g5 := genSet(5, btcvault.VaultStatusActive, 0x55)
	gql.keySets = setsOf("after", g4r, g5)
	results, err := bot.ParseBlock(context.Background(), buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(),
		makePayToAddrTx(t, addrFor(t, bot, g5, depositTestInstr), p),
		makePayToAddrTx(t, addrFor(t, bot, g4r, depositTestInstr), p)}), 101)
	require.NoError(t, err)
	assert.Len(t, matchedIndices(results), 2, "new active and now-retiring generation both watched")
}

// An instruction requested on another bot instance (written to the shared store after
// this instance's last load) is picked up at the next block.
func TestDepositIndex_PicksUpInstructionsFromOtherInstances(t *testing.T) {
	bot, gql, _, _, _, instrs := depositBot(t)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	gql.keySets = setsOf("f1", g4)
	p := &chaincfg.TestNet4Params
	_, err := bot.ParseBlock(context.Background(), buildBlock(t, []*wire.MsgTx{makeCoinbaseTx()}), 100)
	require.NoError(t, err)

	other := "deposit_to=hive:someone-else"
	_, err = instrs.Touch(context.Background(), other) // the other instance's request
	require.NoError(t, err)
	results, err := bot.ParseBlock(context.Background(), buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(),
		makePayToAddrTx(t, addrFor(t, bot, g4, other), p)}), 101)
	require.NoError(t, err)
	assert.Equal(t, []string{other}, matchedIndices(results)[1])
}

// A failed registry read holds the block (retried next cycle), like the height read.
func TestDepositIndex_RegistryReadErrorHoldsTheBlock(t *testing.T) {
	bot, gql, caller, state, _, _ := depositBot(t)
	gql.lastHeight = "200"
	gql.readErr = map[string]error{"FetchVaultKeySets": errors.New("node unreachable")}
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	block := buildBlock(t, []*wire.MsgTx{makeCoinbaseTx()})
	assert.False(t, bot.HandleMap(block, 100))
	assert.Empty(t, caller.getCalls())
	h, err := state.GetBlockHeight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(100), h, "the block is kept for retry")
}

// With a registry, a stored address no generation derives cannot be credited by the
// contract: reported, not mapped.
func TestDepositIndex_UnderivableStoredAddressIsNotMappedWithARegistry(t *testing.T) {
	bot, gql, _, _, addr, _ := depositBot(t)
	gql.keySets = setsOf("f1", genSet(4, btcvault.VaultStatusActive, 0x44))
	stray := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[stray] = depositTestInstr
	results, err := bot.ParseBlock(context.Background(),
		buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(), makePayToAddrTx(t, stray, &chaincfg.TestNet4Params)}), 100)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// A contract without a registry (mainnet v1, the other UTXO chains): the legacy key
// pair is the single creditable generation. A known instruction is watched even
// with no stored address row (the row may have been pruned), and a stored row
// still maps exactly as before.
func TestDepositIndex_LegacyContract(t *testing.T) {
	bot, gql, _, _, addr, instrs := depositBot(t)
	legacy := DepositKeySet{Status: btcvault.VaultStatusActive, Primary: depositKey(0x77, 0x02),
		Backup: depositKey(0x78, 0x03), Creditable: true, Legacy: true}
	gql.keySets = &DepositKeySets{Fingerprint: "legacy:x", Sets: []DepositKeySet{legacy}}
	_, err := instrs.Touch(context.Background(), depositTestInstr)
	require.NoError(t, err)
	stray := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[stray] = "deposit_to=hive:testuser"
	p := &chaincfg.TestNet4Params
	results, err := bot.ParseBlock(context.Background(), buildBlock(t, []*wire.MsgTx{makeCoinbaseTx(),
		makePayToAddrTx(t, addrFor(t, bot, legacy, depositTestInstr), p),
		makePayToAddrTx(t, stray, p)}), 100)
	require.NoError(t, err)
	got := matchedIndices(results)
	assert.Equal(t, []string{depositTestInstr}, got[1], "derived legacy address of a known instruction")
	assert.Equal(t, []string{"deposit_to=hive:testuser"}, got[2], "stored legacy row unchanged")
}

// A new request scans the recent history of every creditable address of its
// instruction, not only the one handed out.
func TestHandleExistingDeposits_ScansEveryLiveGeneration(t *testing.T) {
	bot, gql, _, _, _, _ := depositBot(t)
	g3 := genSet(3, btcvault.VaultStatusRetiring, 0x33)
	g4 := genSet(4, btcvault.VaultStatusActive, 0x44)
	g1 := genSet(1, btcvault.VaultStatusPurged, 0x11)
	gql.keySets = setsOf("f1", g1, g3, g4)
	require.NoError(t, bot.refreshDepositIndex(context.Background()))
	got := bot.creditableAddresses(depositTestInstr)
	assert.ElementsMatch(t, []string{addrFor(t, bot, g3, depositTestInstr), addrFor(t, bot, g4, depositTestInstr)}, got)
}

// --- FetchVaultKeySets / FetchPublicKeys against a node's raw state ---

func registryEntry(gen uint32, status btcvault.VaultStatus, p, bk byte) []byte {
	e := make([]byte, btcvault.VaultEntrySize)
	binary.BigEndian.PutUint32(e[0:], gen)
	if p != 0 {
		copy(e[4:37], depositKey(p, 0x02))
	}
	if bk != 0 {
		copy(e[37:70], depositKey(bk, 0x03))
	}
	e[70] = byte(status)
	return e
}

func TestFetchVaultKeySets_FromTheRegistry(t *testing.T) {
	va := make([]byte, 4)
	binary.BigEndian.PutUint32(va, 1)
	reg := append(append(registryEntry(0, btcvault.VaultStatusPurged, 0xA0, 0xBB),
		registryEntry(1, btcvault.VaultStatusActive, 0xB1, 0xBB)...),
		registryEntry(2, btcvault.VaultStatusPending, 0, 0xBB)...)
	state := map[string]string{"v": hex.EncodeToString(reg), "va": hex.EncodeToString(va),
		"pubkey": "02" + strings.Repeat("a0", 32), "backupkey": "03" + strings.Repeat("bb", 32)}
	ks, err := keyNode(t, state).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	assert.True(t, ks.HasRegistry)
	require.Len(t, ks.Sets, 2, "the pending generation (no keys) derives nothing")
	assert.Equal(t, uint32(0), ks.Sets[0].Generation)
	assert.False(t, ks.Sets[0].Creditable, "purged: alert only")
	assert.Equal(t, uint32(1), ks.Sets[1].Generation)
	assert.True(t, ks.Sets[1].Creditable)

	same, err := keyNode(t, state).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ks.Fingerprint, same.Fingerprint, "same state, same fingerprint")
	reg2 := append(append(registryEntry(0, btcvault.VaultStatusPurged, 0xA0, 0xBB),
		registryEntry(1, btcvault.VaultStatusRetiring, 0xB1, 0xBB)...),
		registryEntry(2, btcvault.VaultStatusActive, 0xC2, 0xBB)...)
	state["v"] = hex.EncodeToString(reg2)
	moved, err := keyNode(t, state).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, ks.Fingerprint, moved.Fingerprint, "a rotation changes the fingerprint")
}

// The contract's fail-safe: a registry with no creditable, keyed generation (genesis
// still pending) credits the legacy pair. The bot mirrors it.
func TestFetchVaultKeySets_PendingOnlyRegistryFallsBackToLegacy(t *testing.T) {
	va := make([]byte, 4)
	reg := registryEntry(0, btcvault.VaultStatusPending, 0, 0)
	ks, err := keyNode(t, map[string]string{"v": hex.EncodeToString(reg), "va": hex.EncodeToString(va),
		"pubkey": "02" + strings.Repeat("d4", 32), "backupkey": "03" + strings.Repeat("d4", 32)}).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	require.Len(t, ks.Sets, 1)
	assert.True(t, ks.Sets[0].Legacy && ks.Sets[0].Creditable)
}

func TestFetchVaultKeySets_LegacyKeysWithoutARegistry(t *testing.T) {
	ks, err := keyNode(t, map[string]string{"pubkey": "02" + strings.Repeat("c3", 32), "backupkey": "03" + strings.Repeat("c3", 32)}).
		FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	assert.False(t, ks.HasRegistry)
	require.Len(t, ks.Sets, 1)
	assert.True(t, ks.Sets[0].Legacy)
	assert.True(t, ks.Sets[0].Creditable)
}

func TestFetchVaultKeySets_EmptyContract(t *testing.T) {
	ks, err := keyNode(t, map[string]string{}).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ks.Sets)
}

// The pre-fold state every v2 contract passes through: SaveToState writes "v" (empty)
// and "va" (generation 0) on every save, and the contract credits the legacy pair.
// The bot must do the same, not stall.
func TestPreFoldContractUsesTheLegacyKeys(t *testing.T) {
	state := map[string]string{"va": "00000000",
		"pubkey": "02" + strings.Repeat("c3", 32), "backupkey": "03" + strings.Repeat("c3", 32)}
	ks, err := keyNode(t, state).FetchVaultKeySets(context.Background())
	require.NoError(t, err)
	require.Len(t, ks.Sets, 1)
	assert.True(t, ks.Sets[0].Legacy)
	primary, _, err := keyNode(t, state).FetchPublicKeys(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "02"+strings.Repeat("c3", 32), hex.EncodeToString(primary))
}

// C1: generation > 0 active without a registry is a broken read (a rotation always
// leaves registry entries). Neither the watched set nor a new deposit address may fall
// back to the legacy slots (gen 0's keys).
func TestActiveGenerationWithoutRegistryFailsClosed(t *testing.T) {
	va := make([]byte, 4)
	binary.BigEndian.PutUint32(va, 4)
	state := map[string]string{"va": hex.EncodeToString(va),
		"pubkey": "02" + strings.Repeat("a0", 32), "backupkey": "03" + strings.Repeat("a0", 32)}
	_, err := keyNode(t, state).FetchVaultKeySets(context.Background())
	assert.Error(t, err)
	_, _, err = keyNode(t, state).FetchPublicKeys(context.Background())
	assert.Error(t, err)
}
