package btcvault

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// CSV timelock for the backup spending path, mirror of
// contract/constants/constants.go. The contract selects mainnet vs testnet by
// network.Net; this MUST match or the derived successor scriptPubKey (and hence
// the output-scoping check) diverges from what the contract actually built.
const (
	BackupCSVBlocksMainnet = 4320 // ~1 month
	BackupCSVBlocksTestnet = 2
)

// CSVBlocksForNet returns the CSV block count the contract uses for net, mirror
// of contract/mapping/utils.go createP2WSHAddressWithBackup:
// mainnet => 4320, any other network => 2.
func CSVBlocksForNet(net *chaincfg.Params) int {
	if net.Net == chaincfg.MainNetParams.Net {
		return BackupCSVBlocksMainnet
	}
	return BackupCSVBlocksTestnet
}

// SuccessorPkScript derives a vault generation's canonical (tag-less) P2WSH
// scriptPubKey — the exact thing a migration sweep's outputs must pay. It is a
// byte-for-byte mirror of the contract's successor derivation
// (contract/mapping/migration.go HandleMigrateVault →
// createP2WSHAddressWithBackup(primary, backup, nil, net) → PayToAddrScript),
// which uses a NIL tag (⇒ the primary path is OP_CHECKSIG, not
// OP_CHECKSIGVERIFY + tag).
//
// primaryHex is the COMMITTED successor pubkey read from tss_keys (never a
// contract field — the C-C guard); backupHex is the immutable/lineage-pinned
// backup. Returns (witnessScript, pkScript). pkScript is what to compare
// against each tx output's PkScript.
func SuccessorPkScript(primaryHex, backupHex string, csvBlocks int, net *chaincfg.Params) (witnessScript, pkScript []byte, err error) {
	primary, err := hex.DecodeString(primaryHex)
	if err != nil {
		return nil, nil, fmt.Errorf("btcvault: primary pubkey hex: %w", err)
	}
	backup, err := hex.DecodeString(backupHex)
	if err != nil {
		return nil, nil, fmt.Errorf("btcvault: backup pubkey hex: %w", err)
	}
	if len(primary) != CompressedPubKeyLen || len(backup) != CompressedPubKeyLen {
		return nil, nil, fmt.Errorf("btcvault: pubkey length primary=%d backup=%d, want %d", len(primary), len(backup), CompressedPubKeyLen)
	}
	return successorPkScriptRaw(primary, backup, csvBlocks, net)
}

// successorPkScriptRaw is SuccessorPkScript over raw (already-decoded) pubkey
// bytes — the form the vault registry stores.
func successorPkScriptRaw(primary, backup []byte, csvBlocks int, net *chaincfg.Params) (witnessScript, pkScript []byte, err error) {
	sb := txscript.NewScriptBuilder()
	sb.AddOp(txscript.OP_IF)
	// Primary path — NIL tag ⇒ OP_CHECKSIG (the tag-less vault/change script).
	sb.AddData(primary)
	sb.AddOp(txscript.OP_CHECKSIG)
	// Backup path — CSV timelock.
	sb.AddOp(txscript.OP_ELSE)
	sb.AddInt64(int64(csvBlocks))
	sb.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	sb.AddOp(txscript.OP_DROP)
	sb.AddData(backup)
	sb.AddOp(txscript.OP_CHECKSIG)
	sb.AddOp(txscript.OP_ENDIF)

	witnessScript, err = sb.Script()
	if err != nil {
		return nil, nil, fmt.Errorf("btcvault: build witness script: %w", err)
	}
	h := sha256.Sum256(witnessScript)
	addr, err := btcutil.NewAddressWitnessScriptHash(h[:], net)
	if err != nil {
		return nil, nil, fmt.Errorf("btcvault: witness script hash address: %w", err)
	}
	pkScript, err = txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("btcvault: pay-to-addr script: %w", err)
	}
	return witnessScript, pkScript, nil
}

// SuccessorPkScriptRaw is the exported raw-bytes form (registry-stored pubkeys).
func SuccessorPkScriptRaw(primary, backup []byte, csvBlocks int, net *chaincfg.Params) (witnessScript, pkScript []byte, err error) {
	if len(primary) != CompressedPubKeyLen || len(backup) != CompressedPubKeyLen {
		return nil, nil, fmt.Errorf("btcvault: pubkey length primary=%d backup=%d, want %d", len(primary), len(backup), CompressedPubKeyLen)
	}
	return successorPkScriptRaw(primary, backup, csvBlocks, net)
}

// ParseTx deserializes a wire.MsgTx from the SigningData.Tx bytes.
func ParseTx(txBytes []byte) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(txBytes)); err != nil {
		return nil, fmt.Errorf("btcvault: deserialize tx: %w", err)
	}
	return tx, nil
}

// RecomputeSegwitV0Sighash independently recomputes the BIP143 (segwit-v0)
// sighash for one input, byte-for-byte mirror of the contract's
// signSpendTransaction (contract/mapping/unmapping.go): a canned prevout
// fetcher (same value for every input — harmless for a v0-only tx, the fetcher
// is not consulted for the v0 midstate), SigHashAll, over witnessScript and
// amountSats. The input's scriptPubKey is reconstructed as P2WSH(witnessScript)
// so the fetcher matches what the contract passed (utxo.PkScript); for a v0
// sighash the value is immaterial but we keep it faithful.
//
// The recomputed digest MUST equal action.Args before a retiring-gen share is
// contributed — this is what binds the (untrusted) template to the signature: a
// SigHashAll digest commits to the outputs, so a signature over it can only
// attach to a tx paying those exact outputs.
func RecomputeSegwitV0Sighash(txBytes []byte, inputIndex int, witnessScript []byte, amountSats int64) ([]byte, error) {
	tx, err := ParseTx(txBytes)
	if err != nil {
		return nil, err
	}
	if inputIndex < 0 || inputIndex >= len(tx.TxIn) {
		return nil, fmt.Errorf("btcvault: input index %d out of range (%d inputs)", inputIndex, len(tx.TxIn))
	}
	// Reconstruct the spent input's P2WSH scriptPubKey = OP_0 <sha256(witnessScript)>.
	h := sha256.Sum256(witnessScript)
	inputPkScript, err := txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(h[:]).Script()
	if err != nil {
		return nil, fmt.Errorf("btcvault: build input pkScript: %w", err)
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(inputPkScript, amountSats)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	sigHash, err := txscript.CalcWitnessSigHash(witnessScript, sigHashes, txscript.SigHashAll, tx, inputIndex, amountSats)
	if err != nil {
		return nil, fmt.Errorf("btcvault: calc witness sighash: %w", err)
	}
	return sigHash, nil
}

// AllOutputsPay reports whether EVERY output of tx pays pkScript (a migration
// sweep to a successor pays only the successor — a single output, or a
// successor-paying change output; either way every output must be the
// successor). Mirror of contract/mapping/migration.go assertOutputsPaySuccessor.
func AllOutputsPay(tx *wire.MsgTx, pkScript []byte) bool {
	if len(tx.TxOut) == 0 {
		return false
	}
	for _, out := range tx.TxOut {
		if !bytes.Equal(out.PkScript, pkScript) {
			return false
		}
	}
	return true
}
