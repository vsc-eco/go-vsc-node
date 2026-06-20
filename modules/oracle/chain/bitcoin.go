package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"vsc-node/lib/vsclog"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

var btcLogger = vsclog.Module("oracle-btc")

func init() {
	RegisterChain(&bitcoinRelayer{})
}

var (
	_ chainRelay = &bitcoinRelayer{}
	_ chainBlock = &btcChainData{}
)

type bitcoinRelayer struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
	contractId        string
	autoReorg         bool
	fixedFeeRate      int64 // if > 0, override avgfeerate with this value
}

type btcChainData struct {
	Hash           string    `json:"hash"             validate:"hexadecimal"`
	Height         uint64    `json:"height"`
	PrevBlock      string    `json:"prev_block"       validate:"hexadecimal"`
	MerkleRoot     string    `json:"merkle_root"      validate:"hexadecimal"`
	Timestamp      time.Time `json:"time"`
	AverageFeeRate int64     `json:"average_fee_rate"`

	blockHeader *wire.BlockHeader `json:"-"`
}

// Init implements chainRelay.
func (b *bitcoinRelayer) Init(sconf systemconfig.SystemConfig) error {
	if sconf.OnTestnet() || sconf.OnDevnet() {
		b.validityThreshold = 0
		b.autoReorg = true
		b.fixedFeeRate = 1
	} else {
		b.validityThreshold = 2
		b.autoReorg = false
	}
	return nil
}

// Configure sets the RPC connection config from the oracle config.
func (b *bitcoinRelayer) Configure(host, user, pass string) {
	b.rpcConfig = rpcclient.ConnConfig{
		Host:         host,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
}

// ContractId implements chainRelay.
func (b *bitcoinRelayer) ContractId() string {
	return b.contractId
}

// SetContractId implements chainRelay.
func (b *bitcoinRelayer) SetContractId(id string) {
	b.contractId = id
}

// Symbol implements chainRelay.
func (b *bitcoinRelayer) Symbol() string {
	return "BTC"
}

// TickCheck implements chainRelay.
func (b *bitcoinRelayer) GetLatestValidHeight() (chainState, error) {
	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return chainState{}, fmt.Errorf(
			"failed to connect to btcd server: %w",
			err,
		)
	}
	defer btcdClient.Shutdown()

	// latest chain state check
	latestBlockHeight, err := b.getLatestValidBlockHeight(btcdClient)
	if err != nil {
		return chainState{}, fmt.Errorf("failed to get block count: %w", err)
	}

	return chainState{blockHeight: latestBlockHeight}, nil
}

// GetBlock implements chainRelay.
func (b *bitcoinRelayer) ChainData(
	ctx context.Context,
	startHeight uint64,
	count uint64,
	latestValidHeight uint64,
) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}
	if latestValidHeight < startHeight {
		return nil, fmt.Errorf(
			"bitcoin latest valid height (%d) is behind requested start height (%d)",
			latestValidHeight,
			startHeight,
		)
	}

	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	// Cap at latestValidHeight (inclusive) so we never fetch blocks past the
	// caller's validity cutoff, even when catching up a multi-block batch.
	stopHeight := startHeight + count
	if stopHeight > latestValidHeight+1 {
		stopHeight = latestValidHeight + 1
	}

	// Resolve all block hashes first.
	type hashEntry struct {
		hash   *chainhash.Hash
		height uint64
	}
	entries := make([]hashEntry, 0, stopHeight-startHeight)
	for blockHeight := startHeight; blockHeight < stopHeight; blockHeight++ {
		blockHash, err := btcdClient.GetBlockHash(int64(blockHeight))
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block hash: blockHeight [%d], err [%w]",
				blockHeight, err,
			)
		}
		entries = append(entries, hashEntry{hash: blockHash, height: blockHeight})
	}

	// Attempt to fetch all blocks, collecting any that are pruned.
	results := make([]*btcChainData, len(entries))
	var prunedHashes []*chainhash.Hash
	var prunedIndices []int

	for i, entry := range entries {
		btcBlock, err := getBlockByHash(btcdClient, entry.hash, entry.height, b.fixedFeeRate)
		if err != nil {
			if isPrunedBlockError(err) {
				prunedHashes = append(prunedHashes, entry.hash)
				prunedIndices = append(prunedIndices, i)
				continue
			}
			return nil, fmt.Errorf(
				"failed to get block: [blockHeight: %d], [err: %w]",
				entry.height, err,
			)
		}
		results[i] = btcBlock
	}

	// Batch-fetch any pruned blocks from peers, then retry.
	if len(prunedHashes) > 0 {
		btcLogger.Info("recovering pruned blocks from peers", "count", len(prunedHashes))
		if err := fetchPrunedBlocks(ctx, btcdClient, prunedHashes); err != nil {
			return nil, fmt.Errorf("failed to recover pruned blocks: %w", err)
		}
		btcLogger.Info("pruned blocks recovered successfully", "count", len(prunedHashes))
		for _, idx := range prunedIndices {
			entry := entries[idx]
			btcBlock, err := getBlockByHash(btcdClient, entry.hash, entry.height, b.fixedFeeRate)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to get block after recovery: [blockHeight: %d], [err: %w]",
					entry.height, err,
				)
			}
			results[idx] = btcBlock
		}
	}

	blocks := make([]chainBlock, len(results))
	for i, btcBlock := range results {
		if b.fixedFeeRate > 0 {
			btcBlock.AverageFeeRate = b.fixedFeeRate
		}
		blocks[i] = btcBlock
	}

	return blocks, nil
}

