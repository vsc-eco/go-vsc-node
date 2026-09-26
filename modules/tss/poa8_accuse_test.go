package tss

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/multiformats/go-multicodec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poa8Net is a five-seat committee with real BLS identities, each node running
// the production ask_sigs handler over its own stored session result.
type poa8Net struct {
	names    []string
	nodes    map[string]*TssManager
	election *elections.ElectionResult
}

func newPoa8Net(t *testing.T, names []string, newEpoch, oldEpoch uint64) *poa8Net {
	t.Helper()
	net := &poa8Net{names: names, nodes: make(map[string]*TssManager)}
	members := make([]elections.ElectionMember, len(names))
	weights := make([]uint64, len(names))
	for i, n := range names {
		mgr := newTestTssManager(t, n)
		seed := sha256.Sum256([]byte("poa8-" + n))
		require.NoError(t, mgr.config.SetBlsPrivKeySeed(hex.EncodeToString(seed[:])))
		did, err := mgr.config.BlsDID()
		require.NoError(t, err)
		members[i] = elections.ElectionMember{Account: n, Key: string(did)}
		weights[i] = 10
		net.nodes[n] = mgr
	}
	net.election = &elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: newEpoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
	}
	old := *net.election
	old.Epoch = oldEpoch
	db := &test_utils.MockElectionDb{Elections: map[uint64]*elections.ElectionResult{newEpoch: net.election, oldEpoch: &old}}
	for _, mgr := range net.nodes {
		mgr.electionDb = db
	}
	return net
}

// statementWeight: the leader builds the statement for `accused` from its own
// result, asks every node through the real handler, and verifies each returned
// BLS signature against the leader's CID. Returns the verified signing weight.
func (net *poa8Net) statementWeight(t *testing.T, leader, sessionId, accused string) uint64 {
	t.Helper()
	lm := net.nodes[leader]
	entry, ok := lm.sessionResults[sessionId]
	require.True(t, ok)
	rec, ok := accuseRecordOf(entry.result)
	if !ok {
		return 0
	}
	stmt, ok := lm.accuseStatement(rec, accused)
	if !ok {
		return 0
	}
	raw, err := common.EncodeDagCbor(stmt)
	require.NoError(t, err)
	stmtCid, err := common.HashBytes(raw, multicodec.DagCbor)
	require.NoError(t, err)

	didOf := make(map[string]dids.BlsDID)
	circuitMembers := make([]dids.Member, 0)
	for _, m := range net.election.Members {
		didOf[m.Account] = dids.BlsDID(m.Key)
		circuitMembers = append(circuitMembers, dids.BlsDID(m.Key))
	}
	circuit, err := dids.NewBlsCircuitGenerator(circuitMembers).Generate(stmtCid)
	require.NoError(t, err)

	weight := uint64(0)
	for _, n := range net.names {
		var got []p2pMessage
		send := func(m p2pMessage) error { got = append(got, m); return nil }
		ask := p2pMessage{Type: "ask_sigs", Account: leader, Data: map[string]interface{}{"session_id": sessionId, "accused": accused}}
		require.NoError(t, p2pSpec{tssMgr: net.nodes[n]}.HandleMessage(context.Background(), "", ask, send))
		for _, m := range got {
			if m.Type != "res_sig" || m.Data["accused"] != accused {
				continue
			}
			added, err := circuit.AddAndVerify(didOf[n], m.Data["sig"].(string))
			if err == nil && added {
				weight += 10
			}
		}
	}
	return weight
}

// The devnet session reshare-260 (ledger 2026-09-25 02:23): magi.test3 attested
// ready and went silent. test1/test4/test5 waited on test3 only; test2 waited on
// all four others. Five seats of weight 10: a commitment needs 50 - 50/3 = 34.
var poa8Waiting = map[string][]string{
	"test1": {"test3"},
	"test2": {"test1", "test3", "test4", "test5"},
	"test4": {"test3"},
	"test5": {"test3"},
}

func (net *poa8Net) storeTimeouts(active consensusversion.Version, sessionId string, bh, newEpoch, oldEpoch uint64) {
	for n, waiting := range poa8Waiting {
		res := TimeoutResult{
			tssMgr:      net.nodes[n],
			SessionId:   sessionId,
			KeyId:       "test-key-main",
			BlockHeight: bh,
			Epoch:       newEpoch,
			OldEpoch:    oldEpoch,
			Accused:     accusedIfActive(active, n, waiting),
		}
		net.nodes[n].sessionResults[sessionId] = sessionResultEntry{result: res, blockHeight: bh}
	}
}

