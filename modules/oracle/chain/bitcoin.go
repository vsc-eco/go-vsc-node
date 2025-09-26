package chain

import (
	"errors"
	"fmt"
	"log"
	"os"
	"vsc-node/modules/oracle/p2p"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

type bitcoinRelayer struct {
	rpcConfig rpcclient.ConnConfig
}

var (
	_ chainRelay = &bitcoinRelayer{}

	errInvalidConf = errors.New("invalid config")
)

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	var (
		rpcUsername, rpcUsernameOk = os.LookupEnv("BTCD_RPC_USERNAME")
		rpcPassword, rpcPasswordOk = os.LookupEnv("BTCD_RPC_PASSWORD")
		rpcHost, rpcHostOk         = os.LookupEnv("BTCD_RPC_HOST")
	)

	if !rpcUsernameOk || !rpcPasswordOk || !rpcHostOk {
		return errInvalidConf
	}

	b.rpcConfig = rpcclient.ConnConfig{
		Host:         rpcHost,
		User:         rpcUsername,
		Pass:         rpcPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}

	return nil
}

// GetBlock implements chainRelay.
// Get the block of depth 6
func (b *bitcoinRelayer) GetBlock() (*p2p.BlockRelay, error) {
	btcdClient, err := b.connect(&b.rpcConfig)
	if err != nil {
		return nil, err
	}

	defer btcdClient.Shutdown()

	headBlockHeight, err := btcdClient.GetBlockCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get headblock: %w", err)
	}

	blockHeight := headBlockHeight - 6 // depth 6

	blockHash, err := btcdClient.GetBlockHash(blockHeight)
	if err != nil {
		log.Fatal(err)
	}

	// get block header + total fees
	var (
		blockChan      = make(chan result[*wire.MsgBlock], 1)
		blockStatsChan = make(chan result[*btcjson.GetBlockStatsResult], 1)
	)
	defer close(blockChan)
	defer close(blockStatsChan)

	// get block header + average fee
	go btcdClient.getBlock(blockHash, blockChan)
	go btcdClient.getBlockStats(blockHash, blockStatsChan)

	var (
		blockQueryResult      = <-blockChan
		blockStatsQueryResult = <-blockStatsChan
	)

	if blockQueryResult.err != nil {
		return nil, blockQueryResult.err
	}

	if blockStatsQueryResult.err != nil {
		return nil, blockStatsQueryResult.err
	}

	var (
		blockHeader = blockQueryResult.ok.Header
		avgFee      = blockStatsQueryResult.ok.AverageFee
	)

	// make block relay
	p2pBlockRelay := &p2p.BlockRelay{
		Hash:       blockHash.String(),
		Height:     blockHeight,
		PrevBlock:  blockHeader.PrevBlock.String(),
		MerkleRoot: blockHeader.MerkleRoot.String(),
		Timestamp:  blockHeader.Timestamp.UTC(),
		AverageFee: avgFee,
	}

	return p2pBlockRelay, nil
}

type btcdClient struct {
	*rpcclient.Client
}

func (b *bitcoinRelayer) connect(
	cfg *rpcclient.ConnConfig,
) (*btcdClient, error) {
	cx, err := rpcclient.New(cfg, nil)
	if err != nil {
		return nil, err
	}
	return &btcdClient{cx}, nil
}

type result[T any] struct {
	ok  T
	err error
}

func (b *btcdClient) getBlock(
	blockHash *chainhash.Hash,
	c chan<- result[*wire.MsgBlock],
) {
	r := result[*wire.MsgBlock]{nil, nil}
	r.ok, r.err = b.GetBlock(blockHash)
	c <- r
}

func (b *btcdClient) getBlockStats(
	blockHash *chainhash.Hash,
	c chan<- result[*btcjson.GetBlockStatsResult],
) {
	var (
		blockStatsFilter = []string{"avgfee"}
		r                = result[*btcjson.GetBlockStatsResult]{nil, nil}
	)
	r.ok, r.err = b.GetBlockStats(blockHash, &blockStatsFilter)
	c <- r
}
