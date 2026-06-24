package state_engine_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	governance_db "vsc-node/modules/db/vsc/governance"
	governance "vsc-node/modules/governance"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// memGovDb is an in-memory governance_db.Governance for handler tests.
type memGovDb struct {
	aggregate.Plugin
	proposals map[string]governance_db.Proposal
	votes     map[string][]governance_db.ProposalVote
}

func newMemGovDb() *memGovDb {
	return &memGovDb{
		proposals: map[string]governance_db.Proposal{},
		votes:     map[string][]governance_db.ProposalVote{},
	}
}

func (m *memGovDb) GetProposal(id string) (governance_db.Proposal, bool, error) {
	p, ok := m.proposals[id]
	return p, ok, nil
}
func (m *memGovDb) SaveProposal(p governance_db.Proposal) error {
	p.Voter = ""
	m.proposals[p.ProposalId] = p
	return nil
}
func (m *memGovDb) RecordVote(v governance_db.ProposalVote) error {
	if v.Voter == "" {
		return nil
	}
	for i, ex := range m.votes[v.ProposalId] {
		if ex.Voter == v.Voter {
			m.votes[v.ProposalId][i] = v // dedup: replace
			return nil
		}
	}
	m.votes[v.ProposalId] = append(m.votes[v.ProposalId], v)
	return nil
}
func (m *memGovDb) GetVotes(id string) ([]governance_db.ProposalVote, error) {
	return m.votes[id], nil
}

// fourMemberElection: alice (the slashed beneficiary) + bob, carol, dave, equal
// weight. Effective electorate excludes alice → 3; threshold = ceil(2*3/3) = 2.
func fourMemberElection() *test_utils.MockElectionDb {
	elec := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "alice"}, {Account: "bob"}, {Account: "carol"}, {Account: "dave"},
			},
			Weights: []uint64{1, 1, 1, 1},
		},
		TotalWeight: 4,
	}
	return &test_utils.MockElectionDb{ElectionsByHeight: map[uint64]elections.ElectionResult{0: elec}}
}

func wireRestoreFixture(t *testing.T) (*chainOpFixture, *memGovDb) {
	t.Helper()
	f := newChainOpFixture(t)
	gov := newMemGovDb()
	f.se.WireGovernanceForTest(gov, fourMemberElection(), systemconfig.MocknetConfig())
	return f, gov
}

const restorePayload = `{"id":"slash-restore-1","account":"alice"}`

func reverseCredit(db *test_utils.MockLedgerDb) int64 {
	var total int64
	for _, r := range db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			total += r.Amount
		}
	}
	return total
}

// TestSlashRestore_ThresholdRestoresBond is the happy path: a real slash, then
// witness votes; the bond is re-credited only once 2/3 of the
// beneficiary-excluded electorate has voted. Also exercises vote dedup.
func TestSlashRestore_ThresholdRestoresBond(t *testing.T) {
	f, gov := wireRestoreFixture(t)
	f.fireSlash(t, "slash-restore-1", 100, 100) // alice slashed 10% of 1_000_000 = 100_000; maturity 200

	propID := "slash_restore:slash-restore-1:alice"

	// First vote: proposal created, 1 of 3 — below threshold, no restore.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "bob", "v-bob", 110)
	require.EqualValues(t, 0, reverseCredit(f.db), "no restore below threshold")
	require.Len(t, gov.votes[propID], 1)

	// Duplicate vote from bob: still 1 counting vote, no restore.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "bob", "v-bob-2", 110)
	require.Len(t, gov.votes[propID], 1, "duplicate voter must not double-count")
	require.EqualValues(t, 0, reverseCredit(f.db))

	// Second distinct voter crosses 2/3 → bond restored.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "carol", "v-carol", 111)
	require.EqualValues(t, 100_000, reverseCredit(f.db), "threshold crossed → full bond re-credited")

	p, ok, _ := gov.GetProposal(propID)
	require.True(t, ok)
	require.Equal(t, string(governance.StatusApplied), p.Status)

	// A late vote after apply is a no-op (terminal).
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "dave", "v-dave", 112)
	require.EqualValues(t, 100_000, reverseCredit(f.db), "no double restore after applied")
}

// TestSlashRestore_BeneficiaryVoteExcluded: the slashed account's own vote never
// counts toward its restoration.
func TestSlashRestore_BeneficiaryVoteExcluded(t *testing.T) {
	f, _ := wireRestoreFixture(t)
	f.fireSlash(t, "slash-restore-1", 100, 100)

	// alice (beneficiary) + bob = only 1 counting vote (bob); below threshold 2.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "alice", "v-alice", 110)
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "bob", "v-bob", 111)
	require.EqualValues(t, 0, reverseCredit(f.db), "beneficiary self-vote must not count")
}

// TestSlashRestore_NonMemberIgnored: a vote from a non-electorate account is
// ignored and does not even create the proposal.
func TestSlashRestore_NonMemberIgnored(t *testing.T) {
	f, gov := wireRestoreFixture(t)
	f.fireSlash(t, "slash-restore-1", 100, 100)

	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "eve", "v-eve", 110)
	_, ok, _ := gov.GetProposal("slash_restore:slash-restore-1:alice")
	require.False(t, ok, "non-member must not create a proposal")
	require.EqualValues(t, 0, reverseCredit(f.db))
}

// TestSlashRestore_ExpiresWithoutQuorum: once past the effective window the
// proposal is terminal-expired and a later quorum cannot restore.
func TestSlashRestore_ExpiresWithoutQuorum(t *testing.T) {
	f, gov := wireRestoreFixture(t)
	f.fireSlash(t, "slash-restore-1", 100, 100) // maturity 200

	// Create + 1 vote at block 110.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "bob", "v-bob", 110)
	// Maturity (200) caps the window well before the 3-day expiry, so a vote at
	// block 250 is past expiry → terminal.
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "carol", "v-carol", 250)

	p, ok, _ := gov.GetProposal("slash_restore:slash-restore-1:alice")
	require.True(t, ok)
	require.Equal(t, string(governance.StatusExpired), p.Status)
	require.EqualValues(t, 0, reverseCredit(f.db), "expired proposal cannot restore")

	// Even a fresh quorum after expiry does nothing (one-shot, terminal).
	f.se.HandleSlashRestoreVoteForTest([]byte(restorePayload), "dave", "v-dave", 260)
	require.EqualValues(t, 0, reverseCredit(f.db))
}
