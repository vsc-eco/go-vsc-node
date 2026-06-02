package main

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/vsc-eco/hivego"
)

// TestSigningRoundTrip proves the central correctness property of the
// -no-broadcast / -broadcast-signed flow: reconstructing the transaction from
// the bundle's wire-form operations reproduces the exact same signing digest
// the external signer signed. If this drifts, the attached signature would be
// invalid and the broadcast would be rejected.
func TestSigningRoundTrip(t *testing.T) {
	chainId := "beeab0de00000000000000000000000000000000000000000000000000000000"

	ops := []hivego.HiveOperation{
		hivego.CustomJsonOperation{
			RequiredAuths:        []string{"alice"},
			RequiredPostingAuths: []string{},
			Id:                   "vsc.create_contract",
			Json:                 `{"net_id":"vsc-mainnet","name":"demo","code":"bafy...","runtime":"go"}`,
		},
		hivego.TransferOperation{
			From:   "alice",
			To:     "vsc.gateway",
			Amount: "10.000 HBD",
			Memo:   "",
		},
	}

	tx := hivego.HiveTransaction{
		RefBlockNum:    12345,
		RefBlockPrefix: 0xdeadbeef,
		Expiration:     "2026-06-01T12:00:00",
		Operations:     ops,
	}

	serialized, err := hivego.SerializeTx(tx)
	if err != nil {
		t.Fatalf("serialize original: %v", err)
	}
	wantDigest := hex.EncodeToString(hivego.HashTxForSig(serialized, chainId))
	wantTxId, err := tx.GenerateTrxId()
	if err != nil {
		t.Fatalf("txid original: %v", err)
	}

	// prepare -> wire form -> JSON (what the bundle persists)
	wire, err := opsToWire(ops)
	if err != nil {
		t.Fatalf("opsToWire: %v", err)
	}
	persisted, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}

	// broadcast-signed -> read JSON -> reconstruct typed ops
	var readWire [][2]json.RawMessage
	if err := json.Unmarshal(persisted, &readWire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	gotOps, err := wireToOps(readWire)
	if err != nil {
		t.Fatalf("wireToOps: %v", err)
	}

	reTx := hivego.HiveTransaction{
		RefBlockNum:    tx.RefBlockNum,
		RefBlockPrefix: tx.RefBlockPrefix,
		Expiration:     tx.Expiration,
		Operations:     gotOps,
	}
	reSerialized, err := hivego.SerializeTx(reTx)
	if err != nil {
		t.Fatalf("serialize reconstructed: %v", err)
	}
	gotDigest := hex.EncodeToString(hivego.HashTxForSig(reSerialized, chainId))
	gotTxId, err := reTx.GenerateTrxId()
	if err != nil {
		t.Fatalf("txid reconstructed: %v", err)
	}

	if gotDigest != wantDigest {
		t.Fatalf("signing digest drift after round-trip:\n original:      %s\n reconstructed: %s", wantDigest, gotDigest)
	}
	if gotTxId != wantTxId {
		t.Fatalf("txid drift after round-trip: original %s reconstructed %s", wantTxId, gotTxId)
	}
}

// TestWireToOpsRejectsUnknown ensures an unexpected operation in a bundle is a
// hard error rather than a silent drop (which would change the signed payload).
func TestWireToOpsRejectsUnknown(t *testing.T) {
	wire := [][2]json.RawMessage{
		{json.RawMessage(`"vote"`), json.RawMessage(`{}`)},
	}
	if _, err := wireToOps(wire); err == nil {
		t.Fatal("expected error for unsupported operation, got nil")
	}
}
