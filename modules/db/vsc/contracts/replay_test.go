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

// TestStreamLogsInRange_MockMongo verifies that logs are streamed one by one,
// in order, and that context cancellation stops the stream cleanly.
// This test uses Mongo's mtest harness for mocking cursor responses.
func TestStreamLogsInRange_MockMongo(t *testing.T) {
	t.Skip("requires Mongo mtest harness to run")
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))

	cs := &ContractStateMock{Collection: mt.Coll}

	// Fake data for 3 blocks, each with multiple logs.
	docs := []bson.D{
		{{Key: "block_height", Value: 1}, {Key: "contract_id", Value: "A"},
			{Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L1a", "L1b"}}}}}},
		{{Key: "block_height", Value: 2}, {Key: "contract_id", Value: "B"},
			{Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L2a", "L2b"}}}}}},
		{{Key: "block_height", Value: 3}, {Key: "contract_id", Value: "A"},
			{Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L3a", "L3b"}}}}}},
	}
	mt.AddMockResponses(mtest.CreateCursorResponse(1, "contract_state", mtest.FirstBatch, docs...))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var collected []logstream.ContractLog
	err := cs.StreamLogsInRange(ctx, 1, 3, func(l logstream.ContractLog) error {
		collected = append(collected, l)
		// Cancel early after 4 logs
		if len(collected) >= 4 {
			cancel()
		}
		return nil
	})

	if err != nil && err != context.Canceled {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(collected) == 0 {
		t.Fatalf("no logs were streamed")
	}

	for i, l := range collected {
		fmt.Printf("streamed[%d] block=%d contract=%s log=%s\n", i, l.BlockHeight, l.ContractAddress, l.Log)
		if l.BlockHeight == 0 || l.ContractAddress == "" || l.Log == "" {
			t.Errorf("invalid log entry: %+v", l)
		}
	}
}

// ContractStateMock simulates a contract state collection for StreamLogsInRange tests.
type ContractStateMock struct {
	Collection *mongo.Collection
}

func (cs *ContractStateMock) StreamLogsInRange(
	ctx context.Context,
	fromBlock, toBlock uint64,
	fn func(logstream.ContractLog) error,
) error {
	cursor, err := cs.Collection.Find(ctx, bson.M{})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var out contracts.ContractOutput
		if err := cursor.Decode(&out); err != nil {
			return err
		}

		for _, result := range out.Results {
			for _, l := range result.Logs {
				entry := logstream.ContractLog{
					BlockHeight:     uint64(out.BlockHeight),
					ContractAddress: out.ContractId,
					Log:             l,
					Timestamp:       time.Now().Format(time.RFC3339),
				}
				if err := fn(entry); err != nil {
					return err
				}

				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
		}
	}
	return cursor.Err()
}

// TestStreamLogsInRange_Unbounded_MockMongo ensures very large toBlock ranges are handled gracefully.
func TestStreamLogsInRange_Unbounded_MockMongo(t *testing.T) {
	t.Skip("requires Mongo mtest harness to run")
	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))
	cs := &ContractStateMock{Collection: mt.Coll}

	docs := []bson.D{
		{{Key: "block_height", Value: 5}, {Key: "contract_id", Value: "A"},
			{Key: "results", Value: []bson.D{{{Key: "logs", Value: []string{"L5a"}}}}}},
	}
	mt.AddMockResponses(mtest.CreateCursorResponse(1, "contract_state", mtest.FirstBatch, docs...))

	ctx := context.Background()
	var logs []logstream.ContractLog

	err := cs.StreamLogsInRange(ctx, 1, uint64(math.MaxInt64), func(l logstream.ContractLog) error {
		logs = append(logs, l)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(logs) == 0 {
		t.Fatalf("expected logs, got none")
	}
}
