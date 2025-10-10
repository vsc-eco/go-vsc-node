package mapper

import "sync"

type MapperState struct {
	mutex           sync.Mutex
	LastBlockHeight uint32
	ObservedTxs     map[string]bool
	TxSpends        map[string]SigningData
	LimboTxs        map[string]bool // set for fast lookup
}

func NewMapperState() *MapperState {
	return &MapperState{
		mutex:           sync.Mutex{},
		LastBlockHeight: 918397,
	}
}
