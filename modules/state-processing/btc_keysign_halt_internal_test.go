package state_engine

import (
	"context"
	"testing"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
)

// btcMocknetConfig is the mocknet config with a BTC mapping contract configured,
// so the theft-flag mirror has a contract to read.
type btcMocknetConfig struct{ systemconfig.SystemConfig }

func (c btcMocknetConfig) OracleParams() params.OracleParams {
	op := c.SystemConfig.OracleParams()
	op.ChainContracts = map[string]string{"BTC": "vsc1btcstub"}
	return op
}

// theftHaltState holds one ChainConsensusState and records theft-halt writes.
type theftHaltState struct {
	consensus_state.ConsensusState
	s consensus_state.ChainConsensusState
}

func (m *theftHaltState) Get(context.Context) (consensus_state.ChainConsensusState, error) {
	return m.s, nil
}

func (m *theftHaltState) SetBtcTheftHalt(_ context.Context, halted bool, height uint64) error {
	m.s.BtcTheftHalted = halted
	return nil
}

// oneElection answers every height with the same election.
type oneElection struct {
	elections.Elections
	e elections.ElectionResult
}

func (o oneElection) GetElectionByHeight(uint64) (elections.ElectionResult, error) { return o.e, nil }

// The "th" theft-flag mirror runs only from the 0.7.0 line. The stored flag starts
// set and the contract has no "th" key: below the line the mirror does not run and
// leaves the flag alone, as the 0.3.0 build (which has no mirror) would; from the
// line it reads the contract and clears it.
func TestTheftHaltMirrorFollowsTheVersion(t *testing.T) {
	for _, tc := range []struct {
		protocolVersion uint64
		wantHalted      bool
	}{
		{3, true},
		{7, false},
	} {
		cs := &theftHaltState{s: consensus_state.ChainConsensusState{BtcTheftHalted: true}}
		se := &StateEngine{
			sconf:          btcMocknetConfig{systemconfig.MocknetConfig()},
			consensusState: cs,
			electionDb: oneElection{e: elections.ElectionResult{
				ElectionDataInfo: elections.ElectionDataInfo{ProtocolVersion: tc.protocolVersion},
			}},
		}
		se.refreshChainConsensusCache()
		se.refreshBtcTheftHalt(10)
		if got := cs.s.BtcTheftHalted; got != tc.wantHalted {
			t.Errorf("version 0.%d: theft halt = %v, want %v", tc.protocolVersion, got, tc.wantHalted)
		}
	}
}
