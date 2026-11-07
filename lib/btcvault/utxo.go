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
// authorized ("ru-" and the id in decimal). An unmap reserves every input when
// it is built; a migration sweep reserves only its unconfirmed-pool inputs (ids
// below UtxoConfirmedPoolStart, migration.go migrateTranche) and keeps
// its confirmed inputs in its "ms-" record instead (MigrationSweepKey). The
// settle deletes the UTXO and the mark together.
func ReservedUtxoKey(id uint16) string { return "ru-" + strconv.FormatUint(uint64(id), 10) }

// MigrationSweepRegistryKey lists the BTC contract's in-flight migration
// sweeps (constants.go MigrationSweepRegistryKey "msl"): their txids, 32 raw
// bytes each, appended when a sweep is built and removed when it settles or is
// abandoned (MarshalTxSpendsRegistry).
const MigrationSweepRegistryKey = "msl"

// MigrationSweepKey is an in-flight migration sweep's record: "ms-" and the
// txid in lowercase hex (constants.go MigrationSweepPrefix).
func MigrationSweepKey(txid string) string { return "ms-" + txid }

// UnmarshalMigrationSweepRegistry splits the "msl" value into txids (hex).
func UnmarshalMigrationSweepRegistry(data []byte) ([]string, error) {
	if len(data)%32 != 0 {
		return nil, errors.New("migration sweep registry: length not a multiple of 32")
	}
	out := make([]string, 0, len(data)/32)
	for i := 0; i < len(data); i += 32 {
		out = append(out, hex.EncodeToString(data[i:i+32]))
	}
	return out, nil
}

// MigrationSweepInputIds returns the UTXO ids an "ms-" record spends, as the
// contract's MarshalMigrationSweep writes it: fee u64, successor generation
// u32, build height u32, n u16, n input ids u16, then the successor address.
func MigrationSweepInputIds(data []byte) ([]uint16, error) {
	const head = 8 + 4 + 4
	if len(data) < head+2 {
		return nil, errors.New("migration sweep record too short")
	}
	n := int(binary.BigEndian.Uint16(data[head:]))
	if len(data) < head+2+2*n {
		return nil, errors.New("migration sweep record: truncated input list")
	}
	ids := make([]uint16, n)
	for i := range ids {
		ids[i] = binary.BigEndian.Uint16(data[head+2+2*i:])
	}
	return ids, nil
}

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
