// Package governance_db is the deterministic persistence layer for the
// witness-vote governance engine (see modules/governance for the pure tally
// logic). It stores proposals and per-witness votes in a single Mongo
// collection keyed by (proposal_id, voter): the proposal row is the one with an
// empty voter, votes are the rows with a witness account. Writes are
// idempotent upserts so block replay converges (mirrors tss_db.SetCommitmentData).
package governance_db

import (
	"context"

	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Proposal is the persisted governance-proposal row (the (proposal_id, voter="")
// row). One per proposal_id. CreationBlock anchors both the expiry window and
// the electorate snapshot, so it must be the block of the FIRST vote — which is
// identical on every node because votes are applied in L1 order.
type Proposal struct {
	ProposalId    string `bson:"proposal_id"`
	Voter         string `bson:"voter"` // always "" for the proposal row
	Type          string `bson:"type"`
	Status        string `bson:"status"`
	CreationBlock uint64 `bson:"creation_block"`
	Beneficiary   string `bson:"beneficiary"` // normalized; excluded from the vote
	AppliedBlock  uint64 `bson:"applied_block,omitempty"`
	AppliedTxId   string `bson:"applied_tx_id,omitempty"`

	// slash_restore payload (resolved from the on-chain slash row at creation).
	SlashTxId      string `bson:"slash_tx_id,omitempty"`
	EvidenceKind   string `bson:"evidence_kind,omitempty"`
	SlashedAccount string `bson:"slashed_account,omitempty"`

	// reserve_payout payload.
	Recipient string `bson:"recipient,omitempty"`
	Reason    string `bson:"reason,omitempty"`

	// shared
	Amount int64 `bson:"amount,omitempty"` // sats
}

// ProposalVote is one witness's approve vote (a (proposal_id, voter=account)
// row). One per voter; re-votes upsert the same row.
type ProposalVote struct {
	ProposalId  string `bson:"proposal_id"`
	Voter       string `bson:"voter"` // normalized account
	BlockHeight uint64 `bson:"block_height"`
	TxId        string `bson:"tx_id"`
}

// Governance is the store interface consumed by the state engine.
type Governance interface {
	a.Plugin
	// GetProposal returns the proposal row for an id; ok=false if none exists yet.
	GetProposal(proposalId string) (Proposal, bool, error)
	// SaveProposal upserts the proposal row (voter="").
	SaveProposal(p Proposal) error
	// RecordVote upserts a single witness vote, deduped on (proposal_id, voter).
	RecordVote(v ProposalVote) error
	// GetVotes returns every vote row for a proposal (excludes the proposal row).
	GetVotes(proposalId string) ([]ProposalVote, error)
}

type governance struct {
	*db.Collection
}

var _ Governance = &governance{}

func New(d *vsc.VscDb) Governance {
	return &governance{db.NewCollection(d.DbInstance, "governance_proposals")}
}

func (g *governance) GetProposal(proposalId string) (Proposal, bool, error) {
	res := g.FindOne(context.Background(), bson.M{"proposal_id": proposalId, "voter": ""})
	if err := res.Err(); err != nil {
		if err == mongo.ErrNoDocuments {
			return Proposal{}, false, nil
		}
		return Proposal{}, false, err
	}
	var p Proposal
	if err := res.Decode(&p); err != nil {
		return Proposal{}, false, err
	}
	return p, true, nil
}

func (g *governance) SaveProposal(p Proposal) error {
	p.Voter = "" // the proposal row is always voter=""
	opts := options.FindOneAndUpdate().SetUpsert(true)
	res := g.FindOneAndUpdate(context.Background(),
		bson.M{"proposal_id": p.ProposalId, "voter": ""},
		bson.M{"$set": bson.M{
			"type":            p.Type,
			"status":          p.Status,
			"creation_block":  p.CreationBlock,
			"beneficiary":     p.Beneficiary,
			"applied_block":   p.AppliedBlock,
			"applied_tx_id":   p.AppliedTxId,
			"slash_tx_id":     p.SlashTxId,
			"evidence_kind":   p.EvidenceKind,
			"slashed_account": p.SlashedAccount,
			"recipient":       p.Recipient,
			"reason":          p.Reason,
			"amount":          p.Amount,
		}},
		opts)
	if err := res.Err(); err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	return nil
}

func (g *governance) RecordVote(v ProposalVote) error {
	if v.Voter == "" {
		return nil // a blank voter would collide with the proposal row; ignore
	}
	opts := options.FindOneAndUpdate().SetUpsert(true)
	res := g.FindOneAndUpdate(context.Background(),
		bson.M{"proposal_id": v.ProposalId, "voter": v.Voter},
		bson.M{"$set": bson.M{
			"block_height": v.BlockHeight,
			"tx_id":        v.TxId,
		}},
		opts)
	if err := res.Err(); err != nil && err != mongo.ErrNoDocuments {
		return err
	}
	return nil
}

func (g *governance) GetVotes(proposalId string) ([]ProposalVote, error) {
	cursor, err := g.Find(context.Background(), bson.M{
		"proposal_id": proposalId,
		"voter":       bson.M{"$ne": ""},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())
	out := make([]ProposalVote, 0)
	for cursor.Next(context.Background()) {
		var v ProposalVote
		if err := cursor.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (g *governance) Init() error {
	if err := g.Collection.Init(); err != nil {
		return err
	}
	// Unique (proposal_id, voter): one proposal row (voter="") + one row per
	// voter. Enforces vote dedup at the storage layer.
	return g.CreateIndexIfNotExist(mongo.IndexModel{
		Keys: bson.D{
			{Key: "proposal_id", Value: 1},
			{Key: "voter", Value: 1},
		},
		Options: options.Index().SetUnique(true),
	})
}
