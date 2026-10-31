//go:build evm_devnet

// Anvil L1 harness for the EVM-bridge waves (W1/W2/W3). A fresh anvil at the
// contract's chainId is the L1: deposits (ETH -> vault) and withdrawals
// (vault -> recipient) land in real anvil blocks whose REAL roots
// (transactionsRoot/receiptsRoot) are then planted into the mock verifier via
// devSetHeader, so the contract's tx-trie / receipt-trie proofs verify against
// genuine Ethereum-encoded tries. No Sepolia fork needed — fresh anvil blocks
// carry real canonical roots.
package devnet

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"testing"
	"time"

	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	rawdb "github.com/ethereum/go-ethereum/core/rawdb"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	ethclient "github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	ethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// proofList collects ordered MPT proof nodes (implements ethdb.KeyValueWriter).
type proofList [][]byte

func (p *proofList) Put(key, value []byte) error { *p = append(*p, value); return nil }
func (p *proofList) Delete(key []byte) error     { return nil }

// Anvil is a running anvil process + clients.
type Anvil struct {
	cmd     *exec.Cmd
	URL     string
	ChainID *big.Int
	cli     *ethclient.Client
	rpc     *ethrpc.Client
}

// StartAnvil launches a fresh anvil bound to chainID on the given port.
func StartAnvil(t *testing.T, ctx context.Context, chainID uint64, port string) *Anvil {
	bin := "anvil"
	if _, err := exec.LookPath("anvil"); err != nil {
		bin = os.ExpandEnv("$HOME/.foundry/bin/anvil")
	}
	// Something already answering on the port (an anvil orphaned by an earlier
	// run) would pass the readiness check below, and the wave would run against
	// its hours-long chain.
	if !portFree(port) {
		t.Fatalf("anvil port %s is already in use (an anvil left over from an earlier run?)", port)
	}
	cmd := exec.CommandContext(ctx, bin,
		"--port", port,
		"--chain-id", fmt.Sprintf("%d", chainID),
		"--block-time", "1", // auto-mine every 1s so finalized advances
		"--silent",
	)
	cmd.SysProcAttr = dieWithTest()
	if err := cmd.Start(); err != nil {
		t.Fatalf("anvil start: %v", err)
	}
	url := "http://127.0.0.1:" + port
	a := &Anvil{cmd: cmd, URL: url, ChainID: new(big.Int).SetUint64(chainID)}
	// Wait for RPC up.
	deadline := devnetNow().Add(20 * time.Second)
	for {
		rc, err := ethrpc.DialContext(ctx, url)
		if err == nil {
			a.rpc = rc
			a.cli = ethclient.NewClient(rc)
			if _, e := a.cli.ChainID(ctx); e == nil {
				break
			}
		}
		if devnetNow().After(deadline) {
			t.Fatalf("anvil RPC never came up at %s", url)
		}
		devnetSleep(300 * time.Millisecond)
	}
	return a
}

func (a *Anvil) Stop() {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
}

// SetBalance funds an address with wei via anvil_setBalance.
func (a *Anvil) SetBalance(ctx context.Context, addr ethcommon.Address, wei *big.Int) error {
	return a.rpc.CallContext(ctx, nil, "anvil_setBalance", addr.Hex(), "0x"+wei.Text(16))
}

// Mine advances n blocks (anvil_mine).
func (a *Anvil) Mine(ctx context.Context, n int) error {
	return a.rpc.CallContext(ctx, nil, "anvil_mine", fmt.Sprintf("0x%x", n))
}

// SendETH signs+sends an EIP-1559 ETH transfer from key to `to`, returns the tx
// hash and the block number it mined in.
func (a *Anvil) SendETH(ctx context.Context, key *ecdsa.PrivateKey, to ethcommon.Address, valueWei *big.Int) (ethcommon.Hash, uint64, error) {
	from := cryptoPubkeyAddr(key)
	nonce, err := a.cli.PendingNonceAt(ctx, from)
	if err != nil {
		return ethcommon.Hash{}, 0, fmt.Errorf("nonce: %w", err)
	}
	tip := big.NewInt(1_000_000_000)     // 1 gwei
	feeCap := big.NewInt(20_000_000_000) // 20 gwei
	tx := ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID:   a.ChainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       21000,
		To:        &to,
		Value:     valueWei,
	})
	signed, err := ethtypes.SignTx(tx, ethtypes.LatestSignerForChainID(a.ChainID), key)
	if err != nil {
		return ethcommon.Hash{}, 0, fmt.Errorf("sign: %w", err)
	}
	if err := a.cli.SendTransaction(ctx, signed); err != nil {
		return ethcommon.Hash{}, 0, fmt.Errorf("send: %w", err)
	}
	rcpt, err := a.waitReceipt(ctx, signed.Hash(), 30*time.Second)
	if err != nil {
		return ethcommon.Hash{}, 0, err
	}
	return signed.Hash(), rcpt.BlockNumber.Uint64(), nil
}

