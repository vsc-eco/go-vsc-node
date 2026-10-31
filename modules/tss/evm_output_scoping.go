package tss

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"vsc-node/lib/btcvault"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// A01 — output-scoped signing for the EVM bridge's rotating vault, the S3 gate
// of the BTC vault (output_scoping.go) in Ethereum terms. A superseded
// generation's key is the one being rotated away from because its shares may
// be reconstructed, so the node lends its share only for a migration sweep
// whose transaction pays the ACTIVE generation's address, derived from the
// COMMITTED tss_keys pubkey (never a contract field, the C-C guard), and
// nothing else. A pending generation signs only its check message. Same
// determinism as BTC: every input is consensus state at bh or committed
// tss_keys, so all honest nodes issue or all skip.

// evmScopeDeps are the injectable inputs of the pure EVM scoping core.
type evmScopeDeps struct {
	contractId    string
	readKeyStrict func(key string) ([]byte, bool, error)
	findKey       func(id string) (tss_db.TssKey, error)
}

// evmSweepVersion / evmSweepRecord mirror the bridge contract's SweepRecord
// (mapping/migration.go): only the fields the gate reads.
type evmSweepVersion struct {
	Unsigned string `json:"unsigned"`
}

type evmSweepRecord struct {
	Gen      uint32            `json:"gen"`
	Nonce    uint64            `json:"nonce"`
	Versions []evmSweepVersion `json:"versions"`
}

// evmUnsignedTx is the 0x02 payload the bridge signs: an EIP-1559 transaction
// without its signature.
type evmUnsignedTx struct {
	ChainID    *big.Int
	Nonce      uint64
	GasTipCap  *big.Int
	GasFeeCap  *big.Int
	Gas        uint64
	To         *common.Address `rlp:"nil"`
	Value      *big.Int
	Data       []byte
	AccessList types.AccessList
}

var erc20TransferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

func (d evmScopeDeps) keyName(gen uint32) string {
	return d.contractId + "-" + btcvault.VaultKeyNameFor(btcvault.EvmVaultKeyBase, gen)
}

