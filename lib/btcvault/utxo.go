package btcvault

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
)

// The BTC mapping contract's UTXO book, as its encoders write it
// (utxo-mapping btc-mapping-contract contract/mapping/utils.go
// MarshalUtxoRegistry, MarshalUtxo; constants.go UtxoPrefix, UtxoRegistryKey,
// ReservedUtxoPrefix, LastHeightKey). Read by the BTC solvency watch (N4).
const (
	// UtxoRegistryKey: packed 8-byte entries, a 2-byte id and the lower 6 bytes
	// of the amount (sats), big-endian.
	UtxoRegistryKey = "r"
	// UtxoConfirmedPoolStart: ids from here up are confirmed UTXOs, each
	// SPV-proven when registered; ids below are the unconfirmed pool (change
	// pending confirmation, and legacy entries that may not exist on Bitcoin).
	UtxoConfirmedPoolStart = 1024
	// BtcRelayedHeightKey: the last Bitcoin height relayed to the contract
	// (decimal).
	BtcRelayedHeightKey = "h"
)

// UtxoKey is a UTXO record's key: "u-" and the id in hex.
func UtxoKey(id uint16) string { return "u-" + strconv.FormatUint(uint64(id), 16) }

// ReservedUtxoKey marks a UTXO committed to an in-flight spend the contract
// authorized ("ru-" and the id in decimal): every unmap and migration sweep
// reserves its inputs when it is built, and the settle deletes the UTXO and the
// mark together.
func ReservedUtxoKey(id uint16) string { return "ru-" + strconv.FormatUint(uint64(id), 10) }

// UtxoRegistryEntry is one registered UTXO.
type UtxoRegistryEntry struct {
	Id     uint16
	Amount int64
}

// UnmarshalUtxoRegistry decodes the contract's "r" value.
func UnmarshalUtxoRegistry(data []byte) ([]UtxoRegistryEntry, error) {
	if len(data)%8 != 0 {
		return nil, errors.New("invalid utxo registry: length not a multiple of 8")
	}
	out := make([]UtxoRegistryEntry, len(data)/8)
	var tmp [8]byte
	for i := range out {
		off := i * 8
		out[i].Id = binary.BigEndian.Uint16(data[off:])
		copy(tmp[2:], data[off+2:off+8])
		out[i].Amount = int64(binary.BigEndian.Uint64(tmp[:]))
	}
	return out, nil
}

// UtxoOutpoint is the part of a UTXO record that locates it on Bitcoin.
type UtxoOutpoint struct {
	TxId   string // display hex, as bitcoind takes it
	Vout   uint32
	Amount int64
}

// UnmarshalUtxoOutpoint decodes the outpoint and amount at the head of a "u-"
// record (32-byte txid as display hex bytes, 4-byte vout, 8-byte amount, then
// the script, tag and generation the watch does not need).
func UnmarshalUtxoOutpoint(data []byte) (UtxoOutpoint, error) {
	const head = 32 + 4 + 8
	if len(data) < head+2 {
		return UtxoOutpoint{}, errors.New("utxo record too short")
	}
	return UtxoOutpoint{
		TxId:   hex.EncodeToString(data[:32]),
		Vout:   binary.BigEndian.Uint32(data[32:]),
		Amount: int64(binary.BigEndian.Uint64(data[36:])),
	}, nil
}
