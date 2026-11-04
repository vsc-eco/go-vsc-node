package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// blockProofs is what proving a block's txs and receipts takes: its tx hashes
// and, in block order, the raw txs and the consensus-encoded receipts.
//
// TICKSTALL-1: this was fetched again for every deposit of a block and every
// confirmSpend, one serial RPC call per tx (2N+1 calls for an ETH deposit in
// an N-tx block: 601 calls, 12 s at 20 ms a call, for 300 txs), and the calls
// ignored the tick's deadline. It is fetched once per block, proofFetchWorkers
// calls at a time, within the caller's context.
//
// BP-1 (testnet 11-04): Sepolia blocks carry 1,100-1,250 txs, so the per-tx
// fetch was ~2,250 calls per deposit block, and the first call a throttled
// provider refused after its retries threw the whole block away: the next tick
// started again at tx 0, and the deposits never mapped (Alchemy + thirdweb,
// no fault injection, "fetch raw tx 140/1123: rpc HTTP 429"). Now the receipts
// come from one eth_getBlockReceipts call, kept only when they rebuild the
// root the block header commits to (the per-tx fetch is the fallback), and
// everything the per-tx fetch gets is kept for the next tick (blockProofCache),
// so a throttled provider slows a block down instead of starting it over. The
// raw txs stay per tx: re-encoding the full block's tx objects needs a decoder
// for every tx type on the chain (this go-ethereum has none for type 4).
type blockProofs struct {
	hashes   []string
	raw      [][]byte // nil unless fetched with raw
	receipts [][]byte

	txRoot      []byte // the header's transactionsRoot
	receiptRoot []byte // the header's receiptsRoot
}

// proofFetchWorkers bounds one block's concurrent RPC calls, like the scan's
// worker pool (MED-47).
const proofFetchWorkers = 8

// blockProofCacheSize is how many blocks' partial proof material is kept
// between ticks. A tick works on a handful of deposit blocks; keeping more
// would only hold memory for blocks already mapped.
const blockProofCacheSize = 16

// blockProofCache keeps a block's fetched material between ticks (BP-1).
var blockProofCache = struct {
	sync.Mutex
	order []uint64
	m     map[uint64]*blockProofs
}{m: map[uint64]*blockProofs{}}

// cachedBlockProofs is the kept material for height when it belongs to the
// same tx list (a reorged block starts over), else a fresh entry.
func cachedBlockProofs(height uint64, hashes []string, txRoot, receiptRoot []byte) *blockProofs {
	blockProofCache.Lock()
	defer blockProofCache.Unlock()
	if bp, ok := blockProofCache.m[height]; ok && sameStrings(bp.hashes, hashes) &&
		bytes.Equal(bp.txRoot, txRoot) && bytes.Equal(bp.receiptRoot, receiptRoot) {
		return bp
	}
	bp := &blockProofs{hashes: hashes, txRoot: txRoot, receiptRoot: receiptRoot, receipts: make([][]byte, len(hashes))}
	if _, ok := blockProofCache.m[height]; !ok {
		blockProofCache.order = append(blockProofCache.order, height)
	}
	blockProofCache.m[height] = bp
	for len(blockProofCache.order) > blockProofCacheSize {
		old := blockProofCache.order[0]
		blockProofCache.order = blockProofCache.order[1:]
		delete(blockProofCache.m, old)
	}
	return bp
}

