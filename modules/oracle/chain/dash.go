// Dash chain relay implementation.
//
// DASH is a Bitcoin fork and uses the same wire protocol and RPC API,
// so we reuse btcsuite/btcd for RPC and block header serialization.
//
// Key differences from BTC:
//   - No getblockstats RPC (height comes from getblock verbose instead)
//   - ~2.5 minute block times (vs ~10 min for BTC)
//   - Different validity threshold (1 block vs 3 for BTC) due to
//     ChainLocks providing near-instant finality
//
// To enable: add DASH to ChainContracts in system-config and to the
// Chains map in the oracle config JSON with RPC connection details.
package chain

import (
	"bytes"
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
	RegisterChain(&dashRelayer{})
}

var (
	_ chainRelay = &dashRelayer{}
	_ chainBlock = &dashChainData{}
)

// dashRelayer connects to a Dash Core RPC node to fetch block headers.
type dashRelayer struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
	contractId        string
}

// dashChainData holds a single Dash block header for relay.
type dashChainData struct {
	Hash       string    `json:"hash"        validate:"hexadecimal"`
	Height     uint64    `json:"height"`
	PrevBlock  string    `json:"prev_block"  validate:"hexadecimal"`
	MerkleRoot string    `json:"merkle_root" validate:"hexadecimal"`
	Timestamp  time.Time `json:"time"`

	blockHeader *wire.BlockHeader `json:"-"`
}

// Init implements chainRelay.
func (d *dashRelayer) Init(_ systemconfig.SystemConfig) error {
	// DASH has ChainLocks for near-instant finality, so a lower
	// validity threshold is sufficient compared to BTC.
	d.validityThreshold = 1
	return nil
}

// Configure implements chainRelay.
func (d *dashRelayer) Configure(host, user, pass string) {
	d.rpcConfig = rpcclient.ConnConfig{
		Host:         host,
		User:         user,
		Pass:         pass,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
}

// ContractId implements chainRelay.
func (d *dashRelayer) ContractId() string {
	return d.contractId
}

// SetContractId implements chainRelay.
func (d *dashRelayer) SetContractId(id string) {
	d.contractId = id
}

// Symbol implements chainRelay.
func (d *dashRelayer) Symbol() string {
	return "DASH"
}

// GetLatestValidHeight implements chainRelay.
func (d *dashRelayer) GetLatestValidHeight() (chainState, error) {
	client, err := d.connect()
	if err != nil {
		return chainState{}, fmt.Errorf("failed to connect to dashd: %w", err)
	}
	defer client.Shutdown()

	blockCount, err := client.GetBlockCount()
	if err != nil {
		return chainState{}, fmt.Errorf("failed to get block count: %w", err)
	}

	latestValid := uint64(blockCount) - d.validityThreshold
	return chainState{blockHeight: latestValid}, nil
}

// ChainData implements chainRelay.
func (d *dashRelayer) ChainData(startHeight uint64, count uint64) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}

	client, err := d.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to dashd: %w", err)
	}
	defer client.Shutdown()

	latestBlock, err := client.GetBlockCount()
	if err != nil {
		return nil, err
	}

	stopHeight := startHeight + count
	if stopHeight > uint64(latestBlock) {
		stopHeight = uint64(latestBlock)
	}

	if stopHeight < startHeight {
		return nil, fmt.Errorf("local dash tip (%d) is behind requested start height (%d)", stopHeight, startHeight)
	}

	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for height := startHeight; height < stopHeight; height++ {
		blockHash, err := client.GetBlockHash(int64(height))
		if err != nil {
			return nil, fmt.Errorf("failed to get block hash at height %d: %w", height, err)
		}

		dashBlock, err := getDashBlockByHash(client, blockHash, height)
		if err != nil {
			return nil, fmt.Errorf("failed to get block at height %d: %w", height, err)
		}

		blocks = append(blocks, dashBlock)
	}

	return blocks, nil
}

// BlockHeight implements chainBlock.
func (d *dashChainData) BlockHeight() uint64 {
	return d.Height
}

// Serialize implements chainBlock.
// Encodes the 80-byte block header as a hex string.
func (d *dashChainData) Serialize() (string, error) {
	buf := &bytes.Buffer{}
	if err := d.blockHeader.Serialize(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf.Bytes()), nil
}

// Type implements chainBlock.
func (d *dashChainData) Type() string {
	return "DASH"
}

// getDashBlockByHash fetches a single block and extracts its header.
// Unlike BTC, DASH doesn't support getblockstats, so height is passed in.
func getDashBlockByHash(
	client *rpcclient.Client,
	blockHash *chainhash.Hash,
	knownHeight uint64,
) (*dashChainData, error) {
	block, err := client.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	header := &block.Header
	return &dashChainData{
		Hash:        blockHash.String(),
		Height:      knownHeight,
		PrevBlock:   header.PrevBlock.String(),
		MerkleRoot:  header.MerkleRoot.String(),
		Timestamp:   header.Timestamp.UTC(),
		blockHeader: header,
	}, nil
}

// Clone implements chainRelay.
func (d *dashRelayer) Clone() chainRelay {
	clone := *d
	return &clone
}

func (d *dashRelayer) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&d.rpcConfig, nil)
}

// GetCanonicalBlockHeader implements chainRelay.
func (d *dashRelayer) GetCanonicalBlockHeader(height uint64) (string, error) {
	return "", nil
}

// AutoReorgDetection implements chainRelay.
func (d *dashRelayer) AutoReorgDetection() bool {
	return false
}
