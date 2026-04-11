package state_engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recoveryConfigWrapper overrides ConsensusParams with recovery multisig settings.
type recoveryConfigWrapper struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (r *recoveryConfigWrapper) ConsensusParams() params.ConsensusParams {
	return r.cp
}

func mocknetWithRecoveryMultisig(accounts []string, threshold int) systemconfig.SystemConfig {
	base := systemconfig.MocknetConfig()
	p := base.ConsensusParams()
	p.RecoveryMultisigAccounts = append([]string{}, accounts...)
	p.RecoveryMultisigThreshold = threshold
	return &recoveryConfigWrapper{SystemConfig: base, cp: p}
}

func sampleElection(height uint64) elections.ElectionResult {
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{
			Epoch: 1,
			NetId: "vsc-mocknet",
			Type:  "initial",
		},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "alice", Key: "did:key:alice"},
				{Account: "bob", Key: "did:key:bob"},
				{Account: "carol", Key: "did:key:carol"},
			},
			Weights:             []uint64{1, 1, 1},
			ProtocolVersion:     0,
			VersionMajor:        0,
			VersionNonConsensus: 0,
		},
		BlockHeight: height,
		TotalWeight: 3,
	}
}

func TestE2E_ProposeIgnoredWhenProposerNotInCommittee(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	prop, _ := json.Marshal(map[string]interface{}{
		"net_id":        "vsc-mocknet",
		"major":         9,
		"consensus":     9,
		"non_consensus": 0,
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"stranger"},
		Id:            "vsc.propose_consensus_version",
		Json:          string(prop),
	})
	te.processAndWait()

	assert.Nil(t, mem.Snapshot().PendingProposal)
}

func TestE2E_ProposeConsensusVersionSetsPending(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	// First CreateBlock uses block height 1 (see MockReader.witnessBlock).
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	prop, _ := json.Marshal(map[string]interface{}{
		"net_id":        "vsc-mocknet",
		"major":         1,
		"consensus":     1,
		"non_consensus": 0,
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.propose_consensus_version",
		Json:          string(prop),
	})
	te.processAndWait()

	st := mem.Snapshot()
	require.NotNil(t, st.PendingProposal)
	assert.Equal(t, uint64(1), st.PendingProposal.Major)
	assert.Equal(t, uint64(1), st.PendingProposal.Consensus)
}

func TestE2E_TryFinalizeConsensusProposalAdoptsWhenQuorumReady(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	// Target (1, 1, 0): all three witnesses must announce >= that triple.
	te.WitnessDb.ByAccount["alice"] = &witnesses.Witness{Account: "alice", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}
	te.WitnessDb.ByAccount["bob"] = &witnesses.Witness{Account: "bob", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}
	te.WitnessDb.ByAccount["carol"] = &witnesses.Witness{Account: "carol", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}

	prop, _ := json.Marshal(map[string]interface{}{
		"net_id":        "vsc-mocknet",
		"major":         1,
		"consensus":     1,
		"non_consensus": 0,
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.propose_consensus_version",
		Json:          string(prop),
	})
	te.processAndWait()

	// executeProposeConsensusVersion calls TryFinalize immediately; quorum is already met, so pending is cleared in one step.
	adopted := mem.Snapshot().AdoptedVersion
	assert.Equal(t, uint64(1), adopted.Major)
	assert.Equal(t, uint64(1), adopted.Consensus)
	assert.Nil(t, mem.Snapshot().PendingProposal)
}

func TestE2E_TryFinalizeConsensusProposalDeferredUntilWitnessesReady(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	prop, _ := json.Marshal(map[string]interface{}{
		"net_id":        "vsc-mocknet",
		"major":         1,
		"consensus":     1,
		"non_consensus": 0,
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.propose_consensus_version",
		Json:          string(prop),
	})
	te.processAndWait()
	require.NotNil(t, mem.Snapshot().PendingProposal)

	te.WitnessDb.ByAccount["alice"] = &witnesses.Witness{Account: "alice", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}
	te.WitnessDb.ByAccount["bob"] = &witnesses.Witness{Account: "bob", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}
	te.WitnessDb.ByAccount["carol"] = &witnesses.Witness{Account: "carol", VersionMajor: 1, ProtocolVersion: 1, VersionNonConsensus: 0}
	te.SE.TryFinalizeConsensusProposal(1)

	assert.Nil(t, mem.Snapshot().PendingProposal)
	assert.Equal(t, uint64(1), mem.Snapshot().AdoptedVersion.Consensus)
}

