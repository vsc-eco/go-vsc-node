package chain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

var (
	_ chainRelay = &bitcoinRelayer{}
	_ chainBlock = &btcChainData{}
)

type bitcoinRelayer struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
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

const (
	btcdRpcUsername = "vsc-node-user"
	btcdRpcPassword = "vsc-node-pass"
)

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	var btcdRpcHost string
	if os.Getenv("DEBUG") == "1" {
		btcdRpcHost = "173.211.12.65:8332"
	} else {
		btcdRpcHost = "btcd:8332"
	}

	b.rpcConfig = rpcclient.ConnConfig{
		Host:         btcdRpcHost,
		User:         btcdRpcUsername,
		Pass:         btcdRpcPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	b.validityThreshold = 3

	return nil
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

	// // TODO: get latestSubmittedBlockHeight
	// // - this is read from the contract
	// latestSubmittedBlockHeight := uint64(1)

	// newStateAvailable := latestSubmittedBlockHeight < latestBlockHeight
	// if !newStateAvailable {
	// 	return nil, nil
	// }

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
	if stopHeight > uint64(latestBlock) {
		stopHeight = uint64(latestBlock)
	}

	// get all blocks from startHeight to stopHeight
	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for blockHeight := startHeight; blockHeight <= stopHeight; blockHeight++ {
		blockHash, err := btcdClient.GetBlockHash(int64(blockHeight))
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block hash: blockHeight [%d], err [%w]",
				blockHeight, err,
			)
		}

		btcBlock, err := getBlockByHash(btcdClient, blockHash)
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

// GetContractState implements chainRelay.
func (b *bitcoinRelayer) GetContractState() (chainState, error) {
	panic("unimplemented")
}

// VerifyChainData implements chainRelay.
// func (b *bitcoinRelayer) VerifyChainData(data json.RawMessage) error {
// 	var peerBtcChainData btcChainData
// 	if err := json.Unmarshal(data, &peerBtcChainData); err != nil {
// 		return fmt.Errorf("failed to deserialize bitcoin chain data: %w", err)
// 	}

// 	var blockHash chainhash.Hash
// 	if err := chainhash.Decode(&blockHash, peerBtcChainData.Hash); err != nil {
// 		return fmt.Errorf("failed to decode chain data hash: %w", err)
// 	}

// 	// connect to btcd
// 	btcdClient, err := b.connect()
// 	if err != nil {
// 		return fmt.Errorf("failed to connect to btcd server: %w", err)
// 	}
// 	defer btcdClient.Shutdown()

// 	// get valid block height, verify the peerBtcChainData has a depth of 3
// 	validBlockHeight, err := b.getLatestValidBlockHeight(btcdClient)
// 	if err != nil {
// 		return fmt.Errorf("failed to get block height: %w", err)
// 	}

// 	if peerBtcChainData.Height > validBlockHeight {
// 		return fmt.Errorf("failed to meet block threshold")
// 	}

// 	// fetch block by hash then verify peerBtcChainData
// 	localBtcChainData, err := b.getBlockByHash(btcdClient, &blockHash)
// 	if err != nil {
// 		return fmt.Errorf(
// 			"failed to get chain data: block hash [%s], err [%w]",
// 			blockHash.String(), err,
// 		)
// 	}

// 	validChainData := reflect.DeepEqual(&peerBtcChainData, localBtcChainData)
// 	if !validChainData {
// 		return errInvalidChainData
// 	}

// 	return nil
// }

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
) (*btcChainData, error) {
	// get block header + average fee
	block, err := btcdClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	blockStats, err := btcdClient.GetBlockStats(
		blockHash,
		&[]string{"height", "avgfeerate"},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get block stats: %w", err)
	}

	blockHeader := &block.Header
	btcBlock := btcChainData{
		Hash:           blockHash.String(),
		Height:         uint64(blockStats.Height),
		PrevBlock:      blockHeader.PrevBlock.String(),
		MerkleRoot:     blockHeader.MerkleRoot.String(),
		Timestamp:      blockHeader.Timestamp.UTC(),
		AverageFeeRate: blockStats.AverageFeeRate,
		blockHeader:    blockHeader,
	}

	return &btcBlock, nil
}

func (b *bitcoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