// evaluate is the verdict for a signature by keyId over sighash.
func (d evmScopeDeps) evaluate(keyId string, sighash []byte) scopeVerdict {
	rawV, ok, err := d.readKeyStrict("v")
	if err != nil {
		log.Warn("evm output-scoping: vault registry unreadable, refusing (fail closed)", "contract", d.contractId, "err", err)
		return scopeRefuse
	}
	if !ok || len(rawV) == 0 {
		// Pre-rotation bridge: its one key is the vault, unscoped as before.
		if keyId == d.keyName(0) {
			return scopeAllow
		}
		return scopeRefuse
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(rawV)
	if err != nil || len(vaults) == 0 {
		return scopeRefuse
	}
	byKey := make(map[string]btcvault.Vault, len(vaults))
	active := -1
	actives := 0
	for i, v := range vaults {
		k := d.keyName(v.Generation)
		if _, dup := byKey[k]; dup {
			return scopeRefuse // a duplicate generation is corrupt state
		}
		byKey[k] = v
		if v.Status == btcvault.VaultStatusActive {
			active = i
			actives++
		}
	}
	v, known := byKey[keyId]
	if !known {
		return scopeRefuse
	}
	if actives != 1 {
		// Fresh genesis: one Pending generation, nothing active. It may sign its
		// check message, nothing else; any other shape is corrupt.
		if len(vaults) == 1 && v.Status == btcvault.VaultStatusPending && d.pendingCheckSig(keyId, v.Generation, sighash) {
			return scopeCheckSig
		}
		return scopeRefuse
	}
	switch v.Status {
	case btcvault.VaultStatusActive:
		return scopeAllow // user withdrawals: the contract decides where users' funds go
	case btcvault.VaultStatusPending:
		if d.pendingCheckSig(keyId, v.Generation, sighash) {
			return scopeCheckSig
		}
		return scopeRefuse
	case btcvault.VaultStatusRetiring, btcvault.VaultStatusDraining:
		if d.sweepPaysSuccessor(v.Generation, d.keyName(vaults[active].Generation), sighash) {
			return scopeSuccessorSweep
		}
		if d.ownInFlightWithdrawal(v.Generation, sighash) {
			return scopeOwnWithdrawal
		}
		return scopeRefuse
	default:
		return scopeRefuse // inactive / purged: holds nothing to sign for
	}
}

// committedPub is a committed, active key's 33-byte public key.
func (d evmScopeDeps) committedPub(keyId string) ([]byte, bool) {
	k, err := d.findKey(keyId)
	if err != nil || k.PublicKey == "" || k.Status != tss_db.TssKeyActive {
		return nil, false
	}
	pub, err := hex.DecodeString(strings.TrimPrefix(k.PublicKey, "0x"))
	if err != nil || len(pub) != 33 {
		return nil, false
	}
	return pub, true
}

// pendingCheckSig: the BRK-2 check message of this generation's committed key,
// or the bridge's own vault check digest of it (attestVault, an EIP-191 text
// signature). Neither is a transaction; neither moves funds.
func (d evmScopeDeps) pendingCheckSig(keyId string, gen uint32, sighash []byte) bool {
	pub, ok := d.committedPub(keyId)
	if !ok {
		return false
	}
	if bytes.Equal(btcvault.CheckSigMessage(keyId, gen, pub), sighash) {
		return true
	}
	chainId, ok := d.chainId()
	if !ok {
		return false
	}
	return bytes.Equal(evmVaultCheckDigest(d.contractId, chainId, hex.EncodeToString(pub)), sighash)
}

// evmVaultCheckDigest mirrors the bridge's VaultCheckDigest (mapping/vaultcheck.go).
func evmVaultCheckDigest(contractId string, chainId uint64, pubHex string) []byte {
	msg := "magi evm vault check-signature v1:" + contractId + ":" + strconv.FormatUint(chainId, 10) + ":" + pubHex
	return crypto.Keccak256([]byte("\x19Ethereum Signed Message:\n" + strconv.Itoa(len(msg)) + msg))
}

func (d evmScopeDeps) chainId() (uint64, bool) {
	raw, ok, err := d.readKeyStrict("chainid")
	if err != nil || !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	return n, err == nil && n != 0
}

// sweepPaysSuccessor reports whether sighash is keccak of a signed version of an
// open sweep of generation gen whose transaction pays only the successor.
// Fails closed on any doubt.
func (d evmScopeDeps) sweepPaysSuccessor(gen uint32, successorKeyId string, sighash []byte) bool {
	pub, ok := d.committedPub(successorKeyId)
	if !ok {
		log.Warn("evm output-scoping: successor key not committed/active, refusing sweep", "successor", successorKeyId)
		return false
	}
	ecpub, err := crypto.DecompressPubkey(pub)
	if err != nil {
		return false
	}
	successor := crypto.PubkeyToAddress(*ecpub)
	chainId, ok := d.chainId()
	if !ok {
		return false
	}
	rawList, ok, err := d.readKeyStrict("msr")
	if err != nil || !ok {
		return false
	}
	prefix := strconv.FormatUint(uint64(gen), 10) + "-"
	for _, k := range strings.Split(string(rawList), "|") {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		raw, ok, err := d.readKeyStrict("ms-" + k)
		if err != nil || !ok {
			continue
		}
		var rec evmSweepRecord
		if json.Unmarshal(raw, &rec) != nil || rec.Gen != gen || prefix+strconv.FormatUint(rec.Nonce, 10) != k {
			continue
		}
		for _, ver := range rec.Versions {
			unsigned, err := hex.DecodeString(ver.Unsigned)
			if err != nil || !bytes.Equal(crypto.Keccak256(unsigned), sighash) {
				continue
			}
			// The digest is this version's: it signs THIS transaction, so the
			// transaction must pay only the successor.
			return evmTxPaysOnly(unsigned, chainId, successor)
		}
	}
	return false
}

// evmMaxInFlightWithdrawals mirrors the bridge's MaxInFlightWithdrawals: a
// cut-over leaves at most this many of the retiring generation's withdrawals
// open, at the nonces just below its ceiling.
const evmMaxInFlightWithdrawals = 8

// evmPendingSpend is the part of the bridge's PendingSpend (d-{nonce}) the gate
// reads.
type evmPendingSpend struct {
	UnsignedTxHex string `json:"unsigned_tx_hex"`
	VaultAtQueue  string `json:"vault_at_queue"`
}

// ownInFlightWithdrawal reports whether sighash signs one of generation gen's
// own withdrawals still open from before its cut-over (A01 ROT-HALT-1): the
// bridge cuts over without waiting for them, and the retiring key must be able
// to sign a replacement (higher fee) or a roll of one, or its nonce, and every
// sweep after it, would stay unspent. The transaction must be the bridge's
// record at d-{n}, issued from this generation's address (derived from its
// committed key), at n below the generation's ceiling wnc-{gen} (the first
// nonce after the withdrawals it issued while active), on this chain. At most
// evmMaxInFlightWithdrawals records are read.
func (d evmScopeDeps) ownInFlightWithdrawal(gen uint32, sighash []byte) bool {
	pub, ok := d.committedPub(d.keyName(gen))
	if !ok {
		return false
	}
	ecpub, err := crypto.DecompressPubkey(pub)
	if err != nil {
		return false
	}
	own := crypto.PubkeyToAddress(*ecpub)
	chainId, ok := d.chainId()
	if !ok {
		return false
	}
	raw, ok, err := d.readKeyStrict("wnc-" + strconv.FormatUint(uint64(gen), 10))
	if err != nil || !ok {
		return false
	}
	ceiling, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return false
	}
	lo := uint64(0)
	if ceiling > evmMaxInFlightWithdrawals {
		lo = ceiling - evmMaxInFlightWithdrawals
	}
	for n := lo; n < ceiling; n++ {
		raw, ok, err := d.readKeyStrict("d-" + strconv.FormatUint(n, 10))
		if err != nil || !ok {
			continue
		}
		var ps evmPendingSpend
		if json.Unmarshal(raw, &ps) != nil || !strings.EqualFold(ps.VaultAtQueue, own.Hex()) {
			continue
		}
		unsigned, err := hex.DecodeString(ps.UnsignedTxHex)
		if err != nil || !bytes.Equal(crypto.Keccak256(unsigned), sighash) {
			continue
		}
		return evmTxAt(unsigned, chainId, n)
	}
	return false
}

