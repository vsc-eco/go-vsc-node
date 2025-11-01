package api

import (
	"context"
)

type Ethereum struct {
}

// ChainData implements chain.chainRelay.
func (e *Ethereum) ChainData(startBlockHeight uint64, count uint64) ([]ChainBlock, error) {
	panic("unimplemented")
}

// ContractID implements chain.chainRelay.
func (e *Ethereum) ContractID() string {
	panic("unimplemented")
}

// GetContractState implements chain.chainRelay.
func (e *Ethereum) GetContractState() (ChainState, error) {
	panic("unimplemented")
}

// GetLatestValidHeight implements chain.chainRelay.
func (e *Ethereum) GetLatestValidHeight() (ChainState, error) {
	panic("unimplemented")
}

// Init implements chain.chainRelay.
func (e *Ethereum) Init(context.Context) error {
	panic("unimplemented")
}

// Symbol implements chain.chainRelay.
func (e *Ethereum) Symbol() string {
	panic("unimplemented")
}
