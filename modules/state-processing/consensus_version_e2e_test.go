package state_engine_test

import (
	"encoding/json"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
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

// sampleElection: epoch 1, three members, active version 0.0.0.
func sampleElection(height uint64) elections.ElectionResult {
	return versionedElection(height, 1, 0, 0)
}

// versionedElection builds an election with a specific active major.consensus.
func versionedElection(height, epoch, major, consensus uint64) elections.ElectionResult {
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch, NetId: "vsc-mocknet", Type: "initial"},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "alice", Key: "did:key:alice"},
				{Account: "bob", Key: "did:key:bob"},
				{Account: "carol", Key: "did:key:carol"},
			},
			Weights:         []uint64{1, 1, 1},
			VersionMajor:    major,
			ProtocolVersion: consensus,
		},
		BlockHeight: height,
		TotalWeight: 3,
	}
}

func proposeJSON(major, consensus, activationEpoch uint64) string {
	b, _ := json.Marshal(map[string]interface{}{
		"net_id": "vsc-mocknet", "major": major, "consensus": consensus, "activation_epoch": activationEpoch,
	})
	return string(b)
}

func TestE2E_ProposeSchedulesActivation(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.propose_consensus_version",
		Json:          proposeJSON(1, 1, 0), // 0 -> default activation epoch
	})
	te.processAndWait()

	s := mem.Snapshot().ScheduledActivation
	require.NotNil(t, s)
	assert.Equal(t, uint64(1), s.TargetMajor)
	assert.Equal(t, uint64(1), s.TargetConsensus)
	assert.Equal(t, uint64(2), s.ActivationEpoch, "default activation epoch is currentEpoch+1")
	assert.False(t, s.Forced)
	assert.Equal(t, "alice", s.Proposer)
}

func TestE2E_ProposeIgnoredWhenProposerNotInCommittee(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"stranger"},
		Id:            "vsc.propose_consensus_version",
		Json:          proposeJSON(9, 9, 0),
	})
	te.processAndWait()

	assert.Nil(t, mem.Snapshot().ScheduledActivation)
}

func TestE2E_ProposeBelowOrEqualActiveIgnored(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 1, 2) // active 1.2

	// Equal to active → ignored.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 2, 0),
	})
	te.processAndWait()
	assert.Nil(t, mem.Snapshot().ScheduledActivation, "target equal to active must be ignored")

	// Below active → ignored.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()
	assert.Nil(t, mem.Snapshot().ScheduledActivation, "target below active must be ignored")
}

func TestE2E_ProposeMonotoneReplace(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1) // active 0.0

	// Schedule 1.1.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()
	require.NotNil(t, mem.Snapshot().ScheduledActivation)
	assert.Equal(t, uint64(1), mem.Snapshot().ScheduledActivation.TargetConsensus)

	// Lower target (1.0) must NOT replace.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"bob"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 0, 0),
	})
	te.processAndWait()
	assert.Equal(t, uint64(1), mem.Snapshot().ScheduledActivation.TargetConsensus, "lower target must not replace")

	// Strictly higher target (2.0) replaces.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"carol"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(2, 0, 0),
	})
	te.processAndWait()
	assert.Equal(t, uint64(2), mem.Snapshot().ScheduledActivation.TargetMajor, "higher target must replace")
}

func TestE2E_ProposeCustomAndPastActivationEpoch(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1) // epoch 1

	// Future activation epoch honored.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 5),
	})
	te.processAndWait()
	require.NotNil(t, mem.Snapshot().ScheduledActivation)
	assert.Equal(t, uint64(5), mem.Snapshot().ScheduledActivation.ActivationEpoch)

	// Past/current activation epoch (<= current epoch 1) rejected — a strictly higher target
	// with a bad epoch must not replace the existing schedule.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(2, 0, 1),
	})
	te.processAndWait()
	assert.Equal(t, uint64(1), mem.Snapshot().ScheduledActivation.TargetMajor, "proposal with past activation epoch rejected")
}

