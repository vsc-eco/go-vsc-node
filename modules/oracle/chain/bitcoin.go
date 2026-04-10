package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/lib/vsclog"

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
	if sconf.OnTestnet() {
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
	startHeight uint64,
	count uint64,
) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}

	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	// get stopHeight
	latestBlock, err := btcdClient.GetBlockCount()
	if err != nil {
		return nil, err
	}

	stopHeight := startHeight + count
	if stopHeight > uint64(latestBlock)+1 {
		stopHeight = uint64(latestBlock) + 1
	}

	if stopHeight < startHeight {
		// Local bitcoin node is behind the requested start height — not synced yet.
		return nil, fmt.Errorf("local bitcoin tip (%d) is behind requested start height (%d)", stopHeight, startHeight)
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
		btcBlock, err := getBlockByHash(btcdClient, entry.hash, entry.height)
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
		if err := fetchPrunedBlocks(btcdClient, prunedHashes); err != nil {
			return nil, fmt.Errorf("failed to recover pruned blocks: %w", err)
		}
		for _, idx := range prunedIndices {
			entry := entries[idx]
			btcBlock, err := getBlockByHash(btcdClient, entry.hash, entry.height)
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

	latestValidBlockHeight := uint64(blockCount) - b.validityThreshold
	return latestValidBlockHeight, nil
}

// getPeers returns the list of connected peer IDs.
func getPeers(btcdClient *rpcclient.Client) ([]int32, error) {
	peers, err := btcdClient.GetPeerInfo()
	if err != nil {
		return nil, fmt.Errorf("getpeerinfo failed: %w", err)
	}
	ids := make([]int32, len(peers))
	for i, p := range peers {
		ids[i] = p.ID
	}
	return ids, nil
}

// requestBlockFromPeer sends a getblockfrompeer request for a single block hash.
func requestBlockFromPeer(btcdClient *rpcclient.Client, blockHash *chainhash.Hash, peerID int32) error {
	hashJSON, _ := json.Marshal(blockHash.String())
	peerJSON, _ := json.Marshal(peerID)
	_, err := btcdClient.RawRequest("getblockfrompeer", []json.RawMessage{hashJSON, peerJSON})
	return err
}

// fetchPrunedBlocks requests multiple pruned blocks from peers in batch, then
// polls until all arrive. Tries up to 3 peers, falling back if one times out.
func fetchPrunedBlocks(btcdClient *rpcclient.Client, blockHashes []*chainhash.Hash) error {
	if len(blockHashes) == 0 {
		return nil
	}

	peers, err := getPeers(btcdClient)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return errors.New("no connected peers to fetch pruned blocks from")
	}

	maxPeers := len(peers)
	if maxPeers > 3 {
		maxPeers = 3
	}

	for p := 0; p < maxPeers; p++ {
		// Request all blocks from this peer.
		for _, hash := range blockHashes {
			if err := requestBlockFromPeer(btcdClient, hash, peers[p]); err != nil {
				btcLogger.Debug("getblockfrompeer request failed", "hash", hash, "peer", peers[p], "err", err)
			}
		}

		// Poll until all blocks arrive (up to 10s per peer attempt).
		for i := 0; i < 10; i++ {
			time.Sleep(1 * time.Second)
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
	}
	return fmt.Errorf("%d blocks still unavailable after trying %d peers", len(blockHashes), maxPeers)
}

func getBlockByHash(
	btcdClient *rpcclient.Client,
	blockHash *chainhash.Hash,
	knownHeight uint64,
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

	blockStats, err := btcdClient.GetBlockStats(
		blockHash,
		&[]string{"height", "avgfeerate"},
	)
	if err != nil {
		btcBlock.Height = knownHeight
		btcBlock.AverageFeeRate = 0
	} else {
		btcBlock.Height = uint64(blockStats.Height)
		btcBlock.AverageFeeRate = blockStats.AverageFeeRate
	}

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
