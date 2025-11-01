package api

import (
	"context"
	"fmt"
	"time"
)

const (
	ethRpcApi = "https://ethereum-rpc.publicnode.com"
)

var (
	_ ChainBlock = &ethereumBlock{}
)

type Ethereum struct {
	ctx context.Context
}

// ChainData implements ChainRelay.
func (e *Ethereum) ChainData(startBlockHeight uint64, count uint64) ([]ChainBlock, error) {
	panic("unimplemented")
}

// ContractID implements ChainRelay.
func (e *Ethereum) ContractID() string {
	panic("unimplemented")
}

// GetContractState implements ChainRelay.
func (e *Ethereum) GetContractState() (ChainState, error) {
	panic("unimplemented")
}

// GetLatestValidHeight implements ChainRelay.
func (e *Ethereum) GetLatestValidHeight() (ChainState, error) {
	const method = "eth_blockNumber"

	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()

	var blockNumHex string
	if err := postRPC(ctx, ethRpcApi, method, []any{}, &blockNumHex); err != nil {
		return ChainState{}, fmt.Errorf("failed to post rpc method: %w", err)
	}

	cs := ChainState{}
	if _, err := fmt.Sscanf(blockNumHex, "0x%x", &cs.BlockHeight); err != nil {
		return ChainState{}, fmt.Errorf("failed to parse block height hex: %w", err)
	}

	return cs, nil
}

// Init implements ChainRelay.
func (e *Ethereum) Init(ctx context.Context) error {
	e.ctx = ctx
	return nil
}

// Symbol implements ChainRelay.
func (e *Ethereum) Symbol() string {
	return "eth"
}

	panic("unimplemented")
}
