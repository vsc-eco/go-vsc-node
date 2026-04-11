// Ethereum chain relay implementation.
//
// Unlike BTC/DASH/LTC, Ethereum is not a Bitcoin fork — it uses a completely
// different RPC API (JSON-RPC via go-ethereum/ethclient) and block header
// format (RLP-encoded, variable size).
//
// Key differences from BTC-family chains:
//   - RPC via ethclient.Dial (single URL) instead of btcsuite rpcclient
//   - Block headers are RLP-encoded (variable size) instead of fixed 80 bytes
//   - Uses the "finalized" block tag for safe finality (~13 min / 2 epochs)
//     instead of tip-minus-N confirmation counting
//   - Configure() uses host as the full RPC URL (e.g. "http://geth:8545");
//     user/pass are ignored (use the URL for auth if needed)
//
// To enable: add ETH to ChainContracts in system-config and to the
// Chains map in the oracle config JSON with the RPC endpoint URL as RpcHost.
package chain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"
	systemconfig "vsc-node/modules/common/system-config"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

func init() {
	RegisterChain(&ethereumRelayer{})
}

var (
	_ chainRelay = &ethereumRelayer{}
	_ chainBlock = &ethChainData{}
)

// ethereumRelayer connects to an Ethereum execution client (e.g. Geth, Nethermind)
// via JSON-RPC to fetch block headers.
type ethereumRelayer struct {
	rpcURL     string // full RPC endpoint URL, e.g. "http://geth:8545"
	contractId string
}

// ethChainData holds a single Ethereum block header for relay.
type ethChainData struct {
	Hash       string    `json:"hash"        validate:"hexadecimal"`
	Height     uint64    `json:"height"`
	ParentHash string    `json:"parent_hash" validate:"hexadecimal"`
	StateRoot  string    `json:"state_root"  validate:"hexadecimal"`
	Timestamp  time.Time `json:"time"`

	header *types.Header `json:"-"`
}

// Init implements chainRelay.
func (e *ethereumRelayer) Init(_ systemconfig.SystemConfig) error {
	return nil
}

// Configure implements chainRelay.
// For ETH, host is the full RPC URL (e.g. "http://geth:8545").
// user and pass are not used — include auth in the URL if needed.
func (e *ethereumRelayer) Configure(host, user, pass string) {
	if user != "" || pass != "" {
		slog.Warn("ETH oracle: RpcUser/RpcPass are ignored for Ethereum — "+
			"include credentials in the RPC URL if your node requires auth",
			"host", host)
	}
	e.rpcURL = host
}

// ContractId implements chainRelay.
func (e *ethereumRelayer) ContractId() string {
	return e.contractId
}

// SetContractId implements chainRelay.
func (e *ethereumRelayer) SetContractId(id string) {
	e.contractId = id
}

// Symbol implements chainRelay.
func (e *ethereumRelayer) Symbol() string {
	return "ETH"
}

// GetLatestValidHeight implements chainRelay.
// Uses the "finalized" block tag for safe finality (~13 min behind tip).
func (e *ethereumRelayer) GetLatestValidHeight() (chainState, error) {
	client, err := e.connect()
	if err != nil {
		return chainState{}, fmt.Errorf("failed to connect to ethereum RPC: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use the "finalized" block — guaranteed no reorgs under PoS.
	finalized, err := client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return chainState{}, fmt.Errorf("failed to get finalized block: %w", err)
	}

	return chainState{blockHeight: finalized.Number.Uint64()}, nil
}

// ChainData implements chainRelay.
func (e *ethereumRelayer) ChainData(_ context.Context, startHeight uint64, count uint64) ([]chainBlock, error) {
	if startHeight == 0 {
		return nil, errors.New("start height not provided")
	}

	client, err := e.connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to ethereum RPC: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Cap at the finalized tip to avoid fetching non-final blocks.
	finalized, err := client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return nil, fmt.Errorf("failed to get finalized block: %w", err)
	}

	stopHeight := startHeight + count
	if stopHeight > finalized.Number.Uint64() {
		stopHeight = finalized.Number.Uint64()
	}

	if stopHeight < startHeight {
		return nil, fmt.Errorf("finalized tip (%d) is behind requested start height (%d)", stopHeight, startHeight)
	}

	blocks := make([]chainBlock, 0, stopHeight-startHeight)
	for height := startHeight; height < stopHeight; height++ {
		header, err := client.HeaderByNumber(ctx, new(big.Int).SetUint64(height))
		if err != nil {
			return nil, fmt.Errorf("failed to get header at height %d: %w", height, err)
		}

		blocks = append(blocks, &ethChainData{
			Hash:       header.Hash().Hex(),
			Height:     header.Number.Uint64(),
			ParentHash: header.ParentHash.Hex(),
			StateRoot:  header.Root.Hex(),
			Timestamp:  time.Unix(int64(header.Time), 0).UTC(),
			header:     header,
		})
	}

	return blocks, nil
}

// BlockHeight implements chainBlock.
func (e *ethChainData) BlockHeight() uint64 {
	return e.Height
}

// Serialize implements chainBlock.
// RLP-encodes the full block header as a hex string.
func (e *ethChainData) Serialize() (string, error) {
	encoded, err := rlp.EncodeToBytes(e.header)
	if err != nil {
		return "", fmt.Errorf("failed to RLP-encode header: %w", err)
	}
	return hex.EncodeToString(encoded), nil
}

// Type implements chainBlock.
func (e *ethChainData) Type() string {
	return "ETH"
}

// GetCanonicalBlockHeader implements chainRelay.
func (e *ethereumRelayer) GetCanonicalBlockHeader(height uint64) (string, error) {
	return "", nil
}

// AutoReorgDetection implements chainRelay.
func (e *ethereumRelayer) AutoReorgDetection() bool {
	return false
}

// Clone implements chainRelay.
func (e *ethereumRelayer) Clone() chainRelay {
	clone := *e
	return &clone
}

func (e *ethereumRelayer) connect() (*ethclient.Client, error) {
	if e.rpcURL == "" {
		return nil, errors.New("ethereum RPC URL not configured")
	}
	return ethclient.Dial(e.rpcURL)
}