// SendRawTx broadcasts a raw signed tx (e.g. the bot's vault withdrawal),
// returns the tx hash + mined block.
func (a *Anvil) SendRawTx(ctx context.Context, raw []byte) (ethcommon.Hash, uint64, error) {
	tx := new(ethtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return ethcommon.Hash{}, 0, fmt.Errorf("decode raw tx: %w", err)
	}
	if err := a.cli.SendTransaction(ctx, tx); err != nil {
		return ethcommon.Hash{}, 0, fmt.Errorf("send raw: %w", err)
	}
	rcpt, err := a.waitReceipt(ctx, tx.Hash(), 30*time.Second)
	if err != nil {
		return ethcommon.Hash{}, 0, err
	}
	return tx.Hash(), rcpt.BlockNumber.Uint64(), nil
}

func (a *Anvil) waitReceipt(ctx context.Context, h ethcommon.Hash, timeout time.Duration) (*ethtypes.Receipt, error) {
	deadline := devnetNow().Add(timeout)
	for {
		r, err := a.cli.TransactionReceipt(ctx, h)
		if err == nil && r != nil {
			return r, nil
		}
		if devnetNow().After(deadline) {
			return nil, fmt.Errorf("receipt for %s not found within %s", h.Hex(), timeout)
		}
		devnetSleep(300 * time.Millisecond)
	}
}

// AnvilHeader holds the fields the mock verifier plants (137-byte review6 layout).
type AnvilHeader struct {
	Number       uint64
	StateRoot    string // 0x-less 64-hex
	TxRoot       string
	ReceiptsRoot string
	BaseFee      uint64
	GasLimit     uint64
	Timestamp    uint64
}

// blockRaw is block num's header, tx hashes and (with raw) raw txs, read
// without decoding the txs: go-ethereum v1.14.12 has no EIP-7702 (type 4) tx
// type, so BlockByNumber fails on any block holding one (live on Sepolia:
// phase C5's vault scan stopped at block 12012224).
func blockRaw(ctx context.Context, cli *ethclient.Client, rc *ethrpc.Client, num uint64, raw bool) (*ethtypes.Header, []ethcommon.Hash, [][]byte, error) {
	h, err := cli.HeaderByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("header %d: %w", num, err)
	}
	var blk struct {
		Transactions []ethcommon.Hash `json:"transactions"`
	}
	if err := rc.CallContext(ctx, &blk, "eth_getBlockByNumber", hexutil.EncodeUint64(num), false); err != nil {
		return nil, nil, nil, fmt.Errorf("block %d: %w", num, err)
	}
	if !raw {
		return h, blk.Transactions, nil, nil
	}
	raws := make([][]byte, len(blk.Transactions))
	for i, txh := range blk.Transactions {
		var b hexutil.Bytes
		if err := rc.CallContext(ctx, &b, "eth_getRawTransactionByHash", txh); err != nil {
			return nil, nil, nil, fmt.Errorf("raw tx %d of block %d: %w", i, num, err)
		}
		raws[i] = b
	}
	return h, blk.Transactions, raws, nil
}

// encodeReceipt is r's receipt-trie encoding for any tx type: v1.14.12's
// Receipts.EncodeIndex writes only the type byte for a type it does not know
// (EIP-7702 type 4). A typed receipt is its type byte then the same RLP.
func encodeReceipt(r *ethtypes.Receipt) []byte {
	c := *r
	if c.Type != ethtypes.LegacyTxType {
		c.Type = ethtypes.DynamicFeeTxType
	}
	var buf bytes.Buffer
	ethtypes.Receipts{&c}.EncodeIndex(0, &buf)
	b := buf.Bytes()
	if r.Type != ethtypes.LegacyTxType {
		b[0] = r.Type
	}
	return b
}

// sentAt finds the tx `from` sent at `nonce` in block n from the block's JSON
// (no tx decoding, see blockRaw); ok=false when the block has none.
func sentAt(ctx context.Context, cli *ethclient.Client, rc *ethrpc.Client, n uint64, from ethcommon.Address, nonce uint64) (*ethtypes.Transaction, bool, error) {
	var blk struct {
		Transactions []struct {
			Hash  ethcommon.Hash    `json:"hash"`
			From  ethcommon.Address `json:"from"`
			Nonce hexutil.Uint64    `json:"nonce"`
		} `json:"transactions"`
	}
	if err := rc.CallContext(ctx, &blk, "eth_getBlockByNumber", hexutil.EncodeUint64(n), true); err != nil {
		return nil, false, err
	}
	for _, t := range blk.Transactions {
		if t.From == from && uint64(t.Nonce) == nonce {
			tx, _, err := cli.TransactionByHash(ctx, t.Hash)
			if err != nil {
				return nil, false, err
			}
			return tx, true, nil
		}
	}
	return nil, false, nil
}

