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

var (
	_ chainRelay = &litecoinRelayer{}
	_ chainBlock = &ltcChainData{}
)

type litecoinRelayer struct {
	conf              ChainConfig
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
}

type ltcChainData struct {
	Hash           string    `json:"hash"             validate:"hexadecimal"`
	Height         uint64    `json:"height"`
	PrevBlock      string    `json:"prev_block"       validate:"hexadecimal"`
	MerkleRoot     string    `json:"merkle_root"      validate:"hexadecimal"`
	Timestamp      time.Time `json:"time"`
	AverageFeeRate int64     `json:"average_fee_rate"`

	blockHeader *wire.BlockHeader `json:"-"`
}

// Init implements chainRelay.
func (l *litecoinRelayer) Init() error {
	ltc := l.conf.Get().Litecoin
	l.rpcConfig = rpcclient.ConnConfig{
		Host:         ltc.Host,
		User:         ltc.User,
		Pass:         ltc.Pass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	l.validityThreshold = 3

	return nil
}

// Symbol implements chainRelay.
func (l *litecoinRelayer) Symbol() string {
	return "LTC"
}

// ContractID implements chainRelay.
func (l *litecoinRelayer) ContractID() string {
	return "" // TODO: set once LTC contract is deployed
}

// GetLatestValidHeight implements chainRelay.
func (l *litecoinRelayer) GetLatestValidHeight() (chainState, error) {
	ltcClient, err := l.connect()
	if err != nil {
		return chainState{}, fmt.Errorf(
			"failed to connect to litecoind server: %w",
			err,
		)
	}
	defer ltcClient.Shutdown()

	latestBlockHeight, err := l.getLatestValidBlockHeight(ltcClient)
	if err != nil {
		return chainState{}, fmt.Errorf("failed to get block count: %w", err)
	}

	return chainState{blockHeight: latestBlockHeight}, nil
}

// ChainData implements chainRelay.
func (l *litecoinRelayer) ChainData(
	startHeight uint64,
	count uint64,
) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}

	ltcClient, err := l.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to litecoind server: %w", err)
	}
	defer ltcClient.Shutdown()

	latestBlock, err := ltcClient.GetBlockCount()
	if err != nil {
		return nil, err
	}

	stopHeight := startHeight + count
	if stopHeight > uint64(latestBlock) {
		stopHeight = uint64(latestBlock)
	}

	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for blockHeight := startHeight; blockHeight <= stopHeight; blockHeight++ {
		blockHash, err := ltcClient.GetBlockHash(int64(blockHeight))
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block hash: blockHeight [%d], err [%w]",
				blockHeight, err,
			)
		}

		ltcBlock, err := getLtcBlockByHash(ltcClient, blockHash)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to get block: [blockHeight: %d], [err: %w]",
				blockHeight, err,
			)
		}

		blocks = append(blocks, ltcBlock)
	}

	return blocks, nil
}

// BlockHeight implements chainBlock.
func (l *ltcChainData) BlockHeight() uint64 {
	return l.Height
}

// Serialize implements chainBlock.
func (l *ltcChainData) Serialize() (string, error) {
	buf := &bytes.Buffer{}
	if err := l.blockHeader.Serialize(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// Type implements chainBlock.
func (l *ltcChainData) Type() string {
	return "LTC"
}

// GetContractState implements chainRelay.
func (l *litecoinRelayer) GetContractState() (chainState, error) {
	//Pull from VSC graphql API
	panic("unimplemented")
}

// UTILS

func (l *litecoinRelayer) getLatestValidBlockHeight(
	ltcClient *rpcclient.Client,
) (uint64, error) {
	blockCount, err := ltcClient.GetBlockCount()
	if err != nil {
		return 0, fmt.Errorf("failed to get block count: %w", err)
	}

	latestValidBlockHeight := uint64(blockCount) - l.validityThreshold
	return latestValidBlockHeight, nil
}

func getLtcBlockByHash(
	ltcClient *rpcclient.Client,
	blockHash *chainhash.Hash,
) (*ltcChainData, error) {
	block, err := ltcClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	blockStats, err := ltcClient.GetBlockStats(
		blockHash,
		&[]string{"height", "avgfeerate"},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get block stats: %w", err)
	}

	blockHeader := &block.Header
	ltcBlock := ltcChainData{
		Hash:           blockHash.String(),
		Height:         uint64(blockStats.Height),
		PrevBlock:      blockHeader.PrevBlock.String(),
		MerkleRoot:     blockHeader.MerkleRoot.String(),
		Timestamp:      blockHeader.Timestamp.UTC(),
		AverageFeeRate: blockStats.AverageFeeRate,
		blockHeader:    blockHeader,
	}

	return &ltcBlock, nil
}

func (l *litecoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&l.rpcConfig, nil)
}
