package gqlgen

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"vsc-node/modules/aggregate"
	governance_db "vsc-node/modules/db/vsc/governance"
)

// mockGovDb is an in-memory governance_db.Governance for resolver tests. The
// embedded aggregate.Plugin (left nil) supplies the Init/Start/Stop methods the
// resolver never calls.
type mockGovDb struct {
	aggregate.Plugin
	proposals []governance_db.Proposal
	votes     map[string][]governance_db.ProposalVote
}

func (m *mockGovDb) GetProposal(id string) (governance_db.Proposal, bool, error) {
	for _, p := range m.proposals {
		if p.ProposalId == id {
			return p, true, nil
		}
	}
	return governance_db.Proposal{}, false, nil
}
func (m *mockGovDb) SaveProposal(p governance_db.Proposal) error {
	return nil
}
func (m *mockGovDb) RecordVote(v governance_db.ProposalVote) error {
	return nil
}
func (m *mockGovDb) GetVotes(id string) ([]governance_db.ProposalVote, error) {
	return m.votes[id], nil
}
func (m *mockGovDb) ListProposals(byProposalId, byType, byStatus *string, offset, limit int) ([]governance_db.Proposal, error) {
	out := make([]governance_db.Proposal, 0, len(m.proposals))
	for _, p := range m.proposals {
		if byProposalId != nil && *byProposalId != "" && p.ProposalId != *byProposalId {
			continue
		}
		if byType != nil && *byType != "" && p.Type != *byType {
			continue
		}
		if byStatus != nil && *byStatus != "" && p.Status != *byStatus {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// TestFindGovernanceProposals_MapsBothKindsAndVotes verifies the resolver maps
// both proposal kinds (slash_restore + reserve_payout) and their votes into the
// GraphQL model, honors the type filter, and is nil-safe when governance is
// unwired.
func TestFindGovernanceProposals_MapsBothKindsAndVotes(t *testing.T) {
	gov := &mockGovDb{
		proposals: []governance_db.Proposal{
			{
				ProposalId: "slash_restore:tx1:alice", Type: "slash_restore", Status: "applied",
				CreationBlock: 100, Beneficiary: "alice", Amount: 100_000,
				SlashTxId: "tx1", EvidenceKind: "vsc_double_block_sign", SlashedAccount: "alice",
				AppliedBlock: 101, AppliedTxId: "v-carol",
			},
			{
				ProposalId: "reserve_payout:abc", Type: "reserve_payout", Status: "open",
				CreationBlock: 200, Beneficiary: "victim", Amount: 60_000,
				Recipient: "victim", Reason: "theft-incident",
			},
		},
		votes: map[string][]governance_db.ProposalVote{
			"slash_restore:tx1:alice": {
				{Voter: "bob", BlockHeight: 100, TxId: "v-bob"},
				{Voter: "carol", BlockHeight: 101, TxId: "v-carol"},
			},
		},
	}
	r := &queryResolver{Resolver: &Resolver{Governance: gov}}

	// No filter: both proposals, with votes mapped on the slash_restore row.
	all, err := r.FindGovernanceProposals(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, all, 2)

	var restore *GovernanceProposal
	for i := range all {
		if all[i].Type == "slash_restore" {
			restore = &all[i]
		}
	}
	require.NotNil(t, restore)
	require.EqualValues(t, 100_000, int64(restore.Amount))
	require.EqualValues(t, 100, uint64(restore.CreationBlock))
	require.Equal(t, "tx1", restore.SlashTxID)
	require.Equal(t, "alice", restore.SlashedAccount)
	require.Len(t, restore.Votes, 2)
	require.EqualValues(t, 101, uint64(restore.Votes[1].BlockHeight))

	// Filter by type returns only the reserve payout, with no votes.
	typ := "reserve_payout"
	rp, err := r.FindGovernanceProposals(context.Background(), &GovernanceProposalFilter{ByType: &typ})
	require.NoError(t, err)
	require.Len(t, rp, 1)
	require.Equal(t, "victim", rp[0].Recipient)
	require.Equal(t, "theft-incident", rp[0].Reason)
	require.Empty(t, rp[0].Votes)

	// Unwired governance → empty result, never a panic or error.
	rNil := &queryResolver{Resolver: &Resolver{}}
	empty, err := rNil.FindGovernanceProposals(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}
