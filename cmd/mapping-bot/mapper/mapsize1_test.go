package mapper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/cmd/mapping-bot/database"
	"vsc-node/modules/common/params"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildBatchedBlock is a block whose payment tx pays depositAddr among `others`
// unrelated P2WPKH outputs from one segwit input: an exchange's batched withdrawal
// (mainnet 11-05: Uphold, 122 outputs, 4,100 bytes).
func buildBatchedBlock(t *testing.T, depositAddr string, others int, p *chaincfg.Params) ([]byte, *wire.MsgTx) {
	t.Helper()
	coinbase := wire.NewMsgTx(wire.TxVersion)
	coinbase.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0xffffffff}, SignatureScript: []byte{0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04}, Sequence: 0xffffffff})
	coinbase.AddTxOut(&wire.TxOut{Value: 50e8, PkScript: []byte{txscript.OP_TRUE}})
	pay := wire.NewMsgTx(wire.TxVersion)
	pay.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: coinbase.TxHash(), Index: 0},
		Witness:          wire.TxWitness{bytes.Repeat([]byte{0x30}, 72), bytes.Repeat([]byte{0x02}, 33)},
		Sequence:         0xffffffff,
	})
	addr, err := btcutil.DecodeAddress(depositAddr, p)
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)
	pay.AddTxOut(&wire.TxOut{Value: 25_000, PkScript: script})
	for i := 0; i < others; i++ {
		pay.AddTxOut(&wire.TxOut{Value: 10_000 + int64(i), PkScript: append([]byte{0x00, 0x14}, bytes.Repeat([]byte{byte(i)}, 20)...)})
	}
	block := wire.MsgBlock{Header: wire.BlockHeader{Version: 1, Timestamp: time.Now(), Bits: 0x1d00ffff}, Transactions: []*wire.MsgTx{coinbase, pay}}
	h0, h1 := coinbase.TxHash(), pay.TxHash()
	block.Header.MerkleRoot = chainhash.DoubleHashH(append(h0[:], h1[:]...))
	var buf bytes.Buffer
	require.NoError(t, block.Serialize(&buf))
	return buf.Bytes(), pay
}

// MAP-SIZE-1 (mainnet 11-05): a map whose hex payload passes the node's
// contract-payload cap is sent in base64, with its witness when that fits (the
// contract's map log names the sender from it; same txid); a small deposit's map is
// byte-identical to before.
func TestMAPSIZE1_ABatchedDepositIsMappedInBase64(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:audiohive-me"
	gql.lastHeight = "200"
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}

	blockBytes, pay := buildBatchedBlock(t, depositAddr, 140, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	assert.True(t, bot.HandleMap(blockBytes, 100))
	calls := caller.getCalls()
	require.Len(t, calls, 1)
	var sent MappingInputData
	require.NoError(t, json.Unmarshal(calls[0].Payload, &sent))
	require.NotNil(t, sent.TxData)
	assert.Empty(t, sent.TxData.RawTxHex, "the compact form replaces raw_tx_hex")
	raw, err := base64.StdEncoding.DecodeString(sent.TxData.RawTxB64)
	require.NoError(t, err)
	var withWitnessTx bytes.Buffer
	require.NoError(t, pay.Serialize(&withWitnessTx))
	assert.Equal(t, withWitnessTx.Bytes(), raw, "raw_tx_b64 keeps the witness when it fits")
	assert.Equal(t, pay.TxID(), txIDFromTxData(sent.TxData), "same txid")
	n := mapPayloadNodeSize(&sent)
	assert.LessOrEqual(t, n, params.MAX_CONTRACT_PAYLOAD_SIZE, "the map fits the node's cap")
	full := sent
	td := *sent.TxData
	td.RawTxB64 = ""
	var withWitness bytes.Buffer
	require.NoError(t, pay.Serialize(&withWitness))
	td.RawTxHex = strings.ToLower(hexString(withWitness.Bytes()))
	full.TxData = &td
	t.Logf("map payload to the node: hex %d bytes, sent %d bytes (cap %d)", mapPayloadNodeSize(&full), n, params.MAX_CONTRACT_PAYLOAD_SIZE)
	assert.Greater(t, mapPayloadNodeSize(&full), params.MAX_CONTRACT_PAYLOAD_SIZE, "the fixture reproduces the mainnet shape")

	// A small deposit keeps raw_tx_hex and the old JSON shape (no raw_tx_b64 key).
	bot2, gql2, caller2, state2, addr2, _ := newTestBotWithMocks()
	addr2.instructions[depositAddr] = "deposit_to=hive:testuser"
	gql2.lastHeight = "200"
	gql2.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	require.NoError(t, state2.SetBlockHeight(context.Background(), 100))
	assert.True(t, bot2.HandleMap(buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params), 100))
	small := caller2.getCalls()
	require.Len(t, small, 1)
	assert.Contains(t, string(small[0].Payload), `"raw_tx_hex":"`)
	assert.NotContains(t, string(small[0].Payload), "raw_tx_b64")
}

