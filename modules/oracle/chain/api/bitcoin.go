package api

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

var (
	_ ChainRelay = &Bitcoin{}
	_ ChainBlock = &BtcChainData{}
)

type Bitcoin struct {
	rpcConfig         rpcclient.ConnConfig
	validityThreshold uint64
	ctx               context.Context
}

type BtcChainData struct {
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
func (b *Bitcoin) Init(ctx context.Context) error {
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
func (b *Bitcoin) Symbol() string {
	return "btc"
}

// ContractID implements chainRelay.
func (b *Bitcoin) ContractID() string {
	return "vsc1BonkE2CtHqjnkFdH8hoAEMP25bbWhSr3UA"
}

// TickCheck implements chainRelay.
func (b *Bitcoin) GetLatestValidHeight() (ChainState, error) {
	// connect to btcd
	btcdClient, err := b.connect()
	if err != nil {
		return ChainState{}, fmt.Errorf(
			"failed to connect to btcd server: %w",
			err,
		)
	}
	defer btcdClient.Shutdown()

	// latest chain state check
	latestBlockHeight, err := b.getLatestValidBlockHeight(btcdClient)
	if err != nil {
		return ChainState{}, fmt.Errorf("failed to get block count: %w", err)
	}

	// // TODO: get latestSubmittedBlockHeight
	// // - this is read from the contract
	// latestSubmittedBlockHeight := uint64(1)

	// newStateAvailable := latestSubmittedBlockHeight < latestBlockHeight
	// if !newStateAvailable {
	// 	return nil, nil
	// }

	return ChainState{BlockHeight: latestBlockHeight}, nil
}

// GetBlock implements chainRelay.
func (b *Bitcoin) ChainData(
	startHeight uint64,
	count uint64,
) ([]ChainBlock, error) {
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

	stopHeight := min(startHeight+count, uint64(latestBlock))

	// get all blocks from startHeight to stopHeight
	blocks := make([]ChainBlock, 0, stopHeight-startHeight)
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
func (b *Bitcoin) GetContractState() (ChainState, error) {
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
		return ChainState{}, fmt.Errorf("failed to query graphQL: %w", err)
	}

	var stateMap map[string]json.RawMessage
	if err := json.Unmarshal(query.GetStateByKeys, &stateMap); err != nil {
		return ChainState{}, fmt.Errorf(
			"failed to deserialize stateMap: %w", err,
		)
	}

	observedData, exists := stateMap[queryKey]
	if !exists {
		return ChainState{BlockHeight: 1}, nil
	}

	var blockData struct {
		LastHeight uint32 `json:"last_height"`
	}
	if err := json.Unmarshal(observedData, &blockData); err != nil {
		return ChainState{}, fmt.Errorf(
			"failed to deserialize blockData: %w", err,
		)
	}

	if blockData.LastHeight == 0 {
		blockData.LastHeight++
	}

	return ChainState{BlockHeight: uint64(blockData.LastHeight)}, nil
}

// Height implements chainBlock.
func (b *BtcChainData) BlockHeight() (uint64, error) {
	return b.Height, nil
}

// Serialize implements chainBlock.
func (b *BtcChainData) Serialize() (string, error) {
	buf := &bytes.Buffer{}
	if err := b.blockHeader.Serialize(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// Type implements chainBlock.
func (b *BtcChainData) Type() string {
	return "BTC"
}

func (b *BtcChainData) AverageFee() (uint64, error) {
	return uint64(b.AverageFeeRate), nil
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

func (b *Bitcoin) getLatestValidBlockHeight(
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
) (*BtcChainData, error) {
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
	btcBlock := BtcChainData{
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

func (b *Bitcoin) connect() (*rpcclient.Client, error) {
	return rpcclient.New(&b.rpcConfig, nil)
}
