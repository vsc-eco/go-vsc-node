package api

import "context"

const (
	graphQLUrl = "https://api.vsc.eco/api/v1/graphql"
)

type ChainBlock interface {
	Type() string //symbol of block network
	Serialize() (string, error)
	BlockHeight() uint64
	AverageFee() int64
}

type ChainState struct {
	BlockHeight uint64
}

type ChainRelay interface {
	Init(context.Context) error
	// Returns the (lowercase) ticker of the chain (ie, btc for bitcoin).
	Symbol() string
	// Get the deployed contract ID
	ContractID() string
	// Checks for (optional) latest chain state.
	GetLatestValidHeight() (ChainState, error)
	// Get the lastest state on contract
	GetContractState() (ChainState, error)
	// Fetch chaindata
	ChainData(startBlockHeight uint64, count uint64) ([]ChainBlock, error)
}

type ChainSession struct {
	SessionID         string
	Symbol            string
	ContractId        string
	ChainData         []ChainBlock
	NewBlocksToSubmit bool
}
