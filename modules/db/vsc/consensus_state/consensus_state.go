package consensus_state

import (
	"context"
	"errors"

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

// ConsensusState is chain-global recovery flags plus the pending scheduled version switch.
type ConsensusState interface {
	a.Plugin
	Get(ctx context.Context) (ChainConsensusState, error)
	Upsert(ctx context.Context, state ChainConsensusState) error
	SetScheduledActivation(ctx context.Context, s *ScheduledActivation) error
	ClearScheduledActivation(ctx context.Context) error
	SetProcessingSuspended(ctx context.Context, suspended bool) error
	// SetForcedActivationAndClearSuspension is the recovery path: schedule a Forced
	// switch and lift the processing halt in one update.
	SetForcedActivationAndClearSuspension(ctx context.Context, s *ScheduledActivation) error
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
		ProcessingSuspended: false,
		ScheduledActivation: nil,
	}
}

func (c *consensusState) Upsert(ctx context.Context, state ChainConsensusState) error {
	state.ID = singletonID
	_, err := c.Collection.ReplaceOne(ctx, bson.M{"_id": singletonID}, state, options.Replace().SetUpsert(true))
	return err
}

func (c *consensusState) SetScheduledActivation(ctx context.Context, s *ScheduledActivation) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$set": bson.M{"scheduled_activation": s}},
		options.Update().SetUpsert(true),
	)
	return err
}

func (c *consensusState) ClearScheduledActivation(ctx context.Context) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{"$unset": bson.M{"scheduled_activation": ""}},
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

func (c *consensusState) SetForcedActivationAndClearSuspension(ctx context.Context, s *ScheduledActivation) error {
	_, err := c.Collection.UpdateOne(ctx,
		bson.M{"_id": singletonID},
		bson.M{
			"$set": bson.M{
				"scheduled_activation": s,
				"processing_suspended": false,
			},
		},
		options.Update().SetUpsert(true),
	)
	return err
}