// dropBlockProofs forgets height's kept material (it rebuilt a wrong root).
func dropBlockProofs(height uint64) {
	blockProofCache.Lock()
	defer blockProofCache.Unlock()
	delete(blockProofCache.m, height)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fetchBlockProofs fetches block height's tx hashes and receipts, and its raw
// txs when raw is set (an ETH deposit or a confirmSpend proves the tx too).
// What it returns rebuilds the header's receiptsRoot (and transactionsRoot
// when raw is set); anything else is an error, never a proof.
func fetchBlockProofs(ctx context.Context, rpc *ethRPC, height uint64, raw bool) (*blockProofs, error) {
	blockData, err := rpc.callCtx(ctx, "eth_getBlockByNumber", fmt.Sprintf(`"0x%x", false`, height))
	if err != nil {
		return nil, fmt.Errorf("fetch block %d: %w", height, err)
	}
	var block struct {
		Transactions     []string `json:"transactions"`
		TransactionsRoot string   `json:"transactionsRoot"`
		ReceiptsRoot     string   `json:"receiptsRoot"`
	}
	if err := json.Unmarshal(blockData, &block); err != nil {
		return nil, fmt.Errorf("parse block %d: %w", height, err)
	}
	bp := cachedBlockProofs(height, block.Transactions, hexToBytes(block.TransactionsRoot), hexToBytes(block.ReceiptsRoot))
	if err := bp.fill(ctx, rpc, height, raw); err != nil {
		return nil, err
	}
	return bp, nil
}

// fill completes bp's receipts (and raw txs when raw is set): the one-call
// receipts path first, then per-tx calls for what is still missing, then the
// root checks.
func (bp *blockProofs) fill(ctx context.Context, rpc *ethRPC, height uint64, raw bool) error {
	n := len(bp.hashes)
	if raw && bp.raw == nil {
		bp.raw = make([][]byte, n)
	}
	if missing(bp.receipts) > 0 {
		bp.receiptsFromBlockCall(ctx, rpc, height)
	}
	if err := bp.fetchMissing(ctx, rpc, raw); err != nil {
		return err
	}
	if root := listRoot(bp.receipts); !bytes.Equal(root, bp.receiptRoot) {
		dropBlockProofs(height)
		return fmt.Errorf("block %d: rebuilt receipts root %x != header receiptsRoot %x", height, root, bp.receiptRoot)
	}
	if raw {
		if root := listRoot(bp.raw); !bytes.Equal(root, bp.txRoot) {
			dropBlockProofs(height)
			return fmt.Errorf("block %d: rebuilt transactions root %x != header transactionsRoot %x", height, root, bp.txRoot)
		}
	}
	return nil
}

// receiptsFromBlockCall takes every receipt from one eth_getBlockReceipts
// call, kept only if the list rebuilds the header's receiptsRoot. An endpoint
// that does not serve the method (or answers short, out of order or wrong)
// just leaves the receipts to the per-tx path.
func (bp *blockProofs) receiptsFromBlockCall(ctx context.Context, rpc *ethRPC, height uint64) {
	data, err := rpc.callCtx(ctx, "eth_getBlockReceipts", fmt.Sprintf(`"0x%x"`, height))
	if err != nil {
		slog.Debug("BP-1: eth_getBlockReceipts unavailable, per-tx receipts", "block", height, "err", err)
		return
	}
	var rs []receiptForProof
	if err := json.Unmarshal(data, &rs); err != nil || len(rs) != len(bp.hashes) {
		slog.Warn("BP-1: eth_getBlockReceipts answer unusable, per-tx receipts", "block", height, "got", len(rs), "want", len(bp.hashes))
		return
	}
	enc := make([][]byte, len(rs))
	for i := range rs {
		if !strings.EqualFold(rs[i].TransactionHash, bp.hashes[i]) {
			slog.Warn("BP-1: eth_getBlockReceipts out of block order, per-tx receipts", "block", height, "index", i)
			return
		}
		enc[i] = encodeReceiptRLP(&rs[i])
	}
	if root := listRoot(enc); !bytes.Equal(root, bp.receiptRoot) {
		slog.Warn("BP-1: eth_getBlockReceipts does not rebuild receiptsRoot, per-tx receipts", "block", height)
		return
	}
	copy(bp.receipts, enc)
}

// fetchMissing fetches, per tx, the receipts (and raw txs when raw is set)
// still missing. What it fetched stays in bp when a call fails, so the next
// tick only asks for the rest.
func (bp *blockProofs) fetchMissing(ctx context.Context, rpc *ethRPC, raw bool) error {
	var todo []int
	for i := range bp.hashes {
		if bp.receipts[i] == nil || (raw && bp.raw[i] == nil) {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	n := len(bp.hashes)
	var mu sync.Mutex
	return forEachParallel(ctx, len(todo), proofFetchWorkers, func(k int) error {
		i := todo[k]
		hash := fmt.Sprintf(`"%s"`, bp.hashes[i])
		mu.Lock()
		needReceipt := bp.receipts[i] == nil
		mu.Unlock()
		if needReceipt {
			rData, err := callRetry(ctx, rpc, "eth_getTransactionReceipt", hash)
			if err != nil {
				return fmt.Errorf("fetch receipt %d/%d (%d kept): %w", i, n, n-missingLocked(&mu, bp.receipts), err)
			}
			var r receiptForProof
			if err := json.Unmarshal(rData, &r); err != nil {
				return fmt.Errorf("parse receipt %d: %w", i, err)
			}
			enc := encodeReceiptRLP(&r)
			mu.Lock()
			bp.receipts[i] = enc
			mu.Unlock()
		}
		if !raw {
			return nil
		}
		mu.Lock()
		needRaw := bp.raw[i] == nil
		mu.Unlock()
		if !needRaw {
			return nil
		}
		tData, err := callRetry(ctx, rpc, "eth_getRawTransactionByHash", hash)
		if err != nil {
			return fmt.Errorf("fetch raw tx %d/%d (%d kept): %w", i, n, n-missingLocked(&mu, bp.raw), err)
		}
		var rawHex string
		if err := json.Unmarshal(tData, &rawHex); err != nil {
			return fmt.Errorf("parse raw tx %d: %w", i, err)
		}
		b := hexToBytes(rawHex)
		if b == nil {
			return fmt.Errorf("raw tx %d decoded to nil (RPC returned %q)", i, rawHex)
		}
		mu.Lock()
		bp.raw[i] = b
		mu.Unlock()
		return nil
	})
}

func missing(vs [][]byte) int {
	c := 0
	for _, v := range vs {
		if v == nil {
			c++
		}
	}
	return c
}

func missingLocked(mu *sync.Mutex, vs [][]byte) int {
	mu.Lock()
	defer mu.Unlock()
	return missing(vs)
}

// emptyTrieRoot is keccak256(rlp("")), the root of a block with no txs.
var emptyTrieRoot = hexToBytes("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

// listRoot is the root of the index-keyed trie over values (the shape of a
// block's transactions and receipts tries).
func listRoot(values [][]byte) []byte {
	if len(values) == 0 {
		return emptyTrieRoot
	}
	keys := make([][]byte, len(values))
	for k := range keys {
		keys[k] = rlpEncodeUint64(uint64(k))
	}
	root, _, _ := buildMPTProof(keys, values, 0)
	return root
}
