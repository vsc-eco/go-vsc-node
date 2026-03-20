package chain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

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
func (b *bitcoinRelayer) Init() error {
	b.validityThreshold = 3
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

	// get all blocks from startHeight to stopHeight
	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for blockHeight := startHeight; blockHeight < stopHeight; blockHeight++ {
		blockHash, err := btcdClient.GetBlockHash(int64(blockHeight))
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block hash: blockHeight [%d], err [%w]",
				blockHeight, err,
			)
		}

		btcBlock, err := getBlockByHash(btcdClient, blockHash, blockHeight)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block: [blockHeight: %d], [err: %w]",
				blockHeight, err,
			)
		}

		blocks = append(blocks, btcBlock)
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

// Clone implements chainRelay.
func (b *bitcoinRelayer) Clone() chainRelay {
	clone := *b
	return &clone
}

func (b *bitcoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
