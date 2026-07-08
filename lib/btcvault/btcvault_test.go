package btcvault

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/tinylib/msgp/msgp"
)

func pubHex(t *testing.T, seed byte) string {
	t.Helper()
	b := make([]byte, 32)
	b[31] = seed
	_, pub := btcec.PrivKeyFromBytes(b)
	return hex.EncodeToString(pub.SerializeCompressed())
}

// marshalVaultEntry builds one 87-byte entry the same way the contract's
// MarshalVaultRegistry does, so UnmarshalVaultRegistry can be round-tripped.
func marshalVaultEntry(v Vault) []byte {
	buf := make([]byte, VaultEntrySize)
	binary.BigEndian.PutUint32(buf[0:], v.Generation)
	copy(buf[4:37], v.Primary)
	copy(buf[37:70], v.Backup)
	buf[70] = byte(v.Status)
	binary.BigEndian.PutUint32(buf[71:], v.Predecessor)
	binary.BigEndian.PutUint32(buf[75:], v.CreatedHeight)
	binary.BigEndian.PutUint32(buf[79:], v.ActivatedHeight)
	binary.BigEndian.PutUint32(buf[83:], v.RetiredHeight)
	return buf
}

func TestUnmarshalVaultRegistryRoundTrip(t *testing.T) {
	p0, _ := hex.DecodeString(pubHex(t, 1))
	b0, _ := hex.DecodeString(pubHex(t, 2))
	p1, _ := hex.DecodeString(pubHex(t, 3))
	entries := []Vault{
		{Generation: 0, Primary: p0, Backup: b0, Status: VaultStatusRetiring, Predecessor: 0, CreatedHeight: 10, ActivatedHeight: 11, RetiredHeight: 0},
		{Generation: 1, Primary: p1, Backup: b0, Status: VaultStatusActive, Predecessor: 0, CreatedHeight: 20, ActivatedHeight: 21, RetiredHeight: 0},
	}
	var blob []byte
	for _, e := range entries {
		blob = append(blob, marshalVaultEntry(e)...)
	}
	got, err := UnmarshalVaultRegistry(blob)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Generation != 0 || got[0].Status != VaultStatusRetiring || !bytes.Equal(got[0].Primary, p0) || !bytes.Equal(got[0].Backup, b0) {
		t.Fatalf("entry0 mismatch: %+v", got[0])
	}
	if got[1].Generation != 1 || got[1].Status != VaultStatusActive || !bytes.Equal(got[1].Primary, p1) {
		t.Fatalf("entry1 mismatch: %+v", got[1])
	}
	if got[1].CreatedHeight != 20 || got[1].ActivatedHeight != 21 {
		t.Fatalf("entry1 heights mismatch: %+v", got[1])
	}
	// Empty blob = empty registry, not an error (pre-fold / fresh deploy).
	empty, err := UnmarshalVaultRegistry(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty blob: got %v err %v", empty, err)
	}
	// Malformed length fails.
	if _, err := UnmarshalVaultRegistry(make([]byte, VaultEntrySize+1)); err == nil {
		t.Fatalf("expected error on non-multiple length")
	}
}

func TestVaultKeyNameAndFundHolding(t *testing.T) {
	if VaultKeyName(0) != "main" {
		t.Fatalf("gen0 = %q, want main", VaultKeyName(0))
	}
	if VaultKeyName(1) != "mainv1" {
		t.Fatalf("gen1 = %q, want mainv1", VaultKeyName(1))
	}
	if VaultKeyName(42) != "mainv42" {
		t.Fatalf("gen42 = %q, want mainv42", VaultKeyName(42))
	}
	for s, want := range map[VaultStatus]bool{
		VaultStatusPending: false, VaultStatusActive: true, VaultStatusRetiring: true,
		VaultStatusDraining: true, VaultStatusInactive: false, VaultStatusPurged: false,
	} {
		if IsFundHoldingStatus(s) != want {
			t.Fatalf("IsFundHoldingStatus(%d) = %v, want %v", s, IsFundHoldingStatus(s), want)
		}
	}
	if v, ok := ReadUint32BE([]byte{0, 0, 1, 0}); !ok || v != 256 {
		t.Fatalf("ReadUint32BE = %d,%v want 256,true", v, ok)
	}
	if _, ok := ReadUint32BE([]byte{1, 2, 3}); ok {
		t.Fatalf("ReadUint32BE short should fail closed")
	}
}

func encodeSigningData(tx, sigHash, ws []byte, amount int64, withAmount bool) []byte {
	var b []byte
	b = msgp.AppendMapHeader(b, 2)
	b = msgp.AppendString(b, "tx")
	b = msgp.AppendBytes(b, tx)
	b = msgp.AppendString(b, "uh")
	b = msgp.AppendArrayHeader(b, 1)
	nfields := uint32(3)
	if withAmount {
		nfields = 4
	}
	b = msgp.AppendMapHeader(b, nfields)
	b = msgp.AppendString(b, "i")
	b = msgp.AppendUint32(b, 0)
	b = msgp.AppendString(b, "hs")
	b = msgp.AppendBytes(b, sigHash)
	b = msgp.AppendString(b, "ws")
	b = msgp.AppendBytes(b, ws)
	if withAmount {
		b = msgp.AppendString(b, "am")
		b = msgp.AppendInt64(b, amount)
	}
	return b
}