func TestE2E_RecoverySuspendThenProposeIsIgnored(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"},
		Id:            "vsc.recovery_suspend",
		Json:          `{}`,
	})
	te.processAndWait()
	assert.True(t, mem.Snapshot().ProcessingSuspended)

	prop, _ := json.Marshal(map[string]interface{}{
		"net_id":        "vsc-mocknet",
		"major":         2,
		"consensus":     0,
		"non_consensus": 0,
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.propose_consensus_version",
		Json:          string(prop),
	})
	te.processAndWait()

	assert.Nil(t, mem.Snapshot().PendingProposal, "propose must be skipped while suspended")
}

func TestE2E_RecoveryRequireVersionClearsSuspensionAndAdopts(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"},
		Id:            "vsc.recovery_suspend",
		Json:          `{}`,
	})
	te.processAndWait()
	require.True(t, mem.Snapshot().ProcessingSuspended)

	req, _ := json.Marshal(map[string]interface{}{
		"major":         1,
		"consensus":     2,
		"non_consensus": 0,
		"reason":        "test",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"},
		Id:            "vsc.recovery_require_version",
		Json:          string(req),
	})
	te.processAndWait()

	st := mem.Snapshot()
	assert.False(t, st.ProcessingSuspended)
	assert.Equal(t, uint64(1), st.AdoptedVersion.Major)
	assert.Equal(t, uint64(2), st.AdoptedVersion.Consensus)
	require.NotNil(t, st.MinRequiredVersion)
}

func TestE2E_RecoveryRequireVersionWithInsufficientSignersNoOp(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob", "carol"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"},
		Id:            "vsc.recovery_suspend",
		Json:          `{}`,
	})
	te.processAndWait()
	require.True(t, mem.Snapshot().ProcessingSuspended)

	req, _ := json.Marshal(map[string]interface{}{"major": 9, "consensus": 9, "non_consensus": 0})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.recovery_require_version",
		Json:          string(req),
	})
	te.processAndWait()

	assert.True(t, mem.Snapshot().ProcessingSuspended, "single signer must not clear suspension")
}

func TestE2E_DisplayConsensusVersionProvisionalWhenSuspended(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID:             "singleton",
		AdoptedVersion: consensusversion.Version{Major: 1, Consensus: 3, NonConsensus: 7},
	})
	sconf := mocknetWithRecoveryMultisig([]string{"alice"}, 1)
	te := newTestEnvWithConsensus(mem, sconf)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.recovery_suspend",
		Json:          `{}`,
	})
	te.processAndWait()

	assert.Equal(t, "1.4.0-p", te.SE.DisplayConsensusVersion())
	assert.True(t, te.SE.ProcessingSuspendedForPool())
}

func TestE2E_TssMinimumConsensusVersionMergesElectionAndAdopted(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	_ = mem.SetAdoptedVersion(context.Background(), consensusversion.Version{Major: 1, Consensus: 5, NonConsensus: 0})
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[10] = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 2, NetId: "vsc-mocknet", Type: "staked"},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members:             []elections.ElectionMember{{Account: "x", Key: "k"}},
			Weights:             []uint64{10},
			ProtocolVersion:     3,
			VersionMajor:        0,
			VersionNonConsensus: 0,
		},
		BlockHeight: 1,
	}
	te.processAndWait()

	v := te.SE.TssMinimumConsensusVersion(10)
	assert.Equal(t, uint64(1), v.Major)
	assert.Equal(t, uint64(5), v.Consensus, "max of election consensus 3 and adopted 5")
}