func TestPoa8_WithholderStatementLandsAt090(t *testing.T) {
	names := []string{"test1", "test2", "test3", "test4", "test5"}
	const sid = "reshare-260-0-test-key-main"
	const quorum = 50 - 50/3

	// 0.8.0: no accusations exist, nothing can land: the stall the devnet run
	// showed (0 reshares, 0 blames over 8 sessions).
	before := newPoa8Net(t, names, 4, 3)
	before.storeTimeouts(consensusversion.V0_8_0, sid, 260, 4, 3)
	assert.Equal(t, uint64(0), before.statementWeight(t, "test1", sid, "test3"), "below 0.9.0 no node signs an accusation")

	// 0.9.0: the withholder is named by all four honest nodes, including the
	// one a round behind, and lands; the laggard's extra names do not.
	after := newPoa8Net(t, names, 4, 3)
	after.storeTimeouts(consensusversion.V0_9_0, sid, 260, 4, 3)
	w := after.statementWeight(t, "test1", sid, "test3")
	assert.Equal(t, uint64(40), w)
	assert.GreaterOrEqual(t, w, uint64(quorum), "the withholder's statement reaches 2/3")
	for _, honest := range []string{"test1", "test4", "test5"} {
		got := after.statementWeight(t, "test2", sid, honest)
		assert.Equal(t, uint64(10), got, "only the laggard names %s", honest)
		assert.Less(t, got, uint64(quorum))
	}
}

func TestPoa8_SessionBlameCommitmentUnchanged(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c"}, 4, 3)
	base := TimeoutResult{tssMgr: net.nodes["a"], SessionId: "reshare-10-0-k", KeyId: "k", BlockHeight: 10, Epoch: 4}
	with := base
	with.Accused = []string{"b", "c"}
	with.OldEpoch = 3
	assert.Equal(t, base.Serialize(), with.Serialize(), "accusations never enter the session's own commitment")
	eb := ErrorResult{tssMgr: net.nodes["a"], err: assert.AnError, SessionId: "reshare-10-0-k", KeyId: "k", BlockHeight: 10, Epoch: 4}
	ew := eb
	ew.Accused = []string{"b"}
	assert.Equal(t, eb.Serialize(), ew.Serialize())
}

func TestPoa8_StatementOnlyForReshareAndOwnAccused(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d"}, 4, 3)
	a := net.nodes["a"]
	sign := TimeoutResult{SessionId: "sign-10-0-k", KeyId: "k", BlockHeight: 10, Epoch: 4, Accused: []string{"b"}}
	_, ok := accuseRecordOf(sign)
	assert.False(t, ok, "only reshare sessions produce statements")
	none := TimeoutResult{SessionId: "reshare-10-0-k", KeyId: "k", BlockHeight: 10, Epoch: 4}
	_, ok = accuseRecordOf(none)
	assert.False(t, ok)

	rec, ok := accuseRecordOf(TimeoutResult{SessionId: "reshare-10-0-k", KeyId: "k", BlockHeight: 10, Epoch: 4, OldEpoch: 3, Accused: []string{"b"}})
	require.True(t, ok)
	_, ok = a.accuseStatement(rec, "c")
	assert.False(t, ok, "a node never signs for a party it does not accuse")
	s1, ok := a.accuseStatement(rec, "b")
	require.True(t, ok)
	assert.Equal(t, accuseTypeReshare, s1.Type)
	assert.Equal(t, []string{"b"}, decodeBlameBitset(s1.Commitment, net.election.Members))

	// Two nodes accusing b with different overall sets build the same bytes.
	rec2 := rec
	rec2.Accused = []string{"b", "c", "d"}
	s2, ok := net.nodes["c"].accuseStatement(rec2, "b")
	require.True(t, ok)
	assert.Equal(t, s1, s2)

	// A party only in the old election is encoded against it; in neither, no statement.
	oldOnly := makeElection(3, []string{"x", "a"})
	a.electionDb.(*test_utils.MockElectionDb).Elections[3] = oldOnly
	rec3 := rec
	rec3.Accused = []string{"x", "zz"}
	s3, ok := a.accuseStatement(rec3, "x")
	require.True(t, ok)
	assert.Equal(t, uint64(3), s3.Epoch)
	assert.Equal(t, []string{"x"}, decodeBlameBitset(s3.Commitment, oldOnly.Members))
	_, ok = a.accuseStatement(rec3, "zz")
	assert.False(t, ok)
}

func TestPoa8_AccusedSet(t *testing.T) {
	assert.Equal(t, []string{"a", "d"}, accusedSet("b", []string{"d", "b", "a", "", "a"}))
	assert.Nil(t, accusedIfActive(consensusversion.V0_8_0, "b", []string{"a"}))
	assert.Equal(t, []string{"a"}, accusedIfActive(consensusversion.V0_9_0, "b", []string{"a"}))
}

