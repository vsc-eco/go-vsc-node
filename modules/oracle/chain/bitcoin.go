package chain

import (
	"fmt"
	"vsc-node/modules/oracle/p2p"

	"github.com/btcsuite/btcd/rpcclient"
)

type bitcoinRelayer struct {
	rpcConfig    rpcclient.ConnConfig
	highestBlock uint64
}

var (
	_ chainRelay = &bitcoinRelayer{}
)

const (
	btcdRpcUsername    = "vsc-node-user"
	btcdRpcPassword    = "vsc-node-pass"
	bctdRpcHost        = "0.0.0.0:8332"
	headBlockThreshold = 6
)

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

// Init implements chainRelay.
func (b *bitcoinRelayer) Init() error {
	b.rpcConfig = rpcclient.ConnConfig{
		Host:         bctdRpcHost,
		User:         btcdRpcUsername,
		Pass:         btcdRpcPassword,
		HTTPPostMode: true,
		DisableTLS:   true,
	}
	b.highestBlock = 0

	return nil
}

func (b *bitcoinRelayer) TickCheck() (*chainState, error) {
	latestBlock := uint64(1)
	if latestBlock <= b.highestBlock {
		return nil, nil
	}
	// TODO implement this:
	// - State is fetched from contract
	// - Node determines that there are new blocks to fetch

	return &chainState{blockHeight: b.highestBlock + 1}, nil
}

// GetBlock implements chainRelay.
// Get the block of depth 6
func (b *bitcoinRelayer) GetBlock() (*p2p.BlockRelay, error) {
	btcdClient, err := b.connect(&b.rpcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to btcd client: %w", err)
	}

	defer btcdClient.Shutdown()

	headBlockHeight, err := btcdClient.GetBlockCount()
	if err != nil {
		return nil, fmt.Errorf("failed to get headblock: %w", err)
	}

	blockHeight := headBlockHeight - headBlockThreshold
	blockHash, err := btcdClient.GetBlockHash(blockHeight)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get block hash: blockHeight [%d], err [%w]",
			blockHeight, err,
		)
	}

	// get block header + average fee
	block, err := btcdClient.GetBlock(blockHash)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get block: hash [%s], err [%w]",
			blockHash.String(), err,
		)
	}

	blockStats, err := btcdClient.GetBlockStats(blockHash, &[]string{"avgfee"})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to get block stats: hash [%s] , err [%w]",
			blockHash.String(), err,
		)
	}

	// make block relay
	blockHeader := block.Header
	p2pBlockRelay := &p2p.BlockRelay{
		Hash:       blockHash.String(),
		Height:     blockHeight,
		PrevBlock:  blockHeader.PrevBlock.String(),
		MerkleRoot: blockHeader.MerkleRoot.String(),
		Timestamp:  blockHeader.Timestamp.UTC(),
		AverageFee: blockStats.AverageFee,
	}

	return p2pBlockRelay, nil
}