// Height implements chainBlock.
func (b *btcChainData) BlockHeight() uint64 {
	return b.Height
}

// Serialize implements chainBlock.
func (b *btcChainData) Serialize() (string, error) {
	buf := &bytes.Buffer{}
	if err := b.blockHeader.Serialize(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// Type implements chainBlock.
func (b *btcChainData) Type() string {
	return "BTC"
}

// isPrunedBlockError checks whether an RPC error indicates a pruned block by
// inspecting the structured btcjson.RPCError rather than matching on error strings.
func isPrunedBlockError(err error) bool {
	var rpcErr *btcjson.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == btcjson.ErrRPCMisc && strings.Contains(rpcErr.Message, "pruned")
	}
	return false
}

// UTILS STUFF

func (b *bitcoinRelayer) getLatestValidBlockHeight(
	btcdClient *rpcclient.Client,
) (uint64, error) {
	blockCount, err := btcdClient.GetBlockCount()
	if err != nil {
		return 0, fmt.Errorf("failed to get block count: %w", err)
	}

	return validBlockHeight(blockCount, b.validityThreshold)
}

// validBlockHeight computes the latest valid block height as
// blockCount - validityThreshold, guarding against uint64 underflow.
//
// When the chain has not yet accumulated validityThreshold blocks (a fresh or
// syncing bitcoind), the naive subtraction uint64(blockCount) - validityThreshold
// wraps to a value near MaxUint64 and would propagate as a bogus latest-valid
// height into ChainData range checks. Returning an error instead causes the
// oracle tick to treat it as "no valid block yet" rather than relaying a
// wrapped height. Extracted as a pure function so the underflow guard is
// directly testable without a live btcd RPC client.
func validBlockHeight(blockCount int64, validityThreshold uint64) (uint64, error) {
	if blockCount < 0 || uint64(blockCount) < validityThreshold {
		return 0, fmt.Errorf(
			"bitcoin chain not yet at minimum height: blockCount=%d, validityThreshold=%d",
			blockCount, validityThreshold,
		)
	}

	return uint64(blockCount) - validityThreshold, nil
}

// getPeers returns the IDs of connected peers that advertise NODE_NETWORK
// (full block service, bit 0). NODE_NETWORK_LIMITED peers only serve the last
// ~288 blocks and will refuse historical fetches, so they are excluded.
func getPeers(btcdClient *rpcclient.Client) ([]int32, error) {
	peers, err := btcdClient.GetPeerInfo()
	if err != nil {
		return nil, fmt.Errorf("getpeerinfo failed: %w", err)
	}
	const nodeNetwork uint64 = 1
	ids := make([]int32, 0, len(peers))
	for _, p := range peers {
		services, err := strconv.ParseUint(p.Services, 16, 64)
		if err != nil {
			continue
		}
		if services&nodeNetwork != 0 {
			ids = append(ids, p.ID)
		}
	}
	return ids, nil
}

// requestBlockFromPeer sends a getblockfrompeer request for a single block hash.
func requestBlockFromPeer(btcdClient *rpcclient.Client, blockHash *chainhash.Hash, peerID int32) error {
	// json.Marshal cannot fail on a string or int — these are infallible.
	hashJSON, _ := json.Marshal(blockHash.String())
	peerJSON, _ := json.Marshal(peerID)
	_, err := btcdClient.RawRequest("getblockfrompeer", []json.RawMessage{hashJSON, peerJSON})
	return err
}

// prunedRecoveryBudget bounds the TOTAL wall-clock time fetchPrunedBlocks may
// spend polling for pruned blocks across ALL peers. MED #111 (m38 M38-MED-3):
// the previous nested loop (3 peers x 10 iterations x 1s) could block the oracle
// witness goroutine for up to 90s in the hot path, piling up behind the
// ChainOracle tick. This single shared budget is the genuine upper bound on how
// long recovery can stall — the total never exceeds it regardless of how many
// peers we cycle through. The block data returned to the caller is unchanged;
// only the worst-case stall shrinks (90s -> 20s).
const prunedRecoveryBudget = 20 * time.Second

// prunedRecoveryPollInterval is how often we re-poll btcd for the requested
// blocks after issuing getblockfrompeer.
const prunedRecoveryPollInterval = 1 * time.Second

// prunedRecoveryPerPeerPolls is the number of poll intervals to give a single
// peer before moving on to the next one. This preserves recovery success for
// slow / large-block deliveries (each peer gets a fair patience window, not a
// single 1s poll) while the shared prunedRecoveryBudget still caps the total.
// 7 polls x 1s = up to 7s per peer; 3 peers would want 21s but the 20s budget
// is the hard ceiling, so a genuinely slow peer set gives up at the budget.
const prunedRecoveryPerPeerPolls = 7

// fetchPrunedBlocks requests multiple pruned blocks from peers in batch, then
// polls until all arrive. Tries up to 3 archive (NODE_NETWORK) peers, giving each
// a bounded patience window before falling back to the next. The provided context
// controls cancellation on shutdown; a hard prunedRecoveryBudget bounds the total
// time spent so a slow/uncooperative peer set can't stall the oracle hot path
// (MED #111). The set of blocks returned to the caller is unchanged — only the
// upper bound on how long recovery may block is reduced.
func fetchPrunedBlocks(ctx context.Context, btcdClient *rpcclient.Client, blockHashes []*chainhash.Hash) error {
	if len(blockHashes) == 0 {
		return nil
	}

	peers, err := getPeers(btcdClient)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		btcLogger.Warn("no archive (NODE_NETWORK) peers connected — cannot fetch pruned blocks. " +
			"Add an archive peer via addnode=<ip> in bitcoind config")
		return errors.New("no archive (NODE_NETWORK) peers connected to fetch pruned blocks from")
	}

	maxPeers := len(peers)
	if maxPeers > 3 {
		maxPeers = 3
	}

	// Bound the entire recovery (across all peers) to a single time budget,
	// derived from ctx so shutdown still cancels promptly.
	budgetCtx, cancel := context.WithTimeout(ctx, prunedRecoveryBudget)
	defer cancel()

	for p := 0; p < maxPeers; p++ {
		// Request all remaining blocks from this peer.
		for _, hash := range blockHashes {
			if err := requestBlockFromPeer(btcdClient, hash, peers[p]); err != nil {
				btcLogger.Debug("getblockfrompeer request failed", "hash", hash, "peer", peers[p], "err", err)
			}
		}

		// Poll this peer up to prunedRecoveryPerPeerPolls times, but never past
		// the shared budget. The budget (not the per-peer count) is the true
		// upper bound: the total stall across all peers can never exceed
		// prunedRecoveryBudget, while each peer still gets a fair patience
		// window so a slow-but-honest peer is not abandoned after a single poll.
		for i := 0; i < prunedRecoveryPerPeerPolls; i++ {
			select {
			case <-budgetCtx.Done():
				// Budget or parent context expired. Surface a real shutdown
				// (parent ctx) distinctly; otherwise the shared budget is
				// exhausted and we give up entirely.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				btcLogger.Warn("giving up on pruned block recovery (budget exhausted)",
					"remaining", len(blockHashes), "peersAttempted", p+1, "budget", prunedRecoveryBudget)
				return fmt.Errorf("%d blocks still unavailable after %s recovery budget", len(blockHashes), prunedRecoveryBudget)
			case <-time.After(prunedRecoveryPollInterval):
			}
			remaining := make([]*chainhash.Hash, 0)
			for _, hash := range blockHashes {
				_, err := btcdClient.GetBlock(hash)
				if err != nil {
					if isPrunedBlockError(err) {
						remaining = append(remaining, hash)
					} else {
						return fmt.Errorf("failed to get block %s: %w", hash, err)
					}
				}
			}
			if len(remaining) == 0 {
				return nil
			}
			blockHashes = remaining
		}
		// This peer did not deliver all remaining blocks within its patience
		// window; fall back to the next peer (re-issuing the request above).
	}
	btcLogger.Warn("giving up on pruned block recovery", "remaining", len(blockHashes), "peersAttempted", maxPeers)
	return fmt.Errorf("%d blocks still unavailable after trying %d peers", len(blockHashes), maxPeers)
}

