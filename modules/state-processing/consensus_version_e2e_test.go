package state_engine_test

import (
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

// vp builds a coordinated target version (major.consensus).
func vp(major, consensus uint64) consensusversion.Version {
	return consensusversion.Version{Major: major, Consensus: consensus}
}

// propOf returns proposer's candidate proposal from the snapshot (or nil).
func propOf(st consensus_state.ChainConsensusState, proposer string) *consensus_state.VersionProposal {
	for i := range st.VersionProposals {
		if st.VersionProposals[i].Proposer == proposer {
			return &st.VersionProposals[i]
		}
	}
	return nil
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

	s := propOf(mem.Snapshot(), "alice")
	require.NotNil(t, s)
	assert.Equal(t, uint64(1), s.Target.Major)
	assert.Equal(t, uint64(1), s.Target.Consensus)
	assert.Equal(t, uint64(2), s.ActivationEpoch, "default activation epoch is currentEpoch+1")
	assert.Equal(t, uint64(1), s.CreationEpoch)
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

	assert.Empty(t, mem.Snapshot().VersionProposals)
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
	assert.Empty(t, mem.Snapshot().VersionProposals, "target equal to active must be ignored")

	// Below active → ignored.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()
	assert.Empty(t, mem.Snapshot().VersionProposals, "target below active must be ignored")
}

// TestE2E_ProposeMultiCandidateNoPoison: distinct proposers hold independent
// candidate slots, and a junk high target cannot block a legitimate lower one
// (the poison can only fail readiness — it never displaces another candidate).
func TestE2E_ProposeMultiCandidateNoPoison(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1) // active 0.0

	// alice proposes a real 1.1.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()

	// bob proposes a poison 99.0 — it must NOT remove or block alice's candidate.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"bob"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(99, 0, 0),
	})
	te.processAndWait()

	st := mem.Snapshot()
	require.Len(t, st.VersionProposals, 2, "both candidates coexist (one slot per proposer)")
	a := propOf(st, "alice")
	require.NotNil(t, a)
	assert.Equal(t, uint64(1), a.Target.Consensus, "alice's legitimate 1.1 survives the poison")
	b := propOf(st, "bob")
	require.NotNil(t, b)
	assert.Equal(t, uint64(99), b.Target.Major)
}

// TestE2E_ProposeOnePerProposer: a second proposal from the same proposer replaces
// its own slot (never accumulates).
func TestE2E_ProposeOnePerProposer(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 5, 0),
	})
	te.processAndWait()

	st := mem.Snapshot()
	require.Len(t, st.VersionProposals, 1, "alice keeps a single slot")
	assert.Equal(t, uint64(5), st.VersionProposals[0].Target.Consensus, "re-propose replaced alice's own slot")
}

// TestE2E_ProposeSelfVersionGate: a proposer may only propose up to the version it
// is announcing — proposing higher than its witness record is rejected.
func TestE2E_ProposeSelfVersionGate(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)
	// alice announces 1.1; bob has no record (gate skipped for missing records).
	te.WitnessDb.ByAccount = map[string]*witnesses.Witness{
		"alice": {Account: "alice", VersionMajor: 1, ProtocolVersion: 1},
	}

	// alice proposes 1.2 (> announced 1.1) → rejected.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 2, 0),
	})
	te.processAndWait()
	assert.Nil(t, propOf(mem.Snapshot(), "alice"), "proposing above announced version is rejected")

	// alice proposes 1.1 (== announced) → accepted.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(1, 1, 0),
	})
	te.processAndWait()
	require.NotNil(t, propOf(mem.Snapshot(), "alice"), "proposing at announced version is accepted")
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
	a := propOf(mem.Snapshot(), "alice")
	require.NotNil(t, a)
	assert.Equal(t, uint64(5), a.ActivationEpoch)

	// alice re-proposes with a past/current activation epoch (<= current epoch 1) →
	// rejected, so her existing slot is unchanged.
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.propose_consensus_version", Json: proposeJSON(2, 0, 1),
	})
	te.processAndWait()
	a = propOf(mem.Snapshot(), "alice")
	require.NotNil(t, a)
	assert.Equal(t, uint64(1), a.Target.Major, "proposal with past activation epoch rejected; prior slot kept")
	assert.Equal(t, uint64(5), a.ActivationEpoch)
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
	assert.Empty(t, mem.Snapshot().VersionProposals, "propose must be skipped while suspended")
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
	require.NotNil(t, st.ForcedActivation)
	assert.True(t, st.ForcedActivation.Forced, "recovery installs a forced activation")
	assert.Equal(t, uint64(1), st.ForcedActivation.Target.Major)
	assert.Equal(t, uint64(2), st.ForcedActivation.Target.Consensus)
	assert.Equal(t, uint64(2), st.ForcedActivation.ActivationEpoch, "recovery activates next epoch")
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

	assert.Nil(t, mem.Snapshot().ForcedActivation, "must not schedule without suspend-then-resume flow")
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

