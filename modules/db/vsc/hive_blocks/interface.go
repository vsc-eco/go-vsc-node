package hive_blocks

import (
	"context"
	"vsc-node/modules/aggregate"
)

type HiveBlocks interface {
	aggregate.Plugin
	StoreBlocks(blocks ...HiveBlock) error
	ClearBlocks() error
	StoreLastProcessedBlock(blockNumber int) error
	GetLastProcessedBlock() (int, error)
	FetchStoredBlocks(startBlock int, endBlock int) ([]HiveBlock, error)
	ListenToBlockUpdates(ctx context.Context, startBlock int, listener func(block HiveBlock) error) (context.CancelFunc, <-chan error)
	GetHighestBlock() (int, error)
	GetBlock(blockNum int) (HiveBlock, error)
}
