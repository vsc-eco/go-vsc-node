package tss_db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"
	"vsc-node/modules/db"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestTssRequests(t *testing.T) {
	mongoClient := connectDB(t)

	database := mongoClient.Database("vsc-test")
	defer database.Drop(t.Context())

	dbInstance := db.DbInstance{
		Database: database,
	}

	collection := db.NewCollection(&dbInstance, "tss")
	if err := collection.Init(); err != nil {
		t.Fatal(err)
	}

	const dataCount = 10
	const testTssKeyID = "test-tss-keyid"

	mockData := make([]TssRequest, dataCount)
	for i := range dataCount {
		msgHex := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, msgHex); err != nil {
			t.Fatal(err)
		}

		msgSig := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, msgSig); err != nil {
			t.Fatal(err)
		}

		mockData[i] = TssRequest{
			Id:     fmt.Sprintf("tss-id-%d", i),
			Status: TssSignStatus(fmt.Sprintf("tss-status-%d", i)),
			KeyId:  testTssKeyID,
			Msg:    hex.EncodeToString(msgHex),
			Sig:    hex.EncodeToString(msgSig),
		}
	}

	insertMockData(t, collection.Collection, mockData)
	tssDb := tssRequests{collection}

	hexQueries := make([]string, 2)
	hexQueries[0] = mockData[0].Msg
	hexQueries[1] = mockData[1].Msg

	result, err := tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:2], result)

	hexQueries[0] = mockData[0].Msg
	hexQueries[1] = mockData[dataCount-1].Msg

	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, []TssRequest{mockData[0], mockData[dataCount-1]}, result)

	hexQueries = []string{}
	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:], result)

	hexQueries = nil
	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:], result)

	hexQueries = []string{"foo"}
	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(result))
}

func insertMockData(
	t *testing.T,
	collection *mongo.Collection,
	mockData []TssRequest,
) {
	buf := make([]any, len(mockData))
	for i, b := range mockData {
		buf[i] = b
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, err := collection.InsertMany(ctx, buf)
	if err != nil {
		t.Fatal(err)
	}
}

func connectDB(t *testing.T) *mongo.Client {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	uri := "mongodb://127.0.0.1:27017"
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}

	return client
}

// newTestRequestsDb returns a tssRequests backed by a freshly created collection
// that is dropped when the test ends. Init() is called so the migration runs.
func newTestRequestsDb(t *testing.T) *tssRequests {
	t.Helper()
	client := connectDB(t)
	dbName := fmt.Sprintf("vsc-test-req-%d", time.Now().UnixNano())
	database := client.Database(dbName)
	t.Cleanup(func() { database.Drop(t.Context()) })

	instance := db.DbInstance{Database: database}
	reqs := &tssRequests{db.NewCollection(&instance, "tss_requests")}
	if err := reqs.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	return reqs
}

func insertRaw(t *testing.T, reqs *tssRequests, docs ...bson.M) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	buf := make([]any, len(docs))
	for i, d := range docs {
		buf[i] = d
	}
	if _, err := reqs.InsertMany(ctx, buf); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
}

func findOneTyped(t *testing.T, reqs *tssRequests, keyId, msg string) TssRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out TssRequest
	if err := reqs.FindOne(ctx, bson.M{"key_id": keyId, "msg": msg}).Decode(&out); err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	return out
}

// --- Init migration ---

func TestInit_MigratesDevelopSchema(t *testing.T) {
	client := connectDB(t)
	dbName := fmt.Sprintf("vsc-test-mig-dev-%d", time.Now().UnixNano())
	database := client.Database(dbName)
	t.Cleanup(func() { database.Drop(t.Context()) })

	instance := db.DbInstance{Database: database}
	coll := db.NewCollection(&instance, "tss_requests")
	if err := coll.Init(); err != nil {
		t.Fatalf("base Init: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := coll.InsertMany(ctx, []any{
		bson.M{"key_id": "k1", "msg": "dev-failed", "status": string(SignFailed), "sig": "old", "created_height": uint64(500)},
		bson.M{"key_id": "k1", "msg": "dev-pending", "status": string(SignPending), "sig": "", "created_height": uint64(600)},
		bson.M{"key_id": "k1", "msg": "dev-complete", "status": string(SignComplete), "sig": "sig", "created_height": uint64(700)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reqs := &tssRequests{coll}
	if err := reqs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	failed := findOneTyped(t, reqs, "k1", "dev-failed")
	assert.Equal(t, SignPending, failed.Status)
	assert.Equal(t, uint64(500), failed.LastAttempt)
	assert.Equal(t, uint(0), failed.AttemptCount)
	assert.Equal(t, "", failed.Sig)

	pending := findOneTyped(t, reqs, "k1", "dev-pending")
	assert.Equal(t, SignPending, pending.Status)
	assert.Equal(t, uint64(600), pending.LastAttempt)
	assert.Equal(t, uint(0), pending.AttemptCount)

	complete := findOneTyped(t, reqs, "k1", "dev-complete")
	assert.Equal(t, SignComplete, complete.Status)
	assert.Equal(t, "sig", complete.Sig)
}

// TestInit_DoesNotRevivePostMigrationFailedRows confirms the one-time
// revival only applies to pre-queue rows (missing last_attempt). A row
// failed under the queue schema — by MarkFailed for attempt-cap exhaustion
// or MarkFailedByKey for key deprecation — already has last_attempt set
// and must stay failed across restarts.
func TestInit_DoesNotRevivePostMigrationFailedRows(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "post-fail",
		"status":         string(SignFailed),
		"sig":            "",
		"created_height": uint64(500),
		"last_attempt":   uint64(600),
		"attempt_count":  uint(3),
	})

	if err := reqs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	doc := findOneTyped(t, reqs, "k1", "post-fail")
	assert.Equal(t, SignFailed, doc.Status, "queue-schema failed row must stay failed")
	assert.Equal(t, uint64(600), doc.LastAttempt, "last_attempt must be preserved")
	assert.Equal(t, uint(3), doc.AttemptCount, "attempt_count must be preserved")
}

func TestInit_Idempotent(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "aa",
		"status":         string(SignFailed),
		"sig":            "old",
		"created_height": uint64(500),
	})

	if err := reqs.Init(); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	first := findOneTyped(t, reqs, "k1", "aa")

	if err := reqs.Init(); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	second := findOneTyped(t, reqs, "k1", "aa")

	assert.Equal(t, first.Status, second.Status)
	assert.Equal(t, first.LastAttempt, second.LastAttempt)
	assert.Equal(t, first.AttemptCount, second.AttemptCount)
}