func TestDecodeSigningData(t *testing.T) {
	tx := []byte{0x01, 0x02, 0x03}
	sh := []byte{0xaa, 0xbb}
	ws := []byte{0x51, 0x21}
	// Without amount (current contract at 7ecbf9f).
	raw := encodeSigningData(tx, sh, ws, 0, false)
	sd, err := DecodeSigningData(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(sd.Tx, tx) || len(sd.UnsignedSigHashes) != 1 {
		t.Fatalf("bad decode: %+v", sd)
	}
	uh := sd.UnsignedSigHashes[0]
	if uh.Index != 0 || !bytes.Equal(uh.SigHash, sh) || !bytes.Equal(uh.WitnessScript, ws) {
		t.Fatalf("uh mismatch: %+v", uh)
	}
	if uh.HasAmount {
		t.Fatalf("HasAmount should be false when 'am' absent")
	}
	// With amount (post-S3.5 contract).
	raw2 := encodeSigningData(tx, sh, ws, 123456, true)
	sd2, err := DecodeSigningData(raw2)
	if err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if !sd2.UnsignedSigHashes[0].HasAmount || sd2.UnsignedSigHashes[0].Amount != 123456 {
		t.Fatalf("amount not decoded: %+v", sd2.UnsignedSigHashes[0])
	}
}

// TestSuccessorAndSighash exercises the two load-bearing primitives together:
// derive a successor P2WSH, build a tx paying it, and confirm
// RecomputeSegwitV0Sighash reproduces the exact contract-equivalent BIP143
// digest and that the digest is output-committing.
func TestSuccessorAndSighash(t *testing.T) {
	net := &chaincfg.MainNetParams
	primaryHex := pubHex(t, 7)
	backupHex := pubHex(t, 8)
	csv := CSVBlocksForNet(net)
	if csv != BackupCSVBlocksMainnet {
		t.Fatalf("mainnet csv = %d, want %d", csv, BackupCSVBlocksMainnet)
	}
	ws, pk, err := SuccessorPkScript(primaryHex, backupHex, csv, net)
	if err != nil {
		t.Fatalf("successor script: %v", err)
	}
	// pkScript must be a v0 P2WSH: OP_0 <32-byte sha256(ws)>.
	if len(pk) != 34 || pk[0] != txscript.OP_0 || pk[1] != 0x20 {
		t.Fatalf("pkScript not P2WSH: %x", pk)
	}
	class := txscript.GetScriptClass(pk)
	if class != txscript.WitnessV0ScriptHashTy {
		t.Fatalf("pkScript class = %v", class)
	}

	const amount = int64(500000)
	var prevHash chainhash.Hash
	for i := range prevHash {
		prevHash[i] = byte(i + 1)
	}
	buildTx := func(payTo []byte) *wire.MsgTx {
		tx := wire.NewMsgTx(wire.TxVersion)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: prevHash, Index: 0}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(amount-1000, payTo))
		return tx
	}
	tx := buildTx(pk)
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	txBytes := buf.Bytes()

	// Contract-equivalent inline computation (same txscript calls the contract
	// uses in signSpendTransaction).
	fetcher := txscript.NewCannedPrevOutputFetcher(pk, amount)
	want, err := txscript.CalcWitnessSigHash(ws, txscript.NewTxSigHashes(tx, fetcher), txscript.SigHashAll, tx, 0, amount)
	if err != nil {
		t.Fatalf("inline sighash: %v", err)
	}
	got, err := RecomputeSegwitV0Sighash(txBytes, 0, ws, amount)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("sighash mismatch:\n got %x\nwant %x", got, want)
	}

	// Output-committing: changing the output changes the sighash.
	otherPk := make([]byte, len(pk))
	copy(otherPk, pk)
	otherPk[10] ^= 0xff
	txEvil := buildTx(otherPk)
	var buf2 bytes.Buffer
	_ = txEvil.Serialize(&buf2)
	evil, err := RecomputeSegwitV0Sighash(buf2.Bytes(), 0, ws, amount)
	if err != nil {
		t.Fatalf("recompute evil: %v", err)
	}
	if bytes.Equal(evil, got) {
		t.Fatalf("sighash NOT output-committing — different outputs produced same digest")
	}

	// AllOutputsPay recognizes the successor and rejects the mismatch.
	if !AllOutputsPay(tx, pk) {
		t.Fatalf("AllOutputsPay should accept successor-paying tx")
	}
	if AllOutputsPay(txEvil, pk) {
		t.Fatalf("AllOutputsPay should reject non-successor output")
	}
	// Wrong amount => different digest (fail-safe: a lied amount can't match).
	wrongAmt, _ := RecomputeSegwitV0Sighash(txBytes, 0, ws, amount+1)
	if bytes.Equal(wrongAmt, got) {
		t.Fatalf("amount not committed in sighash")
	}
}