// Header reads block `num`'s real header fields for planting.
func (a *Anvil) Header(ctx context.Context, num uint64) (*AnvilHeader, error) {
	h, err := a.cli.HeaderByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", num, err)
	}
	bf := uint64(0)
	if h.BaseFee != nil {
		bf = h.BaseFee.Uint64()
	}
	return &AnvilHeader{
		Number:       h.Number.Uint64(),
		StateRoot:    h.Root.Hex()[2:],
		TxRoot:       h.TxHash.Hex()[2:],
		ReceiptsRoot: h.ReceiptHash.Hex()[2:],
		BaseFee:      bf,
		GasLimit:     h.GasLimit,
		Timestamp:    h.Time,
	}, nil
}

// Finalized returns the current finalized block number (the bot scans only up to it).
func (a *Anvil) Finalized(ctx context.Context) (uint64, error) {
	var head struct {
		Number string `json:"number"`
	}
	if err := a.rpc.CallContext(ctx, &head, "eth_getBlockByNumber", "finalized", false); err != nil {
		return 0, err
	}
	n := new(big.Int)
	n.SetString(head.Number[2:], 16)
	return n.Uint64(), nil
}

func cryptoPubkeyAddr(key *ecdsa.PrivateKey) ethcommon.Address {
	return ethCrypto.PubkeyToAddress(key.PublicKey)
}

// TxTrieProof builds the tx-trie proof for txIndex in block `num`: returns the
// raw typed-tx hex + the concatenated-RLP proof-node hex that the contract's
// splitProofNodes/mpt.VerifyProof expects. It reconstructs the canonical tx
// trie and asserts its root == the block's real transactionsRoot BEFORE
// returning, so a format error is caught locally (not on devnet).
func (a *Anvil) TxTrieProof(ctx context.Context, num, txIndex uint64) (rawHex, proofHex string, err error) {
	h, _, txs, err := blockRaw(ctx, a.cli, a.rpc, num, true)
	if err != nil {
		return "", "", err
	}
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	if txIndex >= uint64(len(txs)) {
		return "", "", fmt.Errorf("txIndex %d out of range (%d txs)", txIndex, len(txs))
	}
	for i, val := range txs {
		key, _ := rlp.EncodeToBytes(uint(i))
		tr.MustUpdate(key, val)
	}
	if tr.Hash() != h.TxHash {
		return "", "", fmt.Errorf("reconstructed tx-trie root %s != block txRoot %s", tr.Hash().Hex(), h.TxHash.Hex())
	}
	var proof proofList
	tkey, _ := rlp.EncodeToBytes(uint(txIndex))
	if err := tr.Prove(tkey, &proof); err != nil {
		return "", "", fmt.Errorf("prove: %w", err)
	}
	var buf []byte
	for _, n := range proof {
		buf = append(buf, n...)
	}
	return hex.EncodeToString(txs[txIndex]), hex.EncodeToString(buf), nil
}

// ReceiptTrieProof builds the receipt-trie proof for txIndex in block `num`:
// returns the consensus-encoded receipt hex + concatenated-RLP proof-node hex.
// Reconstructs the canonical receipt trie (the same EncodeIndex bytes DeriveSha
// uses) and asserts root == the block's real receiptsRoot before returning.
// For a plain ETH transfer the receipt has empty logs + all-zero bloom — the
// CC-1 case (non-stripping receipt encoding must still match the root).
func (a *Anvil) ReceiptTrieProof(ctx context.Context, num, txIndex uint64) (receiptHex, proofHex string, err error) {
	h, hashes, _, err := blockRaw(ctx, a.cli, a.rpc, num, false)
	if err != nil {
		return "", "", err
	}
	if txIndex >= uint64(len(hashes)) {
		return "", "", fmt.Errorf("txIndex %d out of range (%d txs)", txIndex, len(hashes))
	}
	receipts := make(ethtypes.Receipts, len(hashes))
	for i, txh := range hashes {
		r, rErr := a.cli.TransactionReceipt(ctx, txh)
		if rErr != nil {
			return "", "", fmt.Errorf("receipt %d: %w", i, rErr)
		}
		receipts[i] = r
	}
	encode := func(i int) []byte { return encodeReceipt(receipts[i]) }
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	for i := range receipts {
		key, _ := rlp.EncodeToBytes(uint(i))
		tr.MustUpdate(key, encode(i))
	}
	if tr.Hash() != h.ReceiptHash {
		return "", "", fmt.Errorf("reconstructed receipt-trie root %s != block receiptsRoot %s", tr.Hash().Hex(), h.ReceiptHash.Hex())
	}
	var proof proofList
	tkey, _ := rlp.EncodeToBytes(uint(txIndex))
	if err := tr.Prove(tkey, &proof); err != nil {
		return "", "", fmt.Errorf("prove: %w", err)
	}
	var buf []byte
	for _, n := range proof {
		buf = append(buf, n...)
	}
	return hex.EncodeToString(encode(int(txIndex))), hex.EncodeToString(buf), nil
}

