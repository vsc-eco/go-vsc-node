package tss

import (
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
)

// H13 regression.
//
// reshareAccusedCounts decides who is left out of the next reshare, so an
// incomplete read is not a smaller answer -- it is a different one, and it
// under-accuses whoever happens to sit in the rows that did not arrive. Two
// fail-open paths produced exactly that: the row fetch logged a warning and
// fell THROUGH to count over the partial set, and a row whose epoch election
// could not be read was dropped via GetElection, which flattens every error
// (transient included) to nil.
//
// A wrong exclusion is expensive: only a SUCCESSFUL reshare clears an
// accusation, so excluding the wrong party blocks the key for the whole blame
// window. Failing the tick and retrying is much cheaper.

// partialThenErrorDb returns SOME rows alongside an error, which is what a
// cursor dying mid-iteration looks like. This is the shape the old code fell
// through on.
type partialThenErrorDb struct {
	*test_utils.MockTssCommitmentsDb
	rows []tss_db.TssCommitment
}

func (d partialThenErrorDb) FindCommitmentsSimple(keyId *string, byTypes []string, epoch, fromBlock, toBlock *uint64, limit int) ([]tss_db.TssCommitment, error) {
	return d.rows, errors.New("cursor died mid-iteration")
}

// electionErrDb fails the election read with a non-ErrNoDocuments error: a
// per-node infra blip, not a deterministic absence.
type electionErrDb struct {
	*test_utils.MockElectionDb
}

func (electionErrDb) GetElectionStrict(epoch uint64) (*elections.ElectionResult, error) {
	return nil, errors.New("connection reset by peer")
}

// electionAbsentDb reports a genuine absence, which every node sees alike.
type electionAbsentDb struct {
	*test_utils.MockElectionDb
}

func (electionAbsentDb) GetElectionStrict(epoch uint64) (*elections.ElectionResult, error) {
	return nil, mongo.ErrNoDocuments
}

func TestH13_PartialAccusationReadFailsClosed(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d", "e"}, 4, 3)
	a := net.nodes["a"]
	bit := func(acct string) string {
		return mustSetToCommitment(t, a, []Participant{{Account: acct}}, 4)
	}

	// One row DID arrive. The old code would have counted it and excluded "c"
	// on the strength of a read it knew had failed.
	a.tssCommitments = partialThenErrorDb{
		MockTssCommitmentsDb: &test_utils.MockTssCommitmentsDb{},
		rows: []tss_db.TssCommitment{
			{Type: accuseTypeReshare, KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("c")},
		},
	}

	counts, err := a.reshareAccusedCounts("k", 300, 240, 0)
	require.Error(t, err, "a failed row fetch must be reported, not counted over")
	require.Nil(t, counts, "no partial counts may escape alongside the error")
}

func TestH13_TransientElectionReadFailsClosed(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d", "e"}, 4, 3)
	a := net.nodes["a"]
	bit := func(acct string) string {
		return mustSetToCommitment(t, a, []Participant{{Account: acct}}, 4)
	}

	a.tssCommitments = &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{
		"1": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("c")},
	}}
	// Swap in an election db that blips. Done after the fixture rows are built,
	// so the rows themselves are well-formed.
	a.electionDb = electionErrDb{&test_utils.MockElectionDb{}}

	counts, err := a.reshareAccusedCounts("k", 300, 240, 0)
	require.Error(t, err, "a transient election read error must not silently drop the row")
	require.Nil(t, counts)
}

// The other side of the classification: a genuine absence is deterministic --
// every node sees it identically -- so the row is skipped without an error,
// exactly as before. Getting this wrong in the other direction would make an
// ordinary pruned/absent epoch fail every tick.
func TestH13_AbsentElectionSkipsRowWithoutError(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d", "e"}, 4, 3)
	a := net.nodes["a"]
	bit := func(acct string) string {
		return mustSetToCommitment(t, a, []Participant{{Account: acct}}, 4)
	}

	a.tssCommitments = &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{
		"1": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("c")},
	}}
	a.electionDb = electionAbsentDb{&test_utils.MockElectionDb{}}

	counts, err := a.reshareAccusedCounts("k", 300, 240, 0)
	require.NoError(t, err, "a deterministic absence must not fail the tick")
	require.Empty(t, counts)
}

// The positive control: a clean read still counts exactly as it did before, so
// the fail-closed branches are not rejecting everything.
func TestH13_CleanReadStillCounts(t *testing.T) {
	net := newPoa8Net(t, []string{"a", "b", "c", "d", "e"}, 4, 3)
	a := net.nodes["a"]
	bit := func(acct string) string {
		return mustSetToCommitment(t, a, []Participant{{Account: acct}}, 4)
	}

	a.tssCommitments = &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{
		"1": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 280, Epoch: 4, Commitment: bit("c")},
		"2": {Type: accuseTypeReshare, KeyId: "k", BlockHeight: 285, Epoch: 4, Commitment: bit("c")},
	}}

	counts, err := a.reshareAccusedCounts("k", 300, 240, 0)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"c": 2}, counts)
}