// evmTxAt reports whether unsigned is an EIP-1559 transaction on chainId at
// nonce.
func evmTxAt(unsigned []byte, chainId, nonce uint64) bool {
	if len(unsigned) < 2 || unsigned[0] != types.DynamicFeeTxType {
		return false
	}
	var tx evmUnsignedTx
	if rlp.DecodeBytes(unsigned[1:], &tx) != nil || tx.ChainID == nil {
		return false
	}
	return tx.ChainID.Cmp(new(big.Int).SetUint64(chainId)) == 0 && tx.Nonce == nonce
}

// evmTxPaysOnly decodes an unsigned EIP-1559 transaction and reports whether it
// can only move funds to successor: an ETH transfer to it with no calldata, or
// a zero-value ERC-20 transfer(successor, x). An approve, any other call, a
// value with calldata, another type, another chain or trailing bytes fail.
func evmTxPaysOnly(unsigned []byte, chainId uint64, successor common.Address) bool {
	if len(unsigned) < 2 || unsigned[0] != types.DynamicFeeTxType {
		return false
	}
	var tx evmUnsignedTx
	if rlp.DecodeBytes(unsigned[1:], &tx) != nil {
		return false
	}
	if tx.To == nil || tx.ChainID == nil || tx.ChainID.Cmp(new(big.Int).SetUint64(chainId)) != 0 || tx.Value == nil {
		return false
	}
	if tx.Value.Sign() > 0 {
		return *tx.To == successor && len(tx.Data) == 0
	}
	if len(tx.Data) != 68 || !bytes.Equal(tx.Data[:4], erc20TransferSelector) {
		return false
	}
	var padded [32]byte
	copy(padded[12:], successor[:])
	return bytes.Equal(tx.Data[4:36], padded[:])
}

// evmSignRefused is the pre-issuance gate for a rotating EVM vault's key: true
// when THIS node must skip lending its share. Other keys pass untouched.
func (tssMgr *TssManager) evmSignRefused(keyId string, sighash []byte, bh uint64) bool {
	contract, ok := tssMgr.evmRotatingKey(keyId, bh)
	if !ok {
		return false
	}
	deps := evmScopeDeps{
		contractId:    contract,
		readKeyStrict: tssMgr.contractStateReaderAtStrict(contract, bh),
		findKey:       tssMgr.tssKeys.FindKey,
	}
	switch verdict := deps.evaluate(keyId, sighash); verdict {
	case scopeRefuse:
		log.Warn("EVM vault keysign refused by output scoping (A01)", "keyId", keyId, "bh", bh)
		return true
	case scopeCheckSig:
		log.Info("EVM vault keysign is a pending generation's check signature; issuing", "keyId", keyId, "bh", bh)
	case scopeSuccessorSweep:
		log.Info("EVM vault keysign is a sweep to the active generation; issuing", "keyId", keyId, "bh", bh)
	case scopeOwnWithdrawal:
		log.Info("EVM vault keysign re-signs a withdrawal the generation issued before its cut-over; issuing", "keyId", keyId, "bh", bh)
	}
	return false
}
