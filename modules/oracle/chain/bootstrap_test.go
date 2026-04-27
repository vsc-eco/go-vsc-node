package chain

import (
	"context"
	"errors"
	"testing"

	"github.com/chebyrash/promise"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vsc-node/modules/db/vsc/contracts"
	vsclog "vsc-node/lib/vsclog"
	systemconfig "vsc-node/modules/common/system-config"
)

// mockContractState implements contracts.ContractState with a canned response.
type mockContractState struct {
	output contracts.ContractOutput
	err    error
}

func (m *mockContractState) Init() error                           { return nil }
func (m *mockContractState) Start() *promise.Promise[any]          { return promise.New(func(r func(any), _ func(error)) { r(nil) }) }
func (m *mockContractState) Stop() error                           { return nil }
func (m *mockContractState) IngestOutput(contracts.IngestOutputArgs) {}
func (m *mockContractState) GetLastOutput(string, uint64) (contracts.ContractOutput, error) {
	return m.output, m.err
}
func (m *mockContractState) FindOutputs(*string, *string, *string, *uint64, *uint64, int, int) ([]contracts.ContractOutput, error) {
	return nil, nil
}

// mockChainRelay implements chainRelay for bootstrap tests.
type mockChainRelay struct {
	symbol     string
	contractId string
	tipHeight  uint64
	blocks     []chainBlock
}

func (m *mockChainRelay) Init(_ systemconfig.SystemConfig) error { return nil }
func (m *mockChainRelay) Symbol() string                             { return m.symbol }
func (m *mockChainRelay) ContractId() string                         { return m.contractId }
func (m *mockChainRelay) SetContractId(id string)                    { m.contractId = id }
func (m *mockChainRelay) Configure(_, _, _ string)                   {}
func (m *mockChainRelay) GetLatestValidHeight() (chainState, error) {
	return chainState{blockHeight: m.tipHeight}, nil
}
func (m *mockChainRelay) ChainData(_ context.Context, start uint64, count uint64, latestValid uint64) ([]chainBlock, error) {
	stop := start + count
	if stop > latestValid+1 {
		stop = latestValid + 1
	}
	blocks := make([]chainBlock, 0)
	for h := start; h < stop; h++ {
		blocks = append(blocks, &mockChainBlock{height: h, data: "aa", chainType: m.symbol})
	}
	return blocks, nil
}
func (m *mockChainRelay) GetCanonicalBlockHeader(uint64) (string, error) { return "", nil }
func (m *mockChainRelay) Clone() chainRelay                              { c := *m; return &c }
func (m *mockChainRelay) AutoReorgDetection() bool                       { return false }

func TestFetchChainStatus_BootstrapWhenContractHeightZero(t *testing.T) {
	logger := vsclog.Module("test")

	oracle := &ChainOracle{
		ctx:    context.Background(),
		logger: logger,
		contractState: &mockContractState{
			output: contracts.ContractOutput{},
			err:    nil,
		},
	}

	chain := &mockChainRelay{
		symbol:     "ETH",
		contractId: "vsc1TestContract",
		tipHeight:  10745000,
	}

	session, err := oracle.fetchChainStatus(chain)
	require.NoError(t, err)
	assert.True(t, session.newBlocksToSubmit, "should have blocks to submit after bootstrap")
	assert.Equal(t, "ETH", session.symbol)
	assert.Equal(t, "vsc1TestContract", session.contractId)
	assert.Len(t, session.chainData, 1, "should fetch bootstrapLookback=1 block")
	assert.Equal(t, uint64(10745000), session.chainData[0].BlockHeight(), "first block should be tip")
}

func TestFetchChainStatus_BootstrapEmptyChainSkips(t *testing.T) {
	logger := vsclog.Module("test")

	oracle := &ChainOracle{
		ctx:    context.Background(),
		logger: logger,
		contractState: &mockContractState{
			output: contracts.ContractOutput{},
			err:    nil,
		},
	}

	chain := &mockChainRelay{
		symbol:     "ETH",
		contractId: "vsc1TestContract",
		tipHeight:  0,
	}

	session, err := oracle.fetchChainStatus(chain)
	require.NoError(t, err)
	assert.False(t, session.newBlocksToSubmit, "no blocks on chain yet, nothing to submit")
}

func TestFetchChainStatus_BootstrapShortChainStartsAtGenesis(t *testing.T) {
	logger := vsclog.Module("test")

	oracle := &ChainOracle{
		ctx:    context.Background(),
		logger: logger,
		contractState: &mockContractState{
			output: contracts.ContractOutput{},
			err:    nil,
		},
	}

	chain := &mockChainRelay{
		symbol:     "ETH",
		contractId: "vsc1TestContract",
		tipHeight:  1,
	}

	session, err := oracle.fetchChainStatus(chain)
	require.NoError(t, err)
	assert.True(t, session.newBlocksToSubmit, "should bootstrap from genesis on a short chain")
	assert.Len(t, session.chainData, 1, "should fetch the only block")
	assert.Equal(t, uint64(1), session.chainData[0].BlockHeight(), "first block should be genesis (height 1)")
}

func TestFetchChainStatus_BootstrapTransientErrorWaits(t *testing.T) {
	logger := vsclog.Module("test")

	oracle := &ChainOracle{
		ctx:    context.Background(),
		logger: logger,
		contractState: &mockContractState{
			output: contracts.ContractOutput{},
			err:    errors.New("simulated DB outage"),
		},
	}

	chain := &mockChainRelay{
		symbol:     "ETH",
		contractId: "vsc1TestContract",
		tipHeight:  10745000,
	}

	session, err := oracle.fetchChainStatus(chain)
	require.NoError(t, err)
	assert.False(t, session.newBlocksToSubmit, "transient read error should wait, not bootstrap")
}
