package mapper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// psSetup: a previous transaction paying the sender, in a block the mock chain
// serves, and a deposit spending its output 1.
func psSetup(t *testing.T) (*Bot, *mockChainClient, *actionCaller, *mockGraphQL, *wire.MsgTx, string) {
	t.Helper()
	bot, gql, _, _, _, cc := newTestBotWithMocks()
	bot.senderProbe.ok = true // the deployed contract has proveSender
	caller := &actionCaller{errs: map[string]error{}}
	bot.Caller = caller
	gql.txStatuses = map[string]string{"tx-proveSender": "CONFIRMED"}
	pub := append([]byte{0x02}, bytes.Repeat([]byte{0x11}, 32)...)
	a, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub), &chaincfg.TestNet4Params)
	if err != nil {
		t.Fatal(err)
	}
	script, _ := txscript.PayToAddrScript(a)
	prev := wire.NewMsgTx(2)
	prev.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0xffffffff}, SignatureScript: []byte{1}})
	prev.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{0x6a}})
	prev.AddTxOut(&wire.TxOut{Value: 50_000, PkScript: script})
	other := wire.NewMsgTx(2)
	other.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0xffffffff}, SignatureScript: []byte{2}})
	other.AddTxOut(&wire.TxOut{Value: 7, PkScript: []byte{0x6a}})
	block := wire.MsgBlock{Header: wire.BlockHeader{Version: 1}, Transactions: []*wire.MsgTx{other, prev}}
	var buf bytes.Buffer
	if err := block.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	cc.rawBlocks["blockhash"] = buf.Bytes()
	cc.txDetails[prev.TxID()] = chain.TxConfirmationDetails{Confirmed: true, BlockHeight: 4321, BlockHash: "blockhash", TxIndex: 1}
	dep := wire.NewMsgTx(2)
	dep.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: prev.TxHash(), Index: 1}})
	dep.AddTxOut(&wire.TxOut{Value: 40_000, PkScript: script})
	return bot, cc, caller, gql, dep, a.EncodeAddress()
}

func TestProveSender_PayloadProvesTheFirstInputsOutput(t *testing.T) {
	bot, _, _, _, dep, sender := psSetup(t)
	raw, got, err := bot.buildProveSenderPayload(dep)
	if err != nil {
		t.Fatal(err)
	}
	if got != sender {
		t.Fatalf("sender %s, want %s", got, sender)
	}
	var p proveSenderPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(p.DepositTxB64)
	var back wire.MsgTx
	if err := back.Deserialize(bytes.NewReader(b)); err != nil || back.TxHash() != dep.TxHash() {
		t.Fatalf("deposit does not round-trip: %v", err)
	}
	if p.Sender.BlockHeight != 4321 || p.Sender.TxIndex != 1 || p.Sender.MerkleProofHex == "" {
		t.Fatalf("sender proof %+v", p.Sender)
	}
	pb, _ := base64.StdEncoding.DecodeString(p.Sender.RawTxB64)
	var prev wire.MsgTx
	if err := prev.Deserialize(bytes.NewReader(pb)); err != nil || prev.TxHash() != dep.TxIn[0].PreviousOutPoint.Hash {
		t.Fatal("the proven transaction is not the one the deposit spends")
	}
}

func TestProveSender_SentBeforeTheMapAndTheSenderWatched(t *testing.T) {
	bot, _, caller, _, dep, sender := psSetup(t)
	var buf bytes.Buffer
	_ = dep.Serialize(&buf)
	in := &MappingInputData{TxData: &VerificationRequest{RawTxB64: base64.StdEncoding.EncodeToString(buf.Bytes())},
		Instructions: []string{"swap_to=did:pkh:eip155:1:0x00000000000000000000000000000000000000e2&swap_asset_out=eth&destination_chain=ETH"}}
	bot.proveSenderBeforeMap(context.Background(), in)
	if got := caller.actions(); len(got) != 1 || got[0] != "proveSender" {
		t.Fatalf("calls %v, want one proveSender", got)
	}
	if _, ok := bot.refundWatch[sender]; !ok {
		t.Fatal("the proven sender is not watched for its refund")
	}
	// A plain deposit gets no proof.
	in.Instructions = []string{"deposit_to=hive:alice"}
	bot.proveSenderBeforeMap(context.Background(), in)
	if len(caller.actions()) != 1 {
		t.Fatal("a plain deposit was proven")
	}
}