// MAP-SIZE-1: a deposit too large for the cap even in the compact form is
// recorded for the operator without a map call (it would fail on chain) and does
// not hold the block.
func TestMAPSIZE1_TooLargeEvenCompactIsRecordedNotSent(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	failed := newMockFailedTxStore()
	bot.FailedTxDB = failed
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	gql.lastHeight = "200"
	blockBytes, pay := buildBatchedBlock(t, depositAddr, 400, &chaincfg.TestNet4Params)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))

	assert.True(t, bot.HandleMap(blockBytes, 100))
	assert.Empty(t, caller.getCalls(), "no map is sent: it cannot fit the node's payload cap")
	h, _ := state.GetBlockHeight(context.Background())
	assert.Equal(t, uint64(101), h, "the block is not held")
	all, _ := failed.GetAll(context.Background())
	require.Len(t, all, 1)
	assert.Equal(t, "map-too-large-"+pay.TxID(), all[0].TxId)
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i], out[2*i+1] = digits[c>>4], digits[c&0xf]
	}
	return string(out)
}

// MAP-SIZE-1: the node counts the payload as a JSON string literal (quotes, one
// backslash per '"'), so a map at or under the cap in raw bytes can still be over
// it to the node; fitMapPayload must judge by the node's count.
func TestMAPSIZE1_JudgedByTheNodesCountNotTheRawLength(t *testing.T) {
	_, pay := buildBatchedBlock(t, "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", 100, &chaincfg.TestNet4Params)
	var full bytes.Buffer
	require.NoError(t, pay.Serialize(&full))
	in := &MappingInputData{TxData: &VerificationRequest{BlockHeight: 1, RawTxHex: hexString(full.Bytes())}, Instructions: []string{"deposit_to=hive:x"}}
	raw, _ := json.Marshal(in)
	pad := params.MAX_CONTRACT_PAYLOAD_SIZE - len(raw) - 2 // raw two bytes under the cap
	require.Greater(t, pad, 0, "fixture too large")
	in.Instructions[0] += strings.Repeat("a", pad)
	raw, _ = json.Marshal(in)
	require.LessOrEqual(t, len(raw), params.MAX_CONTRACT_PAYLOAD_SIZE)
	require.Greater(t, mapPayloadNodeSize(in), params.MAX_CONTRACT_PAYLOAD_SIZE, "over the cap to the node")
	fitMapPayload(in, pay)
	assert.NotEmpty(t, in.TxData.RawTxB64, "a map over the node's count is sent in the compact form")
	assert.Empty(t, in.TxData.RawTxHex)
}

// witnessHeavyTx spends `inputs` vault-shaped P2WSH coins (witness: a 72-byte
// signature, the branch byte, a 112-byte script) to one output paying addr: a sweep,
// or a deposit made from many coins.
func witnessHeavyTx(t *testing.T, prev chainhash.Hash, inputs int, addr string, p *chaincfg.Params) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	for i := 0; i < inputs; i++ {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: prev, Index: uint32(i)},
			Witness:          wire.TxWitness{bytes.Repeat([]byte{0x30}, 72), {0x01}, bytes.Repeat([]byte{0x51}, 112)},
			Sequence:         0xfffffffd,
		})
	}
	a, err := btcutil.DecodeAddress(addr, p)
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(a)
	require.NoError(t, err)
	tx.AddTxOut(&wire.TxOut{Value: 900_000, PkScript: script})
	return tx
}

