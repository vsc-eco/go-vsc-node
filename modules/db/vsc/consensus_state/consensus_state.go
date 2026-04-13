package consensus_state

import (
	"context"
	"errors"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	a "vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var _ a.Plugin = (*consensusState)(nil)

type consensusState struct {
	*db.Collection
}

func New(d *vsc.VscDb) ConsensusState {
	return &consensusState{db.NewCollection(d.DbInstance, "chain_consensus_state")}
}

func (c *consensusState) Init() error {
	return c.Collection.Init()
}

func (c *consensusState) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		resolve(nil)
	})
}

func (c *consensusState) Stop() error {
	return nil
}

// ConsensusState is chain-global consensus version and recovery flags.
type ConsensusState interface {
	a.Plugin
	Get(ctx context.Context) (ChainConsensusState, error)
	Upsert(ctx context.Context, state ChainConsensusState) error
	SetPendingProposal(ctx context.Context, p *PendingConsensusProposal) error
	ClearPendingProposal(ctx context.Context) error
	SetAdoptedVersion(ctx context.Context, v consensusversion.Version) error
	SetProcessingSuspended(ctx context.Context, suspended bool) error
	SetMinRequiredAndClearSuspension(ctx context.Context, v consensusversion.Version) error
	SetNextActivation(ctx context.Context, a *ConsensusActivation) error
	ClearNextActivation(ctx context.Context) error
}

func (c *consensusState) Get(ctx context.Context) (ChainConsensusState, error) {
	var out ChainConsensusState
	err := c.Collection.FindOne(ctx, bson.M{"_id": singletonID}).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return defaultState(), nil
	}
	if err != nil {
		return ChainConsensusState{}, err
	}
	return out, nil
}

func defaultState() ChainConsensusState {
	return ChainConsensusState{
		ID:                  singletonID,
		AdoptedVersion:      consensusversion.Version{},
		ProcessingSuspended:   false,
		PendingProposal:     nil,
		MinRequiredVersion:    nil,
		NextActivation:      nil,
	}
}

func (c *consensusState) Upsert(ctx context.Context, state ChainConsensusState) error {
	state.ID = singletonID
	_, err := c.Collection.ReplaceOne(ctx, bson.M{"_id": singletonID}, state, options.Replace().SetUpsert(true))
	return err
}

func (c *consensusState) SetPendingProposal(ctx context.Context, p *PendingConsensusProposal) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$set": bson.M{"pending_proposal": p}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) ClearPendingProposal(ctx context.Context) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$unset": bson.M{"pending_proposal": ""}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) SetAdoptedVersion(ctx context.Context, v consensusversion.Version) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$set": bson.M{"adopted_version": v}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) SetProcessingSuspended(ctx context.Context, suspended bool) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$set": bson.M{"processing_suspended": suspended}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) SetMinRequiredAndClearSuspension(ctx context.Context, v consensusversion.Version) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{
			"$set": bson.M{
				"min_required_version": v,
				"processing_suspended":   false,
				"adopted_version":      v,
			},
			"$unset": bson.M{"pending_proposal": ""},
		},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) SetNextActivation(ctx context.Context, a *ConsensusActivation) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$set": bson.M{"next_activation": a}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) ClearNextActivation(ctx context.Context) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$unset": bson.M{"next_activation": ""}},
		options.Update().SetUpsert(true),
	)
	return err
}
