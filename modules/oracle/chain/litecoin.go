// Litecoin chain relay implementation.
//
// LTC is a Bitcoin fork and uses the same wire protocol and RPC API,
// so we reuse btcsuite/btcd for RPC and block header serialization.
//
// Key differences from BTC:
//   - ~2.5 minute block times (vs ~10 min for BTC)
//   - Scrypt PoW instead of SHA-256d (irrelevant for header relay)
//   - No getblockstats RPC support
//   - Lower validity threshold (2 blocks) due to faster block times
//
// To enable: add LTC to ChainContracts in system-config and to the
// Chains map in the oracle config JSON with RPC connection details.
package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

func init() {
	RegisterChain(&litecoinRelayer{})
}

var (
	_ chainRelay = &litecoinRelayer{}
	_ chainBlock = &ltcChainData{}
)

// litecoinRelayer connects to a Litecoin Core RPC node to fetch block headers.
type litecoinRelayer struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
	contractId        string
}

// ltcChainData holds a single Litecoin block header for relay.
type ltcChainData struct {
	Hash       string    `json:"hash"        validate:"hexadecimal"`
	Height     uint64    `json:"height"`
	PrevBlock  string    `json:"prev_block"  validate:"hexadecimal"`
	MerkleRoot string    `json:"merkle_root" validate:"hexadecimal"`
	Timestamp  time.Time `json:"time"`

	blockHeader *wire.BlockHeader `json:"-"`
}

// Init implements chainRelay.
func (l *litecoinRelayer) Init(_ systemconfig.SystemConfig) error {
	// LTC has ~2.5 min blocks, so 2 confirmations ≈ 5 min.
	l.validityThreshold = 2
	return nil
}

// Configure implements chainRelay.
func (l *litecoinRelayer) Configure(host, user, pass string) {
	l.rpcConfig = rpcclient.ConnConfig{
		Host:         host,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
}

// ContractId implements chainRelay.
func (l *litecoinRelayer) ContractId() string {
	return l.contractId
}

// SetContractId implements chainRelay.
func (l *litecoinRelayer) SetContractId(id string) {
	l.contractId = id
}

// Symbol implements chainRelay.
func (l *litecoinRelayer) Symbol() string {
	return "LTC"
}

// GetLatestValidHeight implements chainRelay.
func (l *litecoinRelayer) GetLatestValidHeight() (chainState, error) {
	client, err := l.connect()
	if err != nil {
		return chainState{}, fmt.Errorf("failed to connect to litecoind: %w", err)
	}
	defer client.Shutdown()

	blockCount, err := client.GetBlockCount()
	if err != nil {
		return chainState{}, fmt.Errorf("failed to get block count: %w", err)
	}

	latestValid := uint64(blockCount) - l.validityThreshold
	return chainState{blockHeight: latestValid}, nil
}

// ChainData implements chainRelay.
func (l *litecoinRelayer) ChainData(_ context.Context, startHeight uint64, count uint64, latestValidHeight uint64) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}
	if latestValidHeight < startHeight {
		return nil, fmt.Errorf("litecoin latest valid height (%d) is behind requested start height (%d)", latestValidHeight, startHeight)
	}

	client, err := l.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to litecoind: %w", err)
	}
	defer client.Shutdown()

	// Cap at latestValidHeight (inclusive) so we never fetch blocks past the
	// caller's validity cutoff.
	stopHeight := startHeight + count
	if stopHeight > latestValidHeight+1 {
		stopHeight = latestValidHeight + 1
	}

	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for height := startHeight; height < stopHeight; height++ {
		blockHash, err := client.GetBlockHash(int64(height))
		if err != nil {
			return nil, fmt.Errorf("failed to get block hash at height %d: %w", height, err)
		}

		ltcBlock, err := getLtcBlockByHash(client, blockHash, height)
		if err != nil {
			return nil, fmt.Errorf("failed to get block at height %d: %w", height, err)
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
// Encodes the 80-byte block header as a hex string.
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

// getLtcBlockByHash fetches a single block and extracts its header.
// Litecoin doesn't support getblockstats, so height is passed in.
func getLtcBlockByHash(
	client *rpcclient.Client,
	blockHash *chainhash.Hash,
	knownHeight uint64,
) (*ltcChainData, error) {
	block, err := client.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	header := &block.Header
	return &ltcChainData{
		Hash:        blockHash.String(),
		Height:      knownHeight,
		PrevBlock:   header.PrevBlock.String(),
		MerkleRoot:  header.MerkleRoot.String(),
		Timestamp:   header.Timestamp.UTC(),
		blockHeader: header,
	}, nil
}

// Clone implements chainRelay.
func (l *litecoinRelayer) Clone() chainRelay {
	clone := *l
	return &clone
}

func (l *litecoinRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&l.rpcConfig, nil)
}

// GetCanonicalBlockHeader implements chainRelay.
func (l *litecoinRelayer) GetCanonicalBlockHeader(height uint64) (string, error) {
	return "", nil
}

// AutoReorgDetection implements chainRelay.
func (l *litecoinRelayer) AutoReorgDetection() bool {
	return false
}