func blockWith(t *testing.T, tx *wire.MsgTx) ([]byte, *wire.MsgBlock) {
	t.Helper()
	coinbase := wire.NewMsgTx(wire.TxVersion)
	coinbase.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0xffffffff}, SignatureScript: []byte{0x04, 0xff, 0xff, 0x00, 0x1d, 0x01, 0x04}, Sequence: 0xffffffff})
	coinbase.AddTxOut(&wire.TxOut{Value: 50e8, PkScript: []byte{txscript.OP_TRUE}})
	block := wire.MsgBlock{Header: wire.BlockHeader{Version: 1, Timestamp: time.Now(), Bits: 0x1d00ffff}, Transactions: []*wire.MsgTx{coinbase, tx}}
	h0, h1 := coinbase.TxHash(), tx.TxHash()
	block.Header.MerkleRoot = chainhash.DoubleHashH(append(h0[:], h1[:]...))
	var buf bytes.Buffer
	require.NoError(t, block.Serialize(&buf))
	return buf.Bytes(), &block
}

// MAP-SIZE-1: when even base64 with the witness passes the cap (a deposit made from
// many segwit coins), the witness is dropped; the txid is unchanged.
func TestMAPSIZE1_WitnessDroppedOnlyWhenItMustBe(t *testing.T) {
	bot, gql, caller, state, addr, _ := newTestBotWithMocks()
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = "deposit_to=hive:testuser"
	gql.lastHeight = "200"
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	tx := witnessHeavyTx(t, chainhash.Hash{0x07}, 50, depositAddr, &chaincfg.TestNet4Params)
	blockBytes, _ := blockWith(t, tx)
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	assert.True(t, bot.HandleMap(blockBytes, 100))
	calls := caller.getCalls()
	require.Len(t, calls, 1)
	var sent MappingInputData
	require.NoError(t, json.Unmarshal(calls[0].Payload, &sent))
	raw, err := base64.StdEncoding.DecodeString(sent.TxData.RawTxB64)
	require.NoError(t, err)
	var full, bare bytes.Buffer
	require.NoError(t, tx.Serialize(&full))
	require.NoError(t, tx.SerializeNoWitness(&bare))
	withWitness := sent
	td := *sent.TxData
	td.RawTxB64 = base64.StdEncoding.EncodeToString(full.Bytes())
	withWitness.TxData = &td
	require.Greater(t, mapPayloadNodeSize(&withWitness), params.MAX_CONTRACT_PAYLOAD_SIZE, "the fixture needs a witness too big for the cap")
	assert.Equal(t, bare.Bytes(), raw, "the witness is dropped only when it must be")
	assert.LessOrEqual(t, mapPayloadNodeSize(&sent), params.MAX_CONTRACT_PAYLOAD_SIZE)
	assert.Equal(t, tx.TxID(), txIDFromTxData(sent.TxData))
}

// MAP-SIZE-1 for confirmSpend: a vault spend's witnesses are ~230 bytes per input in
// hex, so a migration sweep of 45 coins (mainnet's vault, 11-05; the contract allows
// 100 per tranche) passes the node's 8 KB cap and could never be confirmed. It is sent
// in base64; a one-input withdrawal's confirmSpend keeps raw_tx_hex, as before.
func TestMAPSIZE1_ConfirmSpendOfAManyInputSweepFitsTheCap(t *testing.T) {
	bot, _, _, _, _, chainClient := newTestBotWithMocks()
	vault := "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7"
	for _, inputs := range []int{1, 45} {
		tx := witnessHeavyTx(t, chainhash.Hash{0x09, byte(inputs)}, inputs, vault, &chaincfg.TestNet4Params)
		blockBytes, block := blockWith(t, tx)
		hash := block.BlockHash().String()
		chainClient.mu.Lock()
		chainClient.rawBlocks[hash] = blockBytes
		chainClient.mu.Unlock()
		var signed bytes.Buffer
		require.NoError(t, tx.Serialize(&signed))

		out, err := bot.buildConfirmSpendPayload(context.Background(), database.Transaction{TxID: tx.TxID(), RawTx: signed.Bytes()},
			chain.TxConfirmationDetails{Confirmed: true, BlockHeight: 300, BlockHash: hash, TxIndex: 1})
		require.NoError(t, err)
		var sent ConfirmSpendParams
		require.NoError(t, json.Unmarshal(out, &sent))
		assert.LessOrEqual(t, payloadNodeSize(&sent), params.MAX_CONTRACT_PAYLOAD_SIZE, "%d inputs: fits the node's cap", inputs)
		assert.Equal(t, tx.TxID(), txIDFromTxData(sent.TxData))
		if inputs == 1 {
			assert.Equal(t, hexString(signed.Bytes()), sent.TxData.RawTxHex, "a small confirmSpend is byte-identical to before")
			assert.Empty(t, sent.TxData.RawTxB64)
			continue
		}
		hexForm := sent
		td := *sent.TxData
		td.RawTxB64, td.RawTxHex = "", hexString(signed.Bytes())
		hexForm.TxData = &td
		t.Logf("45-input sweep confirmSpend: hex %d bytes to the node, sent %d", payloadNodeSize(&hexForm), payloadNodeSize(&sent))
		assert.Greater(t, payloadNodeSize(&hexForm), params.MAX_CONTRACT_PAYLOAD_SIZE, "in hex it could never be confirmed")
		assert.NotEmpty(t, sent.TxData.RawTxB64)
		assert.Empty(t, sent.TxData.RawTxHex)
	}
}

