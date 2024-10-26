package hive_blocks

type HiveBlocks interface {
	StoreBlock(block *HiveBlock) error
	ClearBlocks() error
	StoreLastProcessedBlock(blockNumber int) error
	GetLastProcessedBlock() (int, error)
	FetchStoredBlocks(startBlock int, endBlock int) ([]HiveBlock, error)
}