// TestE2E_VersionProposalsForHeightFilter: a candidate is height-addressable — not
// visible at its own block height, visible strictly after it (keeps election CIDs pure).
func TestE2E_VersionProposalsForHeightFilter(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID: "singleton",
		VersionProposals: []consensus_state.VersionProposal{
			{Target: vp(1, 1), ActivationEpoch: 3, BlockHeight: 10, Proposer: "alice"},
		},
	})
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = sampleElection(1)
	te.processAndWait() // load cache from mem

	assert.Empty(t, te.SE.VersionProposalsForHeight(10), "candidate not visible at its own block height")
	assert.Len(t, te.SE.VersionProposalsForHeight(11), 1, "candidate visible after its block height")
}

// TestE2E_PruneVersionProposalsAfterElection covers the deterministic GC: adopted /
// sub-floor / hard-expired / no-traction candidates are dropped, fresh ones kept, and
// an adopted recovery override is cleared.
func TestE2E_PruneVersionProposalsAfterElection(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	mem.ReplaceState(consensus_state.ChainConsensusState{
		ID: "singleton",
		// Forced override at 1.2 — adopted once the floor reaches it.
		ForcedActivation: &consensus_state.VersionProposal{
			Target: vp(1, 2), Forced: true, Proposer: "alice",
		},
		VersionProposals: []consensus_state.VersionProposal{
			{Target: vp(1, 2), Proposer: "a", CreationEpoch: 1, ExpiryEpoch: 100},     // == floor → drop (adopted)
			{Target: vp(1, 1), Proposer: "b", CreationEpoch: 1, ExpiryEpoch: 100},     // < floor → drop (sub-floor)
			{Target: vp(1, 3), Proposer: "c", CreationEpoch: 1, ExpiryEpoch: 5},       // expired (5 <= epoch 10) → drop
			{Target: vp(1, 4), Proposer: "alice", CreationEpoch: 1, ExpiryEpoch: 100}, // no traction past fast-fail → drop
			{Target: vp(1, 5), Proposer: "d", CreationEpoch: 9, ExpiryEpoch: 100},     // fresh (within fast-fail) → keep
		},
	})
	te := newTestEnvWithConsensus(mem, nil)
	te.processAndWait() // load cache from mem

	// Ratified election: epoch 10, floor 1.2 (members announce only the floor, so no
	// member meets target 1.4 → that candidate has no traction).
	elec := versionedElection(100, 10, 1, 2)
	te.SE.PruneVersionProposalsAfterElection(elec)

	st := mem.Snapshot()
	assert.Nil(t, st.ForcedActivation, "adopted forced override cleared")
	require.Len(t, st.VersionProposals, 1, "only the fresh, still-viable candidate survives")
	assert.Equal(t, "d", st.VersionProposals[0].Proposer)
	assert.Equal(t, uint64(5), st.VersionProposals[0].Target.Consensus)
}