func TestFindUnsignedRequests_OrdersByLastAttempt(t *testing.T) {
	reqs := newTestRequestsDb(t)
	seed := []TssRequest{
		{KeyId: "k1", Msg: "old-stuck", Status: SignPending, CreatedHeight: 100, LastAttempt: 9500, AttemptCount: 3},
		{KeyId: "k1", Msg: "fresh-head", Status: SignPending, CreatedHeight: 9000, LastAttempt: 9000},
		{KeyId: "k1", Msg: "mid", Status: SignPending, CreatedHeight: 9500, LastAttempt: 9400},
	}
	for _, r := range seed {
		if err := reqs.SetSignedRequest(r); err != nil {
			t.Fatal(err)
		}
	}

	results, err := reqs.FindUnsignedRequests(10_000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3, got %d", len(results))
	}
	assert.Equal(t, "fresh-head", results[0].Msg)
	assert.Equal(t, "mid", results[1].Msg)
	assert.Equal(t, "old-stuck", results[2].Msg)
}

func TestFindUnsignedRequests_GatesOnLastAttempt(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "in-flight", Status: SignPending,
		CreatedHeight: 50, LastAttempt: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "ready", Status: SignPending,
		CreatedHeight: 50, LastAttempt: 50,
	}); err != nil {
		t.Fatal(err)
	}

	// bh < in-flight's LastAttempt: only "ready" returned.
	results, err := reqs.FindUnsignedRequests(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Msg != "ready" {
		t.Fatalf("want [ready], got %+v", results)
	}

	// bh past reservation: both eligible.
	results, err = reqs.FindUnsignedRequests(300, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
}

func TestFindUnsignedRequests_SurfacesCapped(t *testing.T) {
	// At-cap rows remain in the query result so the caller can mark them
	// failed at selection time. Enforcement is lazy, not filter-based.
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "capped",
		"status":         string(SignPending),
		"sig":            "",
		"created_height": uint64(100),
		"last_attempt":   uint64(100),
		"attempt_count":  MaxSignAttempts,
	})

	results, err := reqs.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Msg != "capped" {
		t.Fatalf("want [capped] returned for caller to mark failed, got %+v", results)
	}
	assert.Equal(t, MaxSignAttempts, results[0].AttemptCount)
}