// MAP-SIZE-1: a map recorded as failed with an oversized raw_tx_hex (mainnet 11-05: the
// Uphold deposit, sent by the bot before fitTxData) is re-fitted by /retry, not resent as
// stored; a stored payload within the cap is resent byte-identical.
func TestMAPSIZE1_RetryRefitsAnOversizedStoredMap(t *testing.T) {
	bot, gql, caller, _, _, _ := newTestBotWithMocks()
	failed := newMockFailedTxStore()
	bot.FailedTxDB = failed
	gql.txStatuses = map[string]string{"mock-tx-id": "CONFIRMED"}
	_, pay := buildBatchedBlock(t, "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", 140, &chaincfg.TestNet4Params)
	var full bytes.Buffer
	require.NoError(t, pay.Serialize(&full))
	big, err := json.Marshal(&MappingInputData{TxData: &VerificationRequest{BlockHeight: 900, RawTxHex: hexString(full.Bytes()), MerkleProofHex: "", TxIndex: 1}, Instructions: []string{"deposit_to=hive:audiohive-me"}})
	require.NoError(t, err)
	require.Greater(t, payloadNodeSize(json.RawMessage(big)), params.MAX_CONTRACT_PAYLOAD_SIZE, "the stored map is the oversized shape")
	small := []byte(`{"tx_data":{"block_height":5,"raw_tx_hex":"00","merkle_proof_hex":"","tx_index":0},"instructions":["deposit_to=hive:x"]}`)
	const bigId = "bafyreifgbtk6lsplt7t7oask6ixv7iqyifwgwbnt5ddvu7msbut3gaicly"
	const smallId = "bafyreigyl2rxj4lrxonf5g3wnecjql25e3cxcklpkufnac3ofquqx6jzzu"
	require.NoError(t, failed.RecordFailed(context.Background(), bigId, "map", big))
	require.NoError(t, failed.RecordFailed(context.Background(), smallId, "map", small))

	require.NoError(t, bot.RetryFailedTx(context.Background(), bigId))
	calls := caller.getCalls()
	require.Len(t, calls, 1)
	var sent MappingInputData
	require.NoError(t, json.Unmarshal(calls[0].Payload, &sent))
	assert.Empty(t, sent.TxData.RawTxHex)
	assert.NotEmpty(t, sent.TxData.RawTxB64)
	assert.LessOrEqual(t, payloadNodeSize(calls[0].Payload), params.MAX_CONTRACT_PAYLOAD_SIZE)
	assert.Equal(t, pay.TxID(), txIDFromTxData(sent.TxData))
	assert.Equal(t, []string{"deposit_to=hive:audiohive-me"}, sent.Instructions)
	assert.Equal(t, uint64(900), sent.TxData.BlockHeight)

	time.Sleep(retryGlobalThrottle + 100*time.Millisecond)
	require.NoError(t, bot.RetryFailedTx(context.Background(), smallId))
	calls = caller.getCalls()
	require.Len(t, calls, 2)
	assert.JSONEq(t, string(small), string(calls[1].Payload), "a stored payload within the cap is resent as stored")
	assert.Equal(t, string(small), string(calls[1].Payload))
}
