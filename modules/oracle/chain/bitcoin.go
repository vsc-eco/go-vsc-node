package chain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
)

var _ chainRelay = &bitcoinRelayer{}

type bitcoinRelayer struct {
	rpcConfig   rpcclient.ConnConfig
	blockHeight uint64
}

const (
	btcdRpcUsername          = "vsc-node-user"
	btcdRpcPassword          = "vsc-node-pass"
	bctdRpcHost              = "0.0.0.0:8332"
	btcdBlockHeightThreshold = 6
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
	b.rpcConfig = rpcclient.ConnConfig{
		Host:         bctdRpcHost,
		User:         btcdRpcUsername,
		Pass:         btcdRpcPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	b.blockHeight = 0

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
	blockCount, err := btcdClient.GetBlockCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get block count: %w", err)
	}

	// TODO: get latestSubmittedBlockHeight
	// - this is read from the contract
	latestSubmittedBlockHeight := uint64(1)

	latestBlock := uint64(blockCount) - btcdBlockHeightThreshold
	newStateAvailable := latestSubmittedBlockHeight < latestBlock
	if newStateAvailable {
		return nil, nil
	}

	b.blockHeight = latestBlock

	return &chainState{blockHeight: b.blockHeight}, nil
}

// GetBlock implements chainRelay.
func (b *bitcoinRelayer) ChainData() (json.RawMessage, error) {
	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd server: %w", err)
	}
	defer btcdClient.Shutdown()

	blockHash, err := btcdClient.GetBlockHash(int64(b.blockHeight))
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get block hash: blockHeight [%d], err [%w]",
			b.blockHeight, err,
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

func (b *bitcoinRelayer) getBlockByHash(
	btcdClient *rpcclient.Client,
	blockHash *chainhash.Hash,
) (*btcChainData, error) {

	// get block header + average fee
	block, err := btcdClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	blockStats, err := btcdClient.GetBlockStats(blockHash, &[]string{"avgfee"})
	if err != nil {
		return nil, fmt.Errorf("failed to get block stats: %w", err)
	}

	// make block relay
	blockHeader := block.Header
	btcBlock := btcChainData{
		Hash:       blockHash.String(),
		Height:     b.blockHeight,
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