func TestProveSender_RefusedDryRunIsNotBroadcast(t *testing.T) {
	bot, _, caller, gql, dep, _ := psSetup(t)
	gql.sim = &SimulatedCall{Success: false, Err: "input", ErrMsg: "the spent output belongs to the vault"}
	if err := bot.proveSender(context.Background(), dep); err == nil {
		t.Fatal("a refused proof was reported as sent")
	}
	if len(caller.actions()) != 0 {
		t.Fatal("a refused proof was broadcast")
	}
}

func TestProveSender_UnconfirmedOrMissingPreviousTx(t *testing.T) {
	bot, cc, _, _, dep, _ := psSetup(t)
	id := dep.TxIn[0].PreviousOutPoint.Hash.String()
	cc.txDetails[id] = chain.TxConfirmationDetails{}
	if _, _, err := bot.buildProveSenderPayload(dep); err == nil {
		t.Fatal("an unconfirmed previous tx was proven")
	}
	cc.txDetails[id] = chain.TxConfirmationDetails{Confirmed: true, BlockHeight: 1, BlockHash: "blockhash", TxIndex: 0}
	if _, _, err := bot.buildProveSenderPayload(dep); err == nil {
		t.Fatal("a wrong index was proven")
	}
	dep.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{9}
	if _, _, err := bot.buildProveSenderPayload(dep); err == nil {
		t.Fatal("an unknown previous tx was proven")
	}
}

func TestProveSender_OnlyTheBTCContract(t *testing.T) {
	bot, _, caller, _, dep, _ := psSetup(t)
	bot.Chain.Name = "ltc"
	var buf bytes.Buffer
	_ = dep.Serialize(&buf)
	bot.proveSenderBeforeMap(context.Background(), &MappingInputData{TxData: &VerificationRequest{RawTxB64: base64.StdEncoding.EncodeToString(buf.Bytes())},
		Instructions: []string{"swap_to=hive:bob&swap_asset_out=hbd"}})
	if len(caller.actions()) != 0 {
		t.Fatal("a sibling chain's bot sent a proof its contract does not take")
	}
}

// The bot switches to sender proofs only once the deployed contract has
// proveSender (its dry run with no proof answers "proveSender needs ..."); a
// contract from before it keeps the old rules, and is asked again later.
func TestProveSender_ProbesTheContract(t *testing.T) {
	bot, gql, _, _, _, _ := newTestBotWithMocks()
	if bot.Chain == nil {
		t.Skip("no chain config in the test bot")
	}
	bot.Chain.Name = "btc"
	gql.sim = &SimulatedCall{Success: false, Err: "wasm_error", ErrMsg: "export proveSender not found"}
	if bot.provesSenders() {
		t.Fatal("an old contract was taken as proving senders")
	}
	gql.sim = &SimulatedCall{Success: false, Err: "bad_input", ErrMsg: "proveSender needs the deposit transaction and a sender proof"}
	if bot.provesSenders() {
		t.Fatal("asked again before senderProbeEvery")
	}
	bot.senderProbe.checked = time.Now().Add(-senderProbeEvery - time.Second)
	if !bot.provesSenders() {
		t.Fatal("the new contract was not recognized")
	}
	gql.sim = nil
	if !bot.provesSenders() {
		t.Fatal("a recognized contract was dropped")
	}
	bot.Chain.Name = "dash"
	if bot.provesSenders() {
		t.Fatal("a sibling chain proves senders")
	}
}
