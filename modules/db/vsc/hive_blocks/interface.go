package hive_blocks

import (
	"vsc-node/modules/aggregate"
)

type HiveBlocks interface {
	aggregate.Plugin
	StoreBlocks(blocks ...HiveBlock) error
	ClearBlocks() error
	StoreLastProcessedBlock(blockNumber int) error
	GetLastProcessedBlock() (int, error)
	FetchStoredBlocks(startBlock int, endBlock int) ([]HiveBlock, error)
	FetchNextBlocks(startBlock int, limit int) ([]HiveBlock, error)
	GetHighestBlock() (int, error)
}
