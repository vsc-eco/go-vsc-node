package hive_blocks

import (
	"context"
	a "vsc-node/modules/aggregate"
)

type HiveBlocks interface {
	a.Plugin
	StoreBlock(ctx context.Context, block *HiveBlock) error
	ClearBlocks(ctx context.Context) error
	StoreLastProcessedBlock(ctx context.Context, blockNumber int) error
	GetLastProcessedBlock(ctx context.Context) (int, error)
	FetchStoredBlocks(ctx context.Context, startBlock int, endBlock int) ([]HiveBlock, error)
}
