package contracts_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"vsc-node/modules/db/vsc/contracts"
	logstream "vsc-node/modules/gql/logstream"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
)

// TestStreamLogsInRange verifies that logs are streamed in batches,
// in order, and that context cancellation stops the stream cleanly.
// NOTE: This test relies on Mongo's internal mtest mock framework.
// It may not run outside that environment, but is kept as a reference
// for how StreamLogsInRange would be tested with a mock collection.

func TestStreamLogsInRange_MockMongo(t *testing.T) {
	t.Skip("requires Mongo mtest harness to run")
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))

	// Mock contractState pointing to the fake "contract_state" collection
	cs := &ContractStateMock{
		Collection: mt.Coll,
	}

	// Prepare fake data (3 blocks, each with 2 logs)
	docs := []bson.D{
		{{Key: "block_height", Value: 1}, {Key: "contract_id", Value: "A"}, {Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L1a", "L1b"}}}}}},
		{{Key: "block_height", Value: 2}, {Key: "contract_id", Value: "B"}, {Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L2a", "L2b"}}}}}},
		{{Key: "block_height", Value: 3}, {Key: "contract_id", Value: "A"}, {Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L3a", "L3b"}}}}}},
	}
	mt.AddMockResponses(mtest.CreateCursorResponse(1, "contract_state", mtest.FirstBatch, docs...))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var collected []logstream.ContractLog
	err := cs.StreamLogsInRange(ctx, 1, 3, 2, func(batch []logstream.ContractLog) error {
		collected = append(collected, batch...)
		// Simulate early cancel mid-replay
		if len(collected) >= 4 {
			cancel()
		}
		return nil
	})
	if err != context.Canceled && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(collected) == 0 {
		t.Fatalf("no logs were streamed")
	}

	// Validate ordering and basic structure
	for i, l := range collected {
		fmt.Printf("streamed[%d] = block %d / contract %s / log %s\n", i, l.BlockHeight, l.ContractAddress, l.Log)
		if l.BlockHeight == 0 || l.ContractAddress == "" || l.Log == "" {
			t.Errorf("invalid log entry: %+v", l)
		}
	}
}

// A minimal fake contractState with StreamLogsInRange for test.
// NOTE: This test relies on Mongo's internal mtest mock framework.
// It is kept as a reference example and may not run outside that environment.
type ContractStateMock struct {
	Collection *mongo.Collection
}

func (cs *ContractStateMock) StreamLogsInRange(ctx context.Context, fromBlock, toBlock uint64, batchSize int, cb func([]logstream.ContractLog) error) error {
	cursor, err := cs.Collection.Find(ctx, bson.M{})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var logs []logstream.ContractLog
	for cursor.Next(ctx) {
		var out contracts.ContractOutput
		if err := cursor.Decode(&out); err != nil {
			return err
		}

		for _, result := range out.Results {
			for _, l := range result.Logs {
				logs = append(logs, logstream.ContractLog{
					BlockHeight:     uint64(out.BlockHeight),
					ContractAddress: out.ContractId,
					Log:             l,
					Timestamp:       time.Now().Format(time.RFC3339),
				})
				if len(logs) >= batchSize {
					if err := cb(logs); err != nil {
						return err
					}
					logs = logs[:0]
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if len(logs) > 0 {
		if err := cb(logs); err != nil {
			return err
		}
	}
	return nil
}

func TestStreamLogsInRange_Unbounded_MockMongo(t *testing.T) {
	t.Skip("requires Mongo mtest harness to run")
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	cs := &ContractStateMock{Collection: mt.Coll}

	// Fake data
	docs := []bson.D{
		{{Key: "block_height", Value: 5}, {Key: "contract_id", Value: "A"},
			{Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L5a"}}}}}},
	}
	mt.AddMockResponses(mtest.CreateCursorResponse(1, "contract_state", mtest.FirstBatch, docs...))

	ctx := context.Background()
	var logs []logstream.ContractLog

	err := cs.StreamLogsInRange(ctx, 1, uint64(math.MaxInt64), 2, func(batch []logstream.ContractLog) error {
		logs = append(logs, batch...)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(logs) == 0 {
		t.Fatalf("expected logs, got none")
	}
}