// FindTxIndex returns the index of txHash within its block.
func (a *Anvil) FindTxIndex(ctx context.Context, num uint64, txHash ethcommon.Hash) (uint64, error) {
	_, hashes, _, err := blockRaw(ctx, a.cli, a.rpc, num, false)
	if err != nil {
		return 0, err
	}
	for i, h := range hashes {
		if h == txHash {
			return uint64(i), nil
		}
	}
	return 0, fmt.Errorf("tx %s not in block %d", txHash.Hex(), num)
}

// DecodeUnsigned1559 parses the bridge's unsigned EIP-1559 withdrawal
// (0x02 || rlp of the nine unsigned fields, PendingSpend.unsigned_tx_hex).
func DecodeUnsigned1559(unsignedHex string) (*ethtypes.DynamicFeeTx, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(unsignedHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("unsigned tx hex: %w", err)
	}
	if len(b) < 2 || b[0] != ethtypes.DynamicFeeTxType {
		return nil, fmt.Errorf("unsigned tx is not EIP-1559")
	}
	var f struct {
		ChainID    *big.Int
		Nonce      uint64
		GasTipCap  *big.Int
		GasFeeCap  *big.Int
		Gas        uint64
		To         *ethcommon.Address `rlp:"nil"`
		Value      *big.Int
		Data       []byte
		AccessList ethtypes.AccessList
	}
	if err := rlp.DecodeBytes(b[1:], &f); err != nil {
		return nil, fmt.Errorf("unsigned tx rlp: %w", err)
	}
	return &ethtypes.DynamicFeeTx{ChainID: f.ChainID, Nonce: f.Nonce, GasTipCap: f.GasTipCap, GasFeeCap: f.GasFeeCap,
		Gas: f.Gas, To: f.To, Value: f.Value, Data: f.Data, AccessList: f.AccessList}, nil
}

// SignUnsigned1559 signs the bridge's queued withdrawal with key, as the TSS
// vault key does, so the broadcast tx is byte-for-byte the one the contract
// queued.
func SignUnsigned1559(unsignedHex string, key *ecdsa.PrivateKey) (*ethtypes.Transaction, error) {
	inner, err := DecodeUnsigned1559(unsignedHex)
	if err != nil {
		return nil, err
	}
	return ethtypes.SignTx(ethtypes.NewTx(inner), ethtypes.LatestSignerForChainID(inner.ChainID), key)
}

// FindTxFrom scans blocks fromBlock..latest for the tx that from sent at
// nonce, returning it and its block number (found=false if not mined yet).
func (a *Anvil) FindTxFrom(ctx context.Context, from ethcommon.Address, nonce, fromBlock uint64) (*ethtypes.Transaction, uint64, bool, error) {
	latest, err := a.cli.BlockNumber(ctx)
	if err != nil {
		return nil, 0, false, err
	}
	for n := fromBlock; n <= latest; n++ {
		tx, ok, err := sentAt(ctx, a.cli, a.rpc, n, from, nonce)
		if err != nil {
			return nil, 0, false, err
		}
		if ok {
			return tx, n, true, nil
		}
	}
	return nil, 0, false, nil
}

// tssKeyAddress is the L1 address of a TSS public key as getTssKey reports it
// (hex, compressed or uncompressed secp256k1).
func tssKeyAddress(pk string) (ethcommon.Address, bool) {
	raw, err := hex.DecodeString(strings.TrimPrefix(pk, "0x"))
	if err != nil {
		return ethcommon.Address{}, false
	}
	switch len(raw) {
	case 33:
		pub, err := ethCrypto.DecompressPubkey(raw)
		if err != nil {
			return ethcommon.Address{}, false
		}
		return ethCrypto.PubkeyToAddress(*pub), true
	case 65:
		pub, err := ethCrypto.UnmarshalPubkey(raw)
		if err != nil {
			return ethcommon.Address{}, false
		}
		return ethCrypto.PubkeyToAddress(*pub), true
	}
	return ethcommon.Address{}, false
}

// b64Proof re-encodes a hex proof (concatenated MPT nodes) as the base64 the
// bridge's confirmSpend and Type A drop proofs take.
func b64Proof(proofHex string) (string, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(proofHex, "0x"))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