func TestE2E_ProposeIgnoredWhileSuspended(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_suspend", Json: `{}`,
	})
	te.processAndWait()
	require.True(t, mem.Snapshot().ProcessingSuspended)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(2, 0, 0),
	})
	te.processAndWait()
	assert.Nil(t, mem.Snapshot().ScheduledActivation, "propose must be skipped while suspended")
}

func TestE2E_RecoverySuspendSetsFlag(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_suspend", Json: `{}`,
	})
	te.processAndWait()
	assert.True(t, mem.Snapshot().ProcessingSuspended)
}

func TestE2E_RecoveryRequireVersionForcedScheduleAndClears(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1) // epoch 1 → recovery activates epoch 2

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_suspend", Json: `{}`,
	})
	te.processAndWait()
	require.True(t, mem.Snapshot().ProcessingSuspended)

	req, _ := json.Marshal(map[string]interface{}{"major": 1, "consensus": 2, "reason": "test"})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_require_version", Json: string(req),
	})
	te.processAndWait()

	st := mem.Snapshot()
	assert.False(t, st.ProcessingSuspended)
	require.NotNil(t, st.ScheduledActivation)
	assert.True(t, st.ScheduledActivation.Forced, "recovery schedules a forced activation")
	assert.Equal(t, uint64(1), st.ScheduledActivation.TargetMajor)
	assert.Equal(t, uint64(2), st.ScheduledActivation.TargetConsensus)
	assert.Equal(t, uint64(2), st.ScheduledActivation.ActivationEpoch, "recovery activates next epoch")
}

func TestE2E_RecoveryRequireVersionWithoutPriorSuspendNoOp(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	require.False(t, mem.Snapshot().ProcessingSuspended)

	req, _ := json.Marshal(map[string]interface{}{"major": 9, "consensus": 1})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_require_version", Json: string(req),
	})
	te.processAndWait()

	assert.Nil(t, mem.Snapshot().ScheduledActivation, "must not schedule without suspend-then-resume flow")
}

func TestE2E_RecoveryRequireVersionWithInsufficientSignersNoOp(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice", "bob", "carol"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice", "bob"}, Id: "vsc.recovery_suspend", Json: `{}`,
	})
	te.processAndWait()
	require.True(t, mem.Snapshot().ProcessingSuspended)

	req, _ := json.Marshal(map[string]interface{}{"major": 9, "consensus": 9})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.recovery_require_version", Json: string(req),
	})
	te.processAndWait()

	assert.True(t, mem.Snapshot().ProcessingSuspended, "single signer must not clear suspension")
}

func TestE2E_DisplayConsensusVersionProvisionalWhenSuspended(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"alice"}, 1)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 1, 3) // active 1.3

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.recovery_suspend", Json: `{}`,
	})
	te.processAndWait()

	assert.Equal(t, "1.4.0-p", te.SE.DisplayConsensusVersion())
	assert.True(t, te.SE.ProcessingSuspendedForPool())
}

func TestE2E_ActiveConsensusVersionPureOfHeight(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[10] = versionedElection(10, 2, 1, 3)

	v := te.SE.TssMinimumConsensusVersion(10)
	assert.Equal(t, uint64(1), v.Major)
	assert.Equal(t, uint64(3), v.Consensus, "active version is the election's ResultVersion, no live merge")
}

func TestE2E_ScheduledActivationForHeightFilter(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID: "singleton",
		ScheduledActivation: &consensus_state.ScheduledActivation{
			TargetMajor: 1, TargetConsensus: 1, ActivationEpoch: 3, BlockHeight: 10,
		},
	})
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)
	te.processAndWait() // load cache from mem

	assert.Nil(t, te.SE.ScheduledActivationForHeight(10), "schedule not visible at its own block height")
	assert.NotNil(t, te.SE.ScheduledActivationForHeight(11), "schedule visible after its block height")
}
