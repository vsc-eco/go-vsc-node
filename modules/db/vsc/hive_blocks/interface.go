package hive_blocks

import (
	"context"
	"vsc-node/modules/aggregate"
)

type HiveBlocks interface {
	aggregate.Plugin
	StoreBlocks(headBlock uint64, blocks ...HiveBlock) error
	ClearBlocks() error
	StoreLastProcessedBlock(blockNumber uint64) error
	GetLastProcessedBlock() (uint64, error)
	FetchStoredBlocks(startBlock uint64, endBlock uint64) ([]HiveBlock, error)
	ListenToBlockUpdates(ctx context.Context, startBlock uint64, listener func(block HiveBlock, headHeight *uint64) error) (context.CancelFunc, <-chan error)
	GetHighestBlock() (uint64, error)
	GetBlock(blockNum uint64) (HiveBlock, error)
	SetMetadata(doc Document) error
	GetMetadata() (Document, error)
}