func TestPoa8_ExclusionCapOneAndThresholdPlusOne(t *testing.T) {
	old := []string{"a", "b", "c", "d", "e"} // threshold(5)=3, keep 4
	got := selectAccusedExclusions(map[string]int{"c": 1, "b": 2, "z": 3}, old, 4, 0)
	assert.Equal(t, map[string]bool{"z": true}, got, "at most one party per reshare, most-named first")

	tie := selectAccusedExclusions(map[string]int{"e": 1, "c": 1}, old, 4, 0)
	assert.Equal(t, map[string]bool{"c": true}, tie, "ties break by account")

	full := selectAccusedExclusions(map[string]int{"a": 3, "z": 1}, []string{"a", "b", "c"}, 3, 0)
	assert.Equal(t, map[string]bool{"z": true}, full, "an old member is never taken below the minimum; a non-old party still can be")

	// Review round 4: blame (33% rule) or a ban already left a new-committee
	// member out; POA-8 must not stack another exclusion on top.
	stacked := selectAccusedExclusions(map[string]int{"b": 5}, old, 4, 1)
	assert.Empty(t, stacked, "no accusation exclusion when blame or ban already excluded someone")
}

// The review PoC: 19 equal seats, 9 colluding. One colluder starves a different
// honest node in each of six failed sessions, and each time every other party
// honestly names that node, so six statements land. Built uncapped, the next
// reshare dropped all six: a 13-member key needing 9 signers, which the
// colluders alone could produce (13 needed with the full set).
func TestPoa8_FramedExclusionsDoNotAddUp(t *testing.T) {
	names := make([]string, 19)
	for i := range names {
		names[i] = string(rune('a'+i)) + "-seat"
	}
	participants := make([]Participant, len(names))
	for i, n := range names {
		participants[i] = Participant{Account: n}
	}
	counts := map[string]int{}
	for _, framed := range names[10:16] { // six honest seats, one statement each
		counts[framed] = 1
	}
	keepOld, keepNew, out := applyAccusedExclusions(participants, participants, counts, len(names), 0)
	require.Len(t, out, 1)
	assert.Len(t, keepOld, 18)
	assert.Len(t, keepNew, 18)
	newThreshold, _ := tss_helpers.GetThreshold(len(keepNew))
	fullThreshold, _ := tss_helpers.GetThreshold(len(names))
	assert.Equal(t, 12, newThreshold+1, "signers needed after one exclusion")
	assert.Equal(t, 13, fullThreshold+1)
	assert.Greater(t, newThreshold+1, 9, "nine colluders cannot sign alone")
}

func TestPoa8_AccusationsTravelInTheirOwnOps(t *testing.T) {
	entry := func(i int) map[string]any { return map[string]any{"i": i} }
	sessions := []map[string]any{entry(0), entry(1)}
	acc := make([]map[string]any, 0)
	for i := 0; i < 23; i++ {
		acc = append(acc, entry(100+i))
	}
	ops := commitmentOpPackets(sessions, acc)
	require.Len(t, ops, 2, "Hive allows 5 custom_json ops per account per block: the broadcast stays at two")
	assert.Equal(t, sessions, ops[0], "session commitments unchanged, in one op")
	assert.Len(t, ops[1], accusePerOp, "at most accusePerOp accusations per broadcast")
	assert.Equal(t, [][]map[string]any{sessions}, commitmentOpPackets(sessions, nil), "no accusations: exactly the old broadcast")
	assert.Len(t, commitmentOpPackets(nil, acc[:1]), 1)
}

func TestPoa8_AccusationsLapseAtTheNextRotation(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d", "e"}, 4, 3)
	a := net.nodes["a"]
	bit := func(acct string) string {
		return a.setToCommitment([]Participant{{Account: acct}}, 4)
	}
	a.tssCommitments = &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{
		"1": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 220, Epoch: 4, Commitment: bit("c")},
		"2": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("c")},
		"3": {Type: accuseTypeReshare, KeyId: "other", BlockHeight: 280, Epoch: 4, Commitment: bit("d")},
		"4": {Type: "blame", KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("e")},
	}}
	// last rotation of k at 240: only the 280 accusation counts; other keys and blames never do
	assert.Equal(t, map[string]int{"c": 1}, a.reshareAccusedCounts("k", 300, 240, 0))
	// rotation at 290 clears it
	assert.Empty(t, a.reshareAccusedCounts("k", 300, 290, 0))
	// the blame window bounds it too
	assert.Empty(t, a.reshareAccusedCounts("k", 300, 0, 290))
}
