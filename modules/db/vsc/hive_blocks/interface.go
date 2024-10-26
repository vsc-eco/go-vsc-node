package hive_blocks

import a "vsc-node/modules/aggregate"

type HiveBlocks interface {
	a.Plugin
	StoreBlock(block *HiveBlock) error
	ClearBlocks() error
	StoreLastProcessedBlock(blockNumber int) error
	GetLastProcessedBlock() (int, error)
	FetchStoredBlocks(startBlock int, endBlock int) ([]HiveBlock, error)
}
