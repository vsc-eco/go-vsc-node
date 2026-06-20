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
	"encoding/hex"

	ethcommon "github.com/ethereum/go-ethereum/common"
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
func (p *proofList) Delete(key []byte) error      { return nil }

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
	cmd := exec.CommandContext(ctx, bin,
		"--port", port,
		"--chain-id", fmt.Sprintf("%d", chainID),
		"--block-time", "1", // auto-mine every 1s so finalized advances
		"--silent",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("anvil start: %v", err)
	}
	url := "http://127.0.0.1:" + port
	a := &Anvil{cmd: cmd, URL: url, ChainID: new(big.Int).SetUint64(chainID)}
	// Wait for RPC up.
	deadline := time.Now().Add(20 * time.Second)
	for {
		rc, err := ethrpc.DialContext(ctx, url)
		if err == nil {
			a.rpc = rc
			a.cli = ethclient.NewClient(rc)
			if _, e := a.cli.ChainID(ctx); e == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("anvil RPC never came up at %s", url)
		}
		time.Sleep(300 * time.Millisecond)
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
	tip := big.NewInt(1_000_000_000)  // 1 gwei
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
	deadline := time.Now().Add(timeout)
	for {
		r, err := a.cli.TransactionReceipt(ctx, h)
		if err == nil && r != nil {
			return r, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("receipt for %s not found within %s", h.Hex(), timeout)
		}
		time.Sleep(300 * time.Millisecond)
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

// Header reads block `num`'s real header fields for planting.
func (a *Anvil) Header(ctx context.Context, num uint64) (*AnvilHeader, error) {
	blk, err := a.cli.BlockByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", num, err)
	}
	h := blk.Header()
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
	blk, err := a.cli.BlockByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return "", "", fmt.Errorf("block %d: %w", num, err)
	}
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	txs := blk.Transactions()
	if txIndex >= uint64(len(txs)) {
		return "", "", fmt.Errorf("txIndex %d out of range (%d txs)", txIndex, len(txs))
	}
	for i, tx := range txs {
		key, _ := rlp.EncodeToBytes(uint(i))
		val, mErr := tx.MarshalBinary()
		if mErr != nil {
			return "", "", fmt.Errorf("marshal tx %d: %w", i, mErr)
		}
		tr.MustUpdate(key, val)
	}
	if tr.Hash() != blk.TxHash() {
		return "", "", fmt.Errorf("reconstructed tx-trie root %s != block txRoot %s", tr.Hash().Hex(), blk.TxHash().Hex())
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
	raw, _ := txs[txIndex].MarshalBinary()
	return hex.EncodeToString(raw), hex.EncodeToString(buf), nil
}

// ReceiptTrieProof builds the receipt-trie proof for txIndex in block `num`:
// returns the consensus-encoded receipt hex + concatenated-RLP proof-node hex.
// Reconstructs the canonical receipt trie (the same EncodeIndex bytes DeriveSha
// uses) and asserts root == the block's real receiptsRoot before returning.
// For a plain ETH transfer the receipt has empty logs + all-zero bloom — the
// CC-1 case (non-stripping receipt encoding must still match the root).
func (a *Anvil) ReceiptTrieProof(ctx context.Context, num, txIndex uint64) (receiptHex, proofHex string, err error) {
	blk, err := a.cli.BlockByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return "", "", fmt.Errorf("block %d: %w", num, err)
	}
	txs := blk.Transactions()
	if txIndex >= uint64(len(txs)) {
		return "", "", fmt.Errorf("txIndex %d out of range (%d txs)", txIndex, len(txs))
	}
	receipts := make(ethtypes.Receipts, len(txs))
	for i, tx := range txs {
		r, rErr := a.cli.TransactionReceipt(ctx, tx.Hash())
		if rErr != nil {
			return "", "", fmt.Errorf("receipt %d: %w", i, rErr)
		}
		receipts[i] = r
	}
	encode := func(i int) []byte {
		var buf bytes.Buffer
		receipts.EncodeIndex(i, &buf)
		return buf.Bytes()
	}
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	for i := range receipts {
		key, _ := rlp.EncodeToBytes(uint(i))
		tr.MustUpdate(key, encode(i))
	}
	if tr.Hash() != blk.ReceiptHash() {
		return "", "", fmt.Errorf("reconstructed receipt-trie root %s != block receiptsRoot %s", tr.Hash().Hex(), blk.ReceiptHash().Hex())
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
	blk, err := a.cli.BlockByNumber(ctx, new(big.Int).SetUint64(num))
	if err != nil {
		return 0, err
	}
	for i, tx := range blk.Transactions() {
		if tx.Hash() == txHash {
			return uint64(i), nil
		}
	}
	return 0, fmt.Errorf("tx %s not in block %d", txHash.Hex(), num)
}
