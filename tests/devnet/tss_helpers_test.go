package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// -----------------------------------------------------------------
// Shared helpers for TSS deterministic tests.
//
// These helpers use the older "direct Hive RPC + manual MongoDB"
// pattern from the initial test implementation. Milo's newer tests
// (blame_*.go) use the Devnet method-based API (d.WaitForCommitment,
// d.GetCommitments, etc.) which is preferred for new tests.
// -----------------------------------------------------------------

type hiveGlobalProps struct {
	HeadBlockNumber int `json:"head_block_number"`
}

func getHeadBlock(hiveRPC string) (int, error) {
	body := `{"jsonrpc":"2.0","method":"database_api.get_dynamic_global_properties","id":1}`
	resp, err := http.Post(hiveRPC, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Result hiveGlobalProps `json:"result"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, fmt.Errorf("unmarshal: %w (body: %s)", err, string(raw))
	}
	return result.Result.HeadBlockNumber, nil
}

func waitForBlock(t *testing.T, hiveRPC string, target int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		head, err := getHeadBlock(hiveRPC)
		if err == nil && head >= target {
			t.Logf("reached block %d (target %d)", head, target)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for block %d (current: %d, err: %v)", target, head, err)
		}
		time.Sleep(2 * time.Second)
	}
}

type tssCommitmentDoc struct {
	Type        string `bson:"type"`
	KeyId       string `bson:"key_id"`
	Epoch       uint64 `bson:"epoch"`
	BlockHeight uint64 `bson:"block_height"`
	Commitment  string `bson:"commitment"`
}

func connectMongo(t *testing.T, mongoURI string) (*mongo.Client, *mongo.Collection) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	return client, client.Database("magi-1").Collection("tss_commitments")
}

func waitForCommitment(t *testing.T, mongoURI string, commitType string, timeout time.Duration) tssCommitmentDoc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client, coll := connectMongo(t, mongoURI)
	defer client.Disconnect(ctx)

	for {
		var doc tssCommitmentDoc
		err := coll.FindOne(ctx, bson.M{"type": commitType},
			options.FindOne().SetSort(bson.M{"block_height": -1})).Decode(&doc)
		if err == nil {
			t.Logf("found %s commitment: keyId=%s epoch=%d block=%d", commitType, doc.KeyId, doc.Epoch, doc.BlockHeight)
			return doc
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %s commitment in MongoDB", commitType)
			return tssCommitmentDoc{}
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForCommitmentAfter waits for a commitment of the given type with block_height > afterBlock.
func waitForCommitmentAfter(t *testing.T, mongoURI string, commitType string, afterBlock uint64, timeout time.Duration) tssCommitmentDoc {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client, coll := connectMongo(t, mongoURI)
	defer client.Disconnect(ctx)

	filter := bson.M{"type": commitType, "block_height": bson.M{"$gt": afterBlock}}
	for {
		var doc tssCommitmentDoc
		err := coll.FindOne(ctx, filter,
			options.FindOne().SetSort(bson.M{"block_height": -1})).Decode(&doc)
		if err == nil {
			t.Logf("found %s commitment after block %d: keyId=%s epoch=%d block=%d",
				commitType, afterBlock, doc.KeyId, doc.Epoch, doc.BlockHeight)
			return doc
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %s commitment after block %d", commitType, afterBlock)
			return tssCommitmentDoc{}
		case <-time.After(2 * time.Second):
		}
	}
}

// insertTssKey inserts a TSS key record directly into MongoDB to trigger keygen.
// On a real network this would be done by a contract calling tss.create_key.
func insertTssKey(t *testing.T, mongoURI string, keyId string, algo string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _ := connectMongo(t, mongoURI)
	defer client.Disconnect(ctx)
	// Insert into ALL node databases (magi-1 through magi-N).
	// The devnet-setup default prefix is "magi".
	for n := 1; n <= 10; n++ {
		dbName := fmt.Sprintf("magi-%d", n)
		coll := client.Database(dbName).Collection("tss_keys")
		_, err := coll.UpdateOne(ctx,
			bson.M{"id": keyId},
			bson.M{"$set": bson.M{"id": keyId, "algo": algo, "status": "created", "epochs": 100}},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			// DB might not exist for nodes > cfg.Nodes, that's fine
			break
		}
	}
	t.Logf("inserted TSS key %s (algo=%s) into MongoDB", keyId, algo)
}

func countCommitments(t *testing.T, mongoURI string, filter bson.M) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, coll := connectMongo(t, mongoURI)
	defer client.Disconnect(ctx)
	n, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		t.Fatalf("count documents: %v", err)
	}
	return n
}

func assertNoSSIDMismatch(t *testing.T, d *Devnet, ctx context.Context, nodes int) {
	t.Helper()
	for i := 1; i <= nodes; i++ {
		logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if err != nil {
			t.Logf("warning: could not get logs for magi-%d: %v", i, err)
			continue
		}
		if strings.Contains(logs, "ssid mismatch") {
			t.Errorf("SSID MISMATCH in magi-%d logs!", i)
		}
	}
}

func nodeLogsContain(d *Devnet, ctx context.Context, node int, substr string) bool {
	logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
	if err != nil {
		return false
	}
	return strings.Contains(logs, substr)
}

func anyNodeLogsContain(d *Devnet, ctx context.Context, nodes int, substr string) (int, bool) {
	for i := 1; i <= nodes; i++ {
		if nodeLogsContain(d, ctx, i, substr) {
			return i, true
		}
	}
	return 0, false
}

func dumpNodeLogs(t *testing.T, d *Devnet, ctx context.Context, nodes int, lines int) {
	t.Helper()
	for i := 1; i <= nodes; i++ {
		logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
		if logs == "" {
			continue
		}
		logLines := strings.Split(logs, "\n")
		start := len(logLines) - lines
		if start < 0 {
			start = 0
		}
		t.Logf("=== magi-%d (last %d lines) ===\n%s", i, lines, strings.Join(logLines[start:], "\n"))
	}
}

// rotateInterval is the value used in tssTestConfig. Tests use this to compute
// reshare block boundaries instead of hardcoding 100.
const testRotateInterval = 20

// nextReshareBoundary returns the next block that is a multiple of rotateInterval.
func nextReshareBoundary(head int) int {
	ri := testRotateInterval
	return ((head / ri) + 1) * ri
}

// tssTestConfig returns a devnet Config with fast TSS intervals for testing.
// RotateInterval=20 (~60s), SignInterval=10, ReadinessOffset=5, generous timeouts.
func tssTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = true  // TSS tests don't need contract deployment funds
	cfg.LogLevel = "error,tss=trace"
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 20, // elections every ~60s (20 blocks * 3s)
		},
		TssParams: &params.TssParams{
			RotateInterval:     uint64(testRotateInterval), // reshare every 20 blocks (~60s)
			SignInterval:       10,                          // sign every 10 blocks
			ReadinessOffset:    5,                           // broadcast readiness 5 blocks before reshare
			ReshareTimeout:     2 * time.Minute,             // reshare round 1 messages are ~175KB, need time to propagate
			DefaultTimeout:     1 * time.Minute,
			CommitDelay:        2 * time.Second,
			WaitForSigsTimeout: 10 * time.Second,
			ReshareSyncDelay:   2 * time.Second,
			PreParamsTimeout:   10 * time.Minute,            // generous timeout for loaded test servers
		},
	}
	return cfg
}

func startDevnet(t *testing.T, cfg *Config, timeout time.Duration) (*Devnet, context.Context) {
	t.Helper()
	requireDocker(t)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Stop(); err != nil {
			t.Logf("warning: stop failed: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	t.Log("starting devnet (this takes several minutes)...")
	if err := d.Start(ctx); err != nil {
		for i := 1; i <= cfg.Nodes; i++ {
			logs, _ := d.Logs(ctx, fmt.Sprintf("magi-%d", i))
			if logs != "" {
				t.Logf("magi-%d logs:\n%s", i, truncateLogs(logs, 30))
			}
		}
		t.Fatalf("starting devnet: %v", err)
	}
	t.Logf("devnet running: Hive=%s Mongo=%s", d.HiveRPCEndpoint(), d.MongoURI())

	// Insert a TSS key record to trigger keygen. On a real network this is
	// done by a contract calling tss.create_key; in tests we seed it directly.
	insertTssKey(t, d.MongoURI(), "test-key-main", "ecdsa")

	return d, ctx
}
