package devnet

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TssCommitmentDoc mirrors the tss_commitments MongoDB document shape.
type TssCommitmentDoc struct {
	Type        string                 `bson:"type"`
	BlockHeight uint64                 `bson:"block_height"`
	Epoch       uint64                 `bson:"epoch"`
	Commitment  string                 `bson:"commitment"`
	KeyId       string                 `bson:"key_id"`
	TxId        string                 `bson:"tx_id"`
	PublicKey   *string                `bson:"public_key"`
	Metadata    map[string]interface{} `bson:"metadata,omitempty"`
}

// TssKeyDoc mirrors the tss_keys MongoDB document shape.
type TssKeyDoc struct {
	Id        string `bson:"id"`
	Status    string `bson:"status"`
	PublicKey string `bson:"public_key"`
	Owner     string `bson:"owner"`
	Algo      string `bson:"algo"`
	Epoch     uint64 `bson:"epoch"`
}

// mongoClient returns a connected mongo client for the given node (1-indexed).
// Each node has its own database: magi-1, magi-2, etc.
func (d *Devnet) mongoClient(ctx context.Context) (*mongo.Client, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(d.MongoURI()))
	if err != nil {
		return nil, fmt.Errorf("connecting to mongo: %w", err)
	}
	return client, nil
}

// nodeDbName returns the MongoDB database name for a given node (1-indexed).
func (d *Devnet) nodeDbName(node int) string {
	return fmt.Sprintf("magi-%d", node)
}

// GetCommitments returns all tss_commitments matching the filter from
// a specific node's database.
func (d *Devnet) GetCommitments(ctx context.Context, node int, filter bson.M) ([]TssCommitmentDoc, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("tss_commitments")
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(bson.M{"block_height": -1}))
	if err != nil {
		return nil, fmt.Errorf("querying commitments: %w", err)
	}
	var docs []TssCommitmentDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("decoding commitments: %w", err)
	}
	return docs, nil
}

// GetLatestBlame returns the most recent blame commitment for a key
// from a node's database.
func (d *Devnet) GetLatestBlame(ctx context.Context, node int, keyId string) (*TssCommitmentDoc, error) {
	docs, err := d.GetCommitments(ctx, node, bson.M{
		"key_id": keyId,
		"type":   "blame",
	})
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return &docs[0], nil
}

// GetTssKeys returns all tss_keys matching the filter from a node's database.
func (d *Devnet) GetTssKeys(ctx context.Context, node int, filter bson.M) ([]TssKeyDoc, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("tss_keys")
	cursor, err := coll.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("querying tss_keys: %w", err)
	}
	var docs []TssKeyDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("decoding tss_keys: %w", err)
	}
	return docs, nil
}

