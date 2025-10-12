package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"os"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/hasura/go-graphql-client"
)

const (
	graphQLUrl = "https://api.vsc.eco/api/v1/graphql"
)

var (
	_ chainRelay = &bitcoinRelayer{}
	_ chainBlock = &btcChainData{}
)

type bitcoinRelayer struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
	ctx               context.Context
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
func (b *bitcoinRelayer) Init(ctx context.Context) error {
	var btcdRpcHost string
	if os.Getenv("DEBUG") == "1" {
		// btcdRpcHost = "173.211.12.65:8332"
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
	b.validityThreshold = 3
	b.ctx = ctx

	return nil
}

// Symbol implements chainRelay.
func (b *bitcoinRelayer) Symbol() string {
	return "BTC"
}

// ContractID implements chainRelay.
func (b *bitcoinRelayer) ContractID() string {
	return "vsc1BonkE2CtHqjnkFdH8hoAEMP25bbWhSr3UA"
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

// GetContractState implements chainRelay.
func (b *bitcoinRelayer) GetContractState() (chainState, error) {
	const queryKey = "blocklist"

	var query struct {
		GetStateByKeys json.RawMessage `graphql:"getStateByKeys(contractId: $contractId, keys: $keys)"`
	}

	variables := map[string]any{
		"contractId": b.ContractID(),
		"keys":       []string{queryKey},
	}

	var debugClient *http.Client
	if os.Getenv("DEBUG") == "1" {
		debugClient = &http.Client{
			Transport: &loggingTransport{http.DefaultTransport},
		}
	}

	client := graphql.NewClient(graphQLUrl, debugClient)
	opName := graphql.OperationName("GetContractState")
	if err := client.Query(b.ctx, &query, variables, opName); err != nil {
		return chainState{}, fmt.Errorf("failed to query graphQL: %w", err)
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(query.GetStateByKeys, &stateMap); err != nil {
		return chainState{}, fmt.Errorf(
			"failed to deserialize stateMap: %w", err,
		)
	}

	observedData, exists := stateMap[queryKey]
	if !exists {
		return chainState{blockHeight: 1}, nil
	}

	var blockData struct {
		LastHeight uint32 `json:"last_height"`
	}
	if err := json.Unmarshal(observedData, &blockData); err != nil {
		return chainState{}, fmt.Errorf(
			"failed to deserialize blockData: %w", err,
		)
	}

	if blockData.LastHeight == 0 {
		blockData.LastHeight++
	}

	return chainState{blockHeight: uint64(blockData.LastHeight)}, nil
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

type loggingTransport struct {
	transport http.RoundTripper
}

func (t *loggingTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	reqDump, _ := httputil.DumpRequestOut(req, true)
	fmt.Printf("\nRequest:\n%s\n\n", reqDump)

	resp, err := t.transport.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	respDump, _ := httputil.DumpResponse(resp, true)
	fmt.Printf("\nResponse:\n%s\n\n", respDump)

	return resp, err
}

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
