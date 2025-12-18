package contracts

import (
	"context"
	"fmt"

	"vsc-node/modules/db/vsc/hive_blocks"
	logstream "vsc-node/modules/gql/logstream"

	"go.mongodb.org/mongo-driver/bson"
)

// StreamLogsInRange streams all contract logs between two block heights (inclusive)
// and passes each log to the provided callback function.
//
// This avoids loading large datasets into memory by processing logs
// one document at a time. The function stops when all logs are streamed
// or when an error occurs.
func (cs *contractState) StreamLogsInRange(
	fromBlock, toBlock uint64,
	fn func(logstream.ContractLog) error,
) error {
	ctx := context.Background()

	// Filter by block height range.
	filters := bson.D{{
		Key: "block_height",
		Value: bson.D{
			{Key: "$gte", Value: fromBlock},
			{Key: "$lte", Value: toBlock},
		},
	}}

	// Limit to a reasonable number of documents per query.
	const maxDocs = 200_000
	pipe := hive_blocks.GetAggTimestampPipeline(filters, "block_height", "timestamp", 0, maxDocs)
	cursor, err := cs.Aggregate(ctx, pipe)
	if err != nil {
		return fmt.Errorf("StreamLogsInRange: aggregate failed: %w", err)
	}
	defer cursor.Close(ctx)

	count := 0
	for cursor.Next(ctx) {
		var out ContractOutput
		if err := cursor.Decode(&out); err != nil {
			return fmt.Errorf("StreamLogsInRange: decode failed: %w", err)
		}

		ts := ""
		if out.Timestamp != nil {
			ts = *out.Timestamp
		}

		// Emit each log through the callback.
		for _, result := range out.Results {
			for _, logStr := range result.Logs {
				log := logstream.ContractLog{
					BlockHeight:     uint64(out.BlockHeight),
					TxID:            out.Inputs[0],
					ContractAddress: out.ContractId,
					Log:             logStr,
					Timestamp:       ts,
				}

				if err := fn(log); err != nil {
					return fmt.Errorf("StreamLogsInRange: callback failed: %w", err)
				}
				count++
			}
		}
	}

	if err := cursor.Err(); err != nil {
		return fmt.Errorf("StreamLogsInRange: cursor error: %w", err)
	}

	fmt.Printf("[contracts] Streamed %d logs between blocks %d–%d\n", count, fromBlock, toBlock)
	return nil
}
