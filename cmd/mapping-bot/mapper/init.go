package mapper

import "sync"

type MapperState struct {
	Mutex           sync.Mutex
	LastBlockHeight uint32
	ObservedTxs     map[string]bool
	TxSpends        map[string]*SigningData
	LimboTxs        map[string]bool // set for fast lookup
}

func NewMapperState() *MapperState {
	return &MapperState{
		Mutex:           sync.Mutex{},
		LastBlockHeight: 918397,
		ObservedTxs:     make(map[string]bool),
		TxSpends:        make(map[string]*SigningData),
		LimboTxs:        make(map[string]bool),
	}
}
