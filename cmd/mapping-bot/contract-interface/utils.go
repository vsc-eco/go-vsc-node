package contractinterface

import (
	"encoding/hex"
	"errors"
)

// ---------------------------------------------------------------------------
// TxSpendsRegistry binary encoding — 32 bytes per display-hex txid.
// ---------------------------------------------------------------------------

func MarshalTxSpendsRegistry(ts TxSpendsRegistry) []byte {
	buf := make([]byte, len(ts)*32)
	for i, txId := range ts {
		decoded, _ := hex.DecodeString(txId)
		copy(buf[i*32:], decoded)
	}
	return buf
}

func UnmarshalTxSpendsRegistry(data []byte) (TxSpendsRegistry, error) {
	if len(data)%32 != 0 {
		return nil, errors.New("invalid tx spends registry: length not a multiple of 32")
	}
	out := make(TxSpendsRegistry, len(data)/32)
	for i := range out {
		out[i] = hex.EncodeToString(data[i*32 : i*32+32])
	}
	return out, nil
}
