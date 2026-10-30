package mapper

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/assert"
)

// C1: confirmSpend's indices are OUTPUT vouts, not input indices. A legacy 1-input unmap
// puts the recipient at vout 0 and the vault change at vout 1; the old bot sent the signed
// INPUT index [0], which never matched the change at vout 1, so the unmap could not settle.
// confirmSpendOutputVouts lists every output vout so the contract can promote the change
// wherever it sits (it keeps only this tx's unconfirmed change and ignores the rest).
func TestC1_ConfirmSpendUsesOutputVouts(t *testing.T) {
	p2wpkh := append([]byte{0x00, 0x14}, make([]byte, 20)...)

	// 1 input, 2 outputs (recipient at vout 0, change at vout 1): the failing C1 shape.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}, Sequence: wire.MaxTxInSequenceNum})
	tx.AddTxOut(&wire.TxOut{Value: 9000, PkScript: p2wpkh})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: p2wpkh})
	assert.Equal(t, []uint32{0, 1}, confirmSpendOutputVouts(tx),
		"indices must be output vouts (so the change at vout 1 is covered), not the input index [0]")

	// A single-output spend (e.g. a migration sweep) lists [0].
	tx2 := wire.NewMsgTx(2)
	tx2.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 0}, Sequence: wire.MaxTxInSequenceNum})
	tx2.AddTxOut(&wire.TxOut{Value: 10000, PkScript: p2wpkh})
	assert.Equal(t, []uint32{0}, confirmSpendOutputVouts(tx2))
}
