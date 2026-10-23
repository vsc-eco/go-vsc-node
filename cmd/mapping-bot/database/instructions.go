package database

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// === InstructionStore Methods ===
//
// BOT-ADDR-2: the contract credits a deposit by its INSTRUCTION, derived into one
// address per deposit-matchable vault generation. The bot used to remember only the
// one address handed out at request time (the then-active generation's), forgot it
// 30 days after the first request, and never followed a rotation. Instructions are
// therefore the durable record; the watched addresses are derived from them and the
// contract's registry on every cycle (mapper/deposit_index.go).

// Touch records a request for an instruction: it creates the record or renews it.
// Returns true when the instruction was not known before.
func (s *InstructionStore) Touch(ctx context.Context, instruction string) (bool, error) {
	now := time.Now().UTC()
	res, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": instruction},
		bson.M{"$set": bson.M{"lastRequestedAt": now}, "$setOnInsert": bson.M{"firstRequestedAt": now}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return false, fmt.Errorf("failed to record instruction request: %w", err)
	}
	return res.UpsertedCount == 1, nil
}

// MarkDeposit records that a deposit under this instruction was mapped. An
// instruction that has received a deposit belongs to a real depositor and is
// never pruned (DeleteStale).
func (s *InstructionStore) MarkDeposit(ctx context.Context, instruction string) error {
	now := time.Now().UTC()
	_, err := s.collection.UpdateOne(ctx,
		bson.M{"_id": instruction},
		bson.M{"$set": bson.M{"lastDepositAt": now},
			"$setOnInsert": bson.M{"firstRequestedAt": now, "lastRequestedAt": now}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("failed to record instruction deposit: %w", err)
	}
	return nil
}

// ListSince returns the instructions requested or used at or after since; a zero
// since returns every instruction.
func (s *InstructionStore) ListSince(ctx context.Context, since time.Time) ([]string, error) {
	filter := bson.M{}
	if !since.IsZero() {
		filter = bson.M{"$or": bson.A{
			bson.M{"lastRequestedAt": bson.M{"$gte": since}},
			bson.M{"lastDepositAt": bson.M{"$gte": since}},
		}}
	}
	cursor, err := s.collection.Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, fmt.Errorf("failed to list instructions: %w", err)
	}
	defer cursor.Close(ctx)
	var docs []DepositInstruction
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("failed to decode instructions: %w", err)
	}
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Instruction
	}
	return out, nil
}

// DeleteStale removes instructions that were never used for a deposit and have not
// been requested for longer than age. This bounds what the public request endpoint
// can make the bot watch; an instruction that has seen a deposit is kept.
func (s *InstructionStore) DeleteStale(ctx context.Context, age time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-age)
	res, err := s.collection.DeleteMany(ctx, bson.M{
		"lastRequestedAt": bson.M{"$lt": cutoff},
		"lastDepositAt":   bson.M{"$exists": false},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to delete stale instructions: %w", err)
	}
	return res.DeletedCount, nil
}

// BackfillFromAddresses folds the instructions of the legacy address mappings into
// this store, keeping the earliest and latest request times. Idempotent; run at
// start so a bot upgraded in place keeps watching every instruction it knew. The
// address mappings themselves are left untouched (an older binary still reads them).
func (s *InstructionStore) BackfillFromAddresses(ctx context.Context, addrs *AddressStore) (int, error) {
	cursor, err := addrs.collection.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.M{
			"_id":   "$instruction",
			"first": bson.M{"$min": "$createdAt"},
			"last":  bson.M{"$max": "$createdAt"},
		}}},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to read address mappings for backfill: %w", err)
	}
	defer cursor.Close(ctx)
	var groups []struct {
		Instruction string    `bson:"_id"`
		First       time.Time `bson:"first"`
		Last        time.Time `bson:"last"`
	}
	if err := cursor.All(ctx, &groups); err != nil {
		return 0, fmt.Errorf("failed to decode address mappings for backfill: %w", err)
	}
	n := 0
	for _, g := range groups {
		if g.Instruction == "" {
			continue
		}
		if _, err := s.collection.UpdateOne(ctx,
			bson.M{"_id": g.Instruction},
			bson.M{"$min": bson.M{"firstRequestedAt": g.First}, "$max": bson.M{"lastRequestedAt": g.Last}},
			options.Update().SetUpsert(true),
		); err != nil {
			return n, fmt.Errorf("failed to backfill instruction: %w", err)
		}
		n++
	}
	return n, nil
}
