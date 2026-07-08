package state_engine_test

import (
	"encoding/json"
	"testing"

	"vsc-node/lib/test_utils"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func haltJSON(duration uint64, reason string) string {
	b, _ := json.Marshal(map[string]interface{}{"duration": duration, "reason": reason})
	return string(b)
}

func unhaltJSON(haltTxId string) string {
	m := map[string]interface{}{}
	if haltTxId != "" {
		m["halt_tx_id"] = haltTxId
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// TestHalt_SingleMemberSetsBoundedHalt: one committee member sets a bounded
// outbound halt; the entry is height-addressable and auto-expires.
func TestHalt_SingleMemberSetsBoundedHalt(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6) // active 0.6.0

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(100, "test"),
	})
	te.processAndWait()
	h := te.Reader.LastBlock

	halts := mem.Snapshot().OutboundHalts
	require.Len(t, halts, 1)
	assert.Equal(t, "alice", halts[0].Account)
	assert.Equal(t, h, halts[0].SetHeight)
	assert.Equal(t, h+100, halts[0].ExpiryHeight)
	assert.True(t, te.SE.OutboundHaltedAt(h))
	assert.True(t, te.SE.OutboundHaltedAt(h+99))
	assert.False(t, te.SE.OutboundHaltedAt(h+100), "auto-expires at ExpiryHeight")
}

// TestHalt_DurationClamped: an out-of-range duration is clamped to [min,max].
func TestHalt_DurationClamped(t *testing.T) {
	// Too-large duration → HaltMaxBlocks.
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(999999, "big")})
	te.processAndWait()
	h := te.Reader.LastBlock
	got := mem.Snapshot().OutboundHalts
	require.Len(t, got, 1)
	assert.Equal(t, h+stateEngine.HaltMaxBlocks, got[0].ExpiryHeight, "clamped to HaltMaxBlocks")

	// Too-small duration → HaltMinBlocks.
	mem2 := test_utils.NewMockConsensusState()
	te2 := newTestEnvWithConsensus(mem2, nil)
	te2.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)
	te2.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(1, "tiny")})
	te2.processAndWait()
	h2 := te2.Reader.LastBlock
	got2 := mem2.Snapshot().OutboundHalts
	require.Len(t, got2, 1)
	assert.Equal(t, h2+stateEngine.HaltMinBlocks, got2[0].ExpiryHeight, "clamped to HaltMinBlocks")
}

// TestHalt_NonMemberRejected: a non-committee signer cannot halt.
func TestHalt_NonMemberRejected(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"stranger"}, Id: "vsc.halt", Json: haltJSON(100, "x")})
	te.processAndWait()

	assert.Empty(t, mem.Snapshot().OutboundHalts, "only current committee members can halt")
}

// TestHalt_AuthSpoofRejected is the regression test for the review's CRITICAL
// finding: a non-member attacker must not be able to spoof a committee member's
// authorization by putting a "self" key in the attacker-controlled op body. With
// TxHalt.Self tagged json:"-", json.Unmarshal cannot overwrite the trusted
// envelope, so the halt is rejected.
func TestHalt_AuthSpoofRejected(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)

	// Signed by non-member "stranger"; body tries to impersonate member "alice".
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"stranger"},
		Id:            "vsc.halt",
		Json:          `{"self":{"requiredauths":["alice"],"block_height":1},"duration":100}`,
	})
	te.processAndWait()

	assert.Empty(t, mem.Snapshot().OutboundHalts, "auth spoof via the op body must not set a halt")
}

// TestHalt_InertBelow060: below the 0.6.0 floor the op is ignored (replay-safe).
func TestHalt_InertBelow060(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 5) // active 0.5.0

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(100, "x")})
	te.processAndWait()

	assert.Empty(t, mem.Snapshot().OutboundHalts, "halt is inert below the 0.6.0 floor")
}

// TestHalt_StackThenCooldownRejects: distinct validators stack; a second halt
// from the same account within its cooldown is rejected.
func TestHalt_StackThenCooldownRejects(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	te := newTestEnvWithConsensus(mem, nil)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(100, "a")})
	te.processAndWait()
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"bob"}, Id: "vsc.halt", Json: haltJSON(100, "b")})
	te.processAndWait()
	require.Len(t, mem.Snapshot().OutboundHalts, 2, "distinct validators stack")

	// alice again, still within cooldown → rejected.
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(100, "a2")})
	te.processAndWait()
	assert.Len(t, mem.Snapshot().OutboundHalts, 2, "alice re-halt within cooldown is rejected")
}

// TestUnhalt_RecoveryMultisigLiftsEarly: the recovery multisig lifts a halt
// early; the entry is deactivated but retained (cooldown keeps running).
func TestUnhalt_RecoveryMultisigLiftsEarly(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"recov1", "recov2"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(1000, "a")})
	te.processAndWait()
	require.True(t, te.SE.OutboundHaltedAt(te.Reader.LastBlock))

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"recov1", "recov2"}, Id: "vsc.unhalt", Json: unhaltJSON("")})
	te.processAndWait()
	h := te.Reader.LastBlock

	halts := mem.Snapshot().OutboundHalts
	require.Len(t, halts, 1, "entry retained for cooldown accounting")
	assert.Equal(t, h, halts[0].ExpiryHeight, "deactivated at the un-halt height")
	assert.False(t, te.SE.OutboundHaltedAt(h), "no longer frozen after early lift")
}

// TestUnhalt_InsufficientAuthRejected: below the recovery threshold, the halt stands.
func TestUnhalt_InsufficientAuthRejected(t *testing.T) {
	mem := test_utils.NewMockConsensusState()
	sconf := mocknetWithRecoveryMultisig([]string{"recov1", "recov2"}, 2)
	te := newTestEnvWithConsensus(mem, sconf)
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 6)

	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"alice"}, Id: "vsc.halt", Json: haltJSON(1000, "a")})
	te.processAndWait()
	setH := te.Reader.LastBlock

	// Only one of two required recovery sigs.
	te.Creator.CustomJson(stateEngine.MockJson{RequiredAuths: []string{"recov1"}, Id: "vsc.unhalt", Json: unhaltJSON("")})
	te.processAndWait()

	assert.True(t, te.SE.OutboundHaltedAt(setH), "halt still active after a sub-threshold un-halt attempt")
}

// NOTE: the exit-handler guards (withdraw / unstake / consensus_unstake reject
// while frozen) are proven at the CONDITION level by TestExitsFrozenAt — each
// guard is a one-line early-return on exitFrozen(se, height). A full op-level
// test (assert a would-succeed withdraw is rejected under a halt) is feasible in
// principle — a completing withdraw debits L2 and queues a gateway action, so the
// frozen vs unfrozen outcomes DO differ observably — but a reliable
// completing-withdraw baseline (RC + valid inputs + gateway wallet) was not landed
// in this unit harness, so that op-level assertion is exercised on devnet.
