package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
)

var _ chainRelay = &bitcoinRelayer{}

type bitcoinRelayer struct {
	rpcConfig rpcclient.ConnConfig
}

const (
	btcdRpcUsername          = "vsc-node-user"
	btcdRpcPassword          = "vsc-node-pass"
	btcdBlockHeightThreshold = 3
)

type btcChainData struct {
	Hash       string    `json:"hash"        validate:"hexadecimal"`
	Height     uint64    `json:"height"`
	PrevBlock  string    `json:"prev_block"  validate:"hexadecimal"`
	MerkleRoot string    `json:"merkle_root" validate:"hexadecimal"`
	Timestamp  time.Time `json:"time"`
	AverageFee int64     `json:"average_fee"`
}

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	var btcdRpcHost string
	if os.Getenv("DEBUG") == "1" {
		btcdRpcHost = "0.0.0.0:8332"
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

	return nil
}

// Symbol implements chainRelay.
func (b *bitcoinRelayer) Symbol() string {
	return "BTC"
}

// TickCheck implements chainRelay.
func (b *bitcoinRelayer) TickCheck() (*chainState, error) {
	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	// latest chain state check
	latestBlockHeight, err := b.getLatestValidBlockHeight(btcdClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get block count: %w", err)
	}

	// TODO: get latestSubmittedBlockHeight
	// - this is read from the contract
	latestSubmittedBlockHeight := uint64(1)

	newStateAvailable := latestSubmittedBlockHeight < latestBlockHeight
	if !newStateAvailable {
		return nil, nil
	}

	return &chainState{blockHeight: latestBlockHeight}, nil
}

// GetBlock implements chainRelay.
func (b *bitcoinRelayer) ChainData(
	chainState *chainState,
) (json.RawMessage, error) {
	if chainState == nil {
		return nil, errors.New("chain state not provided")
	}

	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	blockHeight := int64(chainState.blockHeight)
	blockHash, err := btcdClient.GetBlockHash(blockHeight)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get block hash: blockHeight [%d], err [%w]",
			chainState.blockHeight, err,
		)
	}

	btcBlock, err := b.getBlockByHash(btcdClient, blockHash)
	chainData, err := json.Marshal(&btcBlock)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to serialize bitcoin chain data: %w",
			err,
		)
	}

	return json.RawMessage(chainData), nil
}

// VerifyChainData implements chainRelay.
func (b *bitcoinRelayer) VerifyChainData(data json.RawMessage) error {
	var peerBtcChainData btcChainData
	if err := json.Unmarshal(data, &peerBtcChainData); err != nil {
		return fmt.Errorf("failed to deserialize bitcoin chain data: %w", err)
	}

	var blockHash chainhash.Hash
	if err := chainhash.Decode(&blockHash, peerBtcChainData.Hash); err != nil {
		return fmt.Errorf("failed to decode chain data hash: %w", err)
	}

	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	// get valid block height, verify the peerBtcChainData has a depth of 3
	validBlockHeight, err := b.getLatestValidBlockHeight(btcdClient)
	if err != nil {
		return fmt.Errorf("failed to get block height: %w", err)
	}

	if peerBtcChainData.Height > validBlockHeight {
		return fmt.Errorf("failed to meet block threshold")
	}

	// fetch block by hash then verify peerBtcChainData
	localBtcChainData, err := b.getBlockByHash(btcdClient, &blockHash)
	if err != nil {
		return fmt.Errorf(
			"failed to get chain data: block hash [%s], err [%w]",
			blockHash.String(), err,
		)
	}

	validChainData := reflect.DeepEqual(&peerBtcChainData, localBtcChainData)
	if !validChainData {
		return errInvalidChainData
	}

	return nil
}

// UTILS STUFF

func (b *bitcoinRelayer) getLatestValidBlockHeight(
	btcdClient *rpcclient.Client,
) (uint64, error) {
	blockCount, err := btcdClient.GetBlockCount()
	if err != nil {
		return 0, fmt.Errorf("failed to get block count: %w", err)
	}

	return uint64(blockCount) - btcdBlockHeightThreshold, nil
}

func (b *bitcoinRelayer) getBlockByHash(
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
		&[]string{"avgfee", "height"},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get block stats: %w", err)
	}

	// make block relay
	blockHeader := block.Header
	btcBlock := btcChainData{
		Hash:       blockHash.String(),
		Height:     uint64(blockStats.Height),
		PrevBlock:  blockHeader.PrevBlock.String(),
		MerkleRoot: blockHeader.MerkleRoot.String(),
		Timestamp:  blockHeader.Timestamp.UTC(),
		AverageFee: blockStats.AverageFee,
	}

	return &btcBlock, nil
}

func (b *bitcoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
