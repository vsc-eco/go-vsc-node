// Package btcvault provides the minimal, dependency-light BTC primitives the
// node's TSS layer needs to enforce output-scoped retiring-key signing (S3 /
// non-negotiable #1 — "don't sign blind"): decode the mapping contract's vault
// registry + pending-spend SigningData, derive a successor vault's P2WSH
// scriptPubKey, and recompute a BIP143 (segwit-v0) sighash so the sign
// dispatcher can independently verify what it is being asked to sign.
//
// It lives under lib/ (not cmd/mapping-bot) so it can be imported from
// modules/, exactly like lib/btcclient — the mapping-bot's chain/ and
// contract-interface/ packages sit under a leaf-binary tree that modules/ must
// not depend on. Every function here MIRRORS a contract-side counterpart
// byte-for-byte (utxo-mapping/btc-mapping-contract): the vault registry layout
// (contract/mapping/utils.go MarshalVaultRegistry), the P2WSH script
// (contract/mapping/utils.go createP2WSHAddressWithBackup with a nil tag), and
// the sighash (contract/mapping/unmapping.go signSpendTransaction). Any drift
// from those makes the node reject legitimate migration sweeps.
package btcvault

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
)

// VaultStatus mirrors contract/mapping/types.go VaultStatus (uint8).
type VaultStatus uint8

const (
	VaultStatusPending  VaultStatus = 0
	VaultStatusActive   VaultStatus = 1
	VaultStatusRetiring VaultStatus = 2
	VaultStatusDraining VaultStatus = 3
	VaultStatusInactive VaultStatus = 4
	VaultStatusPurged   VaultStatus = 5
)

// VaultEntrySize is the fixed packed width of one vault registry entry, mirror
// of contract/mapping/types.go VaultEntrySize (4 gen + 33 primary + 33 backup +
// 1 status + 4 predecessor + 4*4 heights = 91 bytes).
//
// ★ MUST equal the contract's VaultEntrySize byte-for-byte (this decodes the
// contract's "v" state key). S5 grew it 87→91 to carry InactiveHeight; this mirror
// moved in lock-step. Deploy-order (cross-repo): the contract that writes 91-byte
// entries and this node that reads them must ship together — a stride mismatch
// misaligns every entry past the first (and len%stride rejects a mixed blob).
const VaultEntrySize = 91

// CompressedPubKeyLen is the byte length of a compressed secp256k1 pubkey.
const CompressedPubKeyLen = 33

// Vault mirrors contract/mapping/types.go Vault. Primary/Backup are raw 33-byte
// compressed pubkeys as stored in the registry blob.
type Vault struct {
	Generation      uint32
	Primary         []byte // 33 bytes
	Backup          []byte // 33 bytes
	Status          VaultStatus
	Predecessor     uint32
	CreatedHeight   uint32
	ActivatedHeight uint32
	RetiredHeight   uint32
	InactiveHeight  uint32 // S5: BTC height the gen went DRAINING→INACTIVE (purge-grace anchor)
}

// UnmarshalVaultRegistry decodes the contract "v" state key: a contiguous blob
// of VaultEntrySize-byte big-endian entries, no delimiters. Byte-for-byte
// mirror of contract/mapping/utils.go UnmarshalVaultRegistry. An empty blob
// yields an empty registry (pre-fold / fresh deploy) — not an error.
func UnmarshalVaultRegistry(data []byte) ([]Vault, error) {
	if len(data)%VaultEntrySize != 0 {
		return nil, errors.New("btcvault: vault registry length not a multiple of VaultEntrySize")
	}
	out := make([]Vault, len(data)/VaultEntrySize)
	for i := range out {
		off := i * VaultEntrySize
		out[i].Generation = binary.BigEndian.Uint32(data[off:])
		primary := make([]byte, CompressedPubKeyLen)
		copy(primary, data[off+4:off+37])
		backup := make([]byte, CompressedPubKeyLen)
		copy(backup, data[off+37:off+70])
		out[i].Primary = primary
		out[i].Backup = backup
		out[i].Status = VaultStatus(data[off+70])
		out[i].Predecessor = binary.BigEndian.Uint32(data[off+71:])
		out[i].CreatedHeight = binary.BigEndian.Uint32(data[off+75:])
		out[i].ActivatedHeight = binary.BigEndian.Uint32(data[off+79:])
		out[i].RetiredHeight = binary.BigEndian.Uint32(data[off+83:])
		out[i].InactiveHeight = binary.BigEndian.Uint32(data[off+87:])
	}
	return out, nil
}

// ReadUint32BE decodes a 4-byte big-endian uint32 (the "vn"/"va" counters).
// Returns (0, false) on a wrong length so a malformed read fails closed.
func ReadUint32BE(data []byte) (uint32, bool) {
	if len(data) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(data), true
}

// UnmarshalTxSpendsRegistry decodes the contract "p" (TxSpendsRegistry) state
// key: contiguous 32-byte raw txids. Returns lowercase-hex txid strings, exactly
// the keys the contract used to store each SigningData under "d-<txidHex>" — a
// byte-for-byte mirror of contract/mapping/utils.go UnmarshalTxSpendsRegistry.
func UnmarshalTxSpendsRegistry(data []byte) ([]string, error) {
	if len(data)%32 != 0 {
		return nil, errors.New("btcvault: tx spends registry length not a multiple of 32")
	}
	out := make([]string, len(data)/32)
	for i := range out {
		out[i] = hex.EncodeToString(data[i*32 : i*32+32])
	}
	return out, nil
}

// VaultKeyName mirrors contract/mapping/utils.go VaultKeyId: generation 0 keeps
// the legacy "main" name, generation N uses "mainv<N>" (no separator — the TSS
// runtime rejects non-alphanumeric key names). This is the KEY NAME only; the
// full node keyId is "<contractId>-" + VaultKeyName(gen).
func VaultKeyName(gen uint32) string {
	if gen == 0 {
		return "main"
	}
	return "main" + "v" + strconv.FormatUint(uint64(gen), 10)
}

// IsFundHoldingStatus mirrors contract/mapping/vault_lifecycle.go
// isFundHoldingStatus: the {Active, Retiring, Draining} set whose keys must stay
// signable. A Retiring/Draining gen is exactly the one whose keysign the node
// must output-scope to the successor.
func IsFundHoldingStatus(s VaultStatus) bool {
	return s == VaultStatusActive || s == VaultStatusRetiring || s == VaultStatusDraining
}
