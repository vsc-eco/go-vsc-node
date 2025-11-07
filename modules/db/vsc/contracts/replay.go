package contracts

import (
	"context"
	"fmt"

	logstream "vsc-node/modules/gql/logstream"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// LoadLogsInRange loads all contract logs between two block heights (inclusive)
// from the contract_state collection, and converts them into logstream.ContractLog
// instances for replay through LogStream.
func (cs *contractState) LoadLogsInRange(fromBlock, toBlock uint64) ([]logstream.ContractLog, error) {
	ctx := context.Background()
	filter := bson.M{
		"block_height": bson.M{
			"$gte": fromBlock,
			"$lte": toBlock,
		},
	}

	findOpts := options.Find().SetSort(bson.M{"block_height": 1})
	cursor, err := cs.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, fmt.Errorf("LoadLogsInRange: query failed: %w", err)
	}
	defer cursor.Close(ctx)

	var logs []logstream.ContractLog
	for cursor.Next(ctx) {
		var out ContractOutput
		if err := cursor.Decode(&out); err != nil {
			return nil, fmt.Errorf("LoadLogsInRange: decode failed: %w", err)
		}

		for _, result := range out.Results {
			for _, logStr := range result.Logs {
				logs = append(logs, logstream.ContractLog{
					BlockHeight:     uint64(out.BlockHeight),
					TxHash:          out.Id, // You can replace this with real TxId if available
					ContractAddress: out.ContractId,
					Log:             logStr,
					Timestamp:       deref(out.Timestamp),
				})
			}
		}
	}

	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("LoadLogsInRange: cursor error: %w", err)
	}

	fmt.Printf("[contracts] Loaded %d logs between blocks %d–%d\n", len(logs), fromBlock, toBlock)
	return logs, nil
}

// Helper to dereference timestamp safely.
func deref(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