func TestFindUnsignedRequests_RespectsLimit(t *testing.T) {
	reqs := newTestRequestsDb(t)
	for i := 0; i < 20; i++ {
		if err := reqs.SetSignedRequest(TssRequest{
			KeyId: "k1", Msg: fmt.Sprintf("m-%02d", i),
			Status: SignPending, CreatedHeight: uint64(100 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := reqs.FindUnsignedRequests(500, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 6 {
		t.Fatalf("want 6, got %d", len(results))
	}
	// Lowest LastAttempt (== CreatedHeight) first → m-00..m-05.
	for i, r := range results {
		assert.Equal(t, fmt.Sprintf("m-%02d", i), r.Msg)
	}
}

func TestFindUnsignedRequests_TiebreakChain(t *testing.T) {
	// Locks in the full (last_attempt, created_height, key_id, msg) sort
	// chain. Consensus-critical: every node must produce the same subset
	// under `limit`, so identical LastAttempt values must resolve to the
	// same order via the subsequent keys.
	reqs := newTestRequestsDb(t)
	// All rows share last_attempt=100. Differentiated by the later keys.
	seed := []TssRequest{
		// Same last_attempt and created_height: tiebreak falls to (key_id, msg).
		{KeyId: "kb", Msg: "a", Status: SignPending, CreatedHeight: 50, LastAttempt: 100},
		{KeyId: "ka", Msg: "b", Status: SignPending, CreatedHeight: 50, LastAttempt: 100},
		{KeyId: "ka", Msg: "a", Status: SignPending, CreatedHeight: 50, LastAttempt: 100},
		// Same last_attempt but distinct created_height: created_height wins before key_id.
		{KeyId: "kz", Msg: "z", Status: SignPending, CreatedHeight: 40, LastAttempt: 100},
	}
	for _, r := range seed {
		if err := reqs.SetSignedRequest(r); err != nil {
			t.Fatal(err)
		}
	}

	results, err := reqs.FindUnsignedRequests(200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("want 4, got %d", len(results))
	}

	// Expected order:
	// 1. created_height=40 (kz/z) — smallest created_height wins after last_attempt tie.
	// 2. ka/a (key_id ka < kb, msg a < b)
	// 3. ka/b
	// 4. kb/a
	assert.Equal(t, "kz", results[0].KeyId, "smallest created_height wins after last_attempt tie")
	assert.Equal(t, "z", results[0].Msg)

	assert.Equal(t, "ka", results[1].KeyId)
	assert.Equal(t, "a", results[1].Msg)

	assert.Equal(t, "ka", results[2].KeyId)
	assert.Equal(t, "b", results[2].Msg)

	assert.Equal(t, "kb", results[3].KeyId)
	assert.Equal(t, "a", results[3].Msg)
}

func TestReserveAttempt_AdvancesLastAttemptAndCount(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "aa", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}

	if err := reqs.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "aa")
	assert.Equal(t, uint64(500+SignAttemptReservation), doc.LastAttempt)
	assert.Equal(t, uint(1), doc.AttemptCount)
}

func TestReserveAttempt_IdempotentWithinReservation(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "aa", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// First reservation at bh=500.
	if err := reqs.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}
	// Re-issuing the same block's reservation must not double-count.
	if err := reqs.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}
	// Even at bh within the reservation window: no-op.
	if err := reqs.ReserveAttempt("k1", "aa", 500+SignAttemptReservation-1); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "aa")
	assert.Equal(t, uint64(500+SignAttemptReservation), doc.LastAttempt)
	assert.Equal(t, uint(1), doc.AttemptCount)
}

func TestReserveAttempt_AdvancesAfterReservationElapses(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "aa", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}
	// bh now past the first reservation.
	if err := reqs.ReserveAttempt("k1", "aa", 500+SignAttemptReservation); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "aa")
	assert.Equal(t, uint64(500+2*SignAttemptReservation), doc.LastAttempt)
	assert.Equal(t, uint(2), doc.AttemptCount)
}

func TestReserveAttempt_NoOpOnNonPending(t *testing.T) {
	// Defensive guard: if a row has transitioned to failed or complete,
	// ReserveAttempt must not touch it. Protects the failed→pending reset
	// path in SetSignedRequest from stale bumps by any future caller that
	// bypasses the BlockTick selection flow.
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "failed",
		"status":         string(SignFailed),
		"sig":            "",
		"created_height": uint64(100),
		"last_attempt":   uint64(100),
		"attempt_count":  uint(3),
	})
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "complete",
		"status":         string(SignComplete),
		"sig":            "sig",
		"created_height": uint64(100),
		"last_attempt":   uint64(100),
		"attempt_count":  uint(1),
	})

	if err := reqs.ReserveAttempt("k1", "failed", 500); err != nil {
		t.Fatal(err)
	}
	if err := reqs.ReserveAttempt("k1", "complete", 500); err != nil {
		t.Fatal(err)
	}

	failed := findOneTyped(t, reqs, "k1", "failed")
	assert.Equal(t, uint64(100), failed.LastAttempt, "failed row must not be bumped")
	assert.Equal(t, uint(3), failed.AttemptCount, "failed row AttemptCount must not increment")

	complete := findOneTyped(t, reqs, "k1", "complete")
	assert.Equal(t, uint64(100), complete.LastAttempt, "complete row must not be bumped")
	assert.Equal(t, uint(1), complete.AttemptCount, "complete row AttemptCount must not increment")
}

func TestMarkFailed_TransitionsPendingRow(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "to-fail",
		"status": string(SignPending), "sig": "",
		"created_height": uint64(100),
		"last_attempt":   uint64(100),
		"attempt_count":  MaxSignAttempts,
	})

	if err := reqs.MarkFailed("k1", "to-fail"); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "to-fail")
	assert.Equal(t, SignFailed, doc.Status)
}

func TestMarkFailed_NoOpOnComplete(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "done",
		"status": string(SignComplete), "sig": "sig",
		"created_height": uint64(100),
		"last_attempt":   uint64(100),
		"attempt_count":  uint(2),
	})

	if err := reqs.MarkFailed("k1", "done"); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "done")
	assert.Equal(t, SignComplete, doc.Status)
	assert.Equal(t, "sig", doc.Sig)
}