func getBlockByHash(
	btcdClient *rpcclient.Client,
	blockHash *chainhash.Hash,
	knownHeight uint64,
	fixedFeeRate int64,
) (*btcChainData, error) {
	block, err := btcdClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	blockHeader := &block.Header
	btcBlock := btcChainData{
		Hash:        blockHash.String(),
		PrevBlock:   blockHeader.PrevBlock.String(),
		MerkleRoot:  blockHeader.MerkleRoot.String(),
		Timestamp:   blockHeader.Timestamp.UTC(),
		blockHeader: blockHeader,
	}

	// RT-9: when an operator has set fixedFeeRate (testnet always does,
	// see bitcoinRelayer.Init), AverageFeeRate is going to be overwritten
	// in the result loop regardless. Skip GetBlockStats entirely on that
	// path so a btcd RPC flake can't fail the whole batch on testnet.
	// On mainnet (fixedFeeRate == 0) we propagate the error — silently
	// zeroing the fee there would surface as a "0 sat/vB" unmap tx that
	// sticks on the BTC network.
	if fixedFeeRate > 0 {
		btcBlock.Height = knownHeight
		btcBlock.AverageFeeRate = fixedFeeRate
		return &btcBlock, nil
	}

	blockStats, err := btcdClient.GetBlockStats(
		blockHash,
		&[]string{"height", "avgfeerate"},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get block stats for %s (height %d): %w", blockHash, knownHeight, err)
	}
	btcBlock.Height = uint64(blockStats.Height)
	btcBlock.AverageFeeRate = blockStats.AverageFeeRate

	return &btcBlock, nil
}

// GetCanonicalBlockHeader implements chainRelay.
func (b *bitcoinRelayer) GetCanonicalBlockHeader(height uint64) (string, error) {
	btcdClient, err := b.connect()
	if err != nil {
		return "", fmt.Errorf("failed to connect to btcd: %w", err)
	}
	defer btcdClient.Shutdown()

	blockHash, err := btcdClient.GetBlockHash(int64(height))
	if err != nil {
		return "", fmt.Errorf("failed to get block hash for height %d: %w", height, err)
	}

	block, err := btcdClient.GetBlock(blockHash)
	if err != nil {
		return "", fmt.Errorf("failed to get block at height %d: %w", height, err)
	}

	buf := &bytes.Buffer{}
	if err := block.Header.Serialize(buf); err != nil {
		return "", fmt.Errorf("failed to serialize block header: %w", err)
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// AutoReorgDetection implements chainRelay.
func (b *bitcoinRelayer) AutoReorgDetection() bool {
	return b.autoReorg
}

// Clone implements chainRelay.
func (b *bitcoinRelayer) Clone() chainRelay {
	clone := *b
	return &clone
}

func (b *bitcoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