// WaitForCommitment polls until a tss_commitment matching the filter
// appears in the given node's database, or the timeout expires.
func (d *Devnet) WaitForCommitment(ctx context.Context, node int, filter bson.M, timeout time.Duration) (*TssCommitmentDoc, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for commitment (filter=%v)", filter)
		}
		docs, err := d.GetCommitments(ctx, node, filter)
		if err != nil {
			return nil, err
		}
		if len(docs) > 0 {
			return &docs[0], nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// getLastProcessedBlock returns the last processed block height for a node.
func (d *Devnet) getLastProcessedBlock(ctx context.Context, node int) (uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("hive_blocks")
	var result struct {
		LastProcessedBlock *uint64 `bson:"last_processed_block"`
	}
	err = coll.FindOne(ctx, bson.M{"type": "metadata"}).Decode(&result)
	if err != nil {
		return 0, err
	}
	if result.LastProcessedBlock == nil {
		return 0, nil
	}
	return *result.LastProcessedBlock, nil
}

// dumpBlockHeight logs the last processed block for a node.
func (d *Devnet) dumpBlockHeight(ctx context.Context, t interface{ Logf(string, ...any) }, node int) {
	bh, err := d.getLastProcessedBlock(ctx, node)
	if err != nil {
		t.Logf("node %d: could not read last processed block: %v", node, err)
	} else {
		t.Logf("node %d: last processed block: %d", node, bh)
	}
}

// WaitForBlockProcessing waits until a node has processed at least
// the given block height.
func (d *Devnet) WaitForBlockProcessing(ctx context.Context, node int, minBlock uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			bh, _ := d.getLastProcessedBlock(ctx, node)
			return fmt.Errorf("node %d only at block %d after %v (need %d)", node, bh, timeout, minBlock)
		}
		bh, err := d.getLastProcessedBlock(ctx, node)
		if err == nil && bh >= minBlock {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// waitForElectionEpoch polls until the elections collection contains
// an election with epoch >= minEpoch.
func (d *Devnet) waitForElectionEpoch(ctx context.Context, node int, minEpoch uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("node %d: election epoch %d never arrived (timeout %v)", node, minEpoch, timeout)
		}
		client, err := d.mongoClient(ctx)
		if err == nil {
			coll := client.Database(d.nodeDbName(node)).Collection("elections")
			count, _ := coll.CountDocuments(ctx, bson.M{"epoch": bson.M{"$gte": minEpoch}})
			client.Disconnect(ctx)
			if count > 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// GetElectionMembers returns the ordered list of account names from
// the election at the given epoch, as stored in MongoDB. This is the
// canonical ordering used for blame/reshare bitset indexing.
func (d *Devnet) GetElectionMembers(ctx context.Context, node int, epoch uint64) ([]string, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("elections")
	var result struct {
		Members []struct {
			Account string `bson:"account"`
		} `bson:"members"`
	}
	err = coll.FindOne(ctx, bson.M{"epoch": epoch}).Decode(&result)
	if err != nil {
		return nil, fmt.Errorf("finding election epoch %d: %w", epoch, err)
	}
	names := make([]string, len(result.Members))
	for i, m := range result.Members {
		names[i] = m.Account
	}
	return names, nil
}

// dumpTssLogs greps a node's Docker logs for TSS-related messages
// (keygen, reshare, blame, session) and logs the last 30 matches.
func (d *Devnet) dumpTssLogs(ctx context.Context, t interface{ Logf(string, ...any) }, node int) {
	container := d.containerName(node)
	// docker logs piped through grep for TSS keywords
	out, err := exec.CommandContext(ctx, "bash", "-c",
		fmt.Sprintf("docker logs %s 2>&1 | grep -iE 'keygen|reshare|blame|tss|session|commitment' | tail -30", container),
	).CombinedOutput()
	if err != nil {
		t.Logf("node %d: could not grep TSS logs: %v", node, err)
		return
	}
	if len(out) == 0 {
		t.Logf("node %d: no TSS-related log lines found", node)
		return
	}
	t.Logf("node %d TSS logs (last 30):\n%s", node, string(out))
}

// dumpContracts logs the contracts collection for a node.
func (d *Devnet) dumpContracts(ctx context.Context, t interface{ Logf(string, ...any) }, node int) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("contracts")
	cursor, err := coll.Find(ctx, bson.M{})
	if err != nil {
		t.Logf("node %d: could not read contracts: %v", node, err)
		return
	}
	var docs []bson.M
	cursor.All(ctx, &docs)
	t.Logf("node %d: contracts collection (%d docs): %+v", node, len(docs), docs)
}

// WaitForTssKey polls until a tss_key matching the filter appears.
func (d *Devnet) WaitForTssKey(ctx context.Context, node int, filter bson.M, timeout time.Duration) (*TssKeyDoc, error) {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for tss_key (filter=%v)", filter)
		}
		docs, err := d.GetTssKeys(ctx, node, filter)
		if err != nil {
			return nil, err
		}
		if len(docs) > 0 {
			return &docs[0], nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
