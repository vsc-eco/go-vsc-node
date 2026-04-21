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

func TestInit_MigratesFailedToUnsigned(t *testing.T) {
	// Seed a failed request as if written by the pre-backoff code: no
	// attempt_count, no next_attempt_height, status=failed.
	client := connectDB(t)
	dbName := fmt.Sprintf("vsc-test-mig-%d", time.Now().UnixNano())
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
		bson.M{"key_id": "k1", "msg": "aa", "status": string(SignFailed), "sig": "oldsig", "created_height": uint64(500)},
		bson.M{"key_id": "k1", "msg": "bb", "status": string(SignFailed), "sig": "", "created_height": uint64(600)},
		bson.M{"key_id": "k1", "msg": "cc", "status": string(SignPending), "sig": "", "created_height": uint64(700)},
		bson.M{"key_id": "k1", "msg": "dd", "status": string(SignComplete), "sig": "gotit", "created_height": uint64(800)},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	reqs := &tssRequests{coll}
	if err := reqs.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// All failed → unsigned, attempt_count=0, next_attempt_height=created_height, sig cleared
	for _, msg := range []string{"aa", "bb"} {
		doc := findOneTyped(t, reqs, "k1", msg)
		assert.Equal(t, SignPending, doc.Status, "msg=%s", msg)
		assert.Equal(t, uint(0), doc.AttemptCount, "msg=%s", msg)
		assert.Equal(t, doc.CreatedHeight, doc.NextAttemptHeight, "msg=%s", msg)
		assert.Equal(t, "", doc.Sig, "msg=%s", msg)
	}

	// Pre-existing unsigned gets next_attempt_height backfilled to created_height.
	doc := findOneTyped(t, reqs, "k1", "cc")
	assert.Equal(t, SignPending, doc.Status)
	assert.Equal(t, uint(0), doc.AttemptCount)
	assert.Equal(t, doc.CreatedHeight, doc.NextAttemptHeight)

	// Complete is untouched.
	doc = findOneTyped(t, reqs, "k1", "dd")
	assert.Equal(t, SignComplete, doc.Status)
	assert.Equal(t, "gotit", doc.Sig)
}

func TestInit_Idempotent(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "aa",
		"status": string(SignFailed), "sig": "old",
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
	assert.Equal(t, first.NextAttemptHeight, second.NextAttemptHeight)
	assert.Equal(t, first.AttemptCount, second.AttemptCount)
}

// --- FindUnsignedRequests ordering ---

func TestFindUnsignedRequests_OrdersByNextAttemptHeight(t *testing.T) {
	reqs := newTestRequestsDb(t)
	// Older-created request with a larger next_attempt_height should come last.
	now := uint64(10_000)
	seed := []TssRequest{
		{KeyId: "k1", Msg: "old-but-backed-off", Status: SignPending, CreatedHeight: 100, NextAttemptHeight: 9_500, AttemptCount: 3},
		{KeyId: "k1", Msg: "fresh-low-next", Status: SignPending, CreatedHeight: 9_000, NextAttemptHeight: 9_000},
		{KeyId: "k1", Msg: "fresh-mid", Status: SignPending, CreatedHeight: 9_500, NextAttemptHeight: 9_400},
	}
	for _, r := range seed {
		if err := reqs.SetSignedRequest(r); err != nil {
			t.Fatal(err)
		}
	}

	results, err := reqs.FindUnsignedRequests(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	assert.Equal(t, "fresh-low-next", results[0].Msg)
	assert.Equal(t, "fresh-mid", results[1].Msg)
	assert.Equal(t, "old-but-backed-off", results[2].Msg)
}

func TestFindUnsignedRequests_GatesOnNextAttemptHeight(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "future", Status: SignPending,
		CreatedHeight: 50, NextAttemptHeight: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "due", Status: SignPending,
		CreatedHeight: 50, NextAttemptHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}

	results, err := reqs.FindUnsignedRequests(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d: %+v", len(results), results)
	}
	assert.Equal(t, "due", results[0].Msg)
}

func TestFindUnsignedRequests_LimitAndStatusFilter(t *testing.T) {
	reqs := newTestRequestsDb(t)
	statuses := []TssSignStatus{SignPending, SignPending, SignPending, SignComplete, SignFailed}
	msgs := []string{"a", "b", "c", "d", "e"}
	for i, s := range statuses {
		if err := reqs.SetSignedRequest(TssRequest{
			KeyId: "k1", Msg: msgs[i], Status: s,
			CreatedHeight: uint64(100 + i), NextAttemptHeight: uint64(100 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := reqs.FindUnsignedRequests(1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results (limit), got %d", len(results))
	}
	// Only unsigned docs can appear.
	for _, r := range results {
		assert.Equal(t, SignPending, r.Status)
	}
}

// --- SetSignedRequest resubmission ---

func TestSetSignedRequest_InsertsNewUnsigned(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "new", CreatedHeight: 500,
	}); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "new")
	assert.Equal(t, SignPending, doc.Status)
	// NextAttemptHeight defaults to CreatedHeight for fresh inserts.
	assert.Equal(t, uint64(500), doc.NextAttemptHeight)
}

func TestSetSignedRequest_NoOpOnExistingUnsigned(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "dup", CreatedHeight: 500,
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a prior dispatch that bumped backoff.
	if err := reqs.BumpAttempt("k1", "dup", 3, 1_200); err != nil {
		t.Fatal(err)
	}

	// Resubmit at a later height.
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "dup", CreatedHeight: 700,
	}); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "dup")
	// Must still be the bumped state, not reset.
	assert.Equal(t, uint(3), doc.AttemptCount)
	assert.Equal(t, uint64(1_200), doc.NextAttemptHeight)
	assert.Equal(t, uint64(500), doc.CreatedHeight)
}

func TestSetSignedRequest_NoOpOnComplete(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "done",
		"status":              string(SignComplete),
		"sig":                 "signedsig",
		"created_height":      uint64(500),
		"attempt_count":       0,
		"next_attempt_height": uint64(500),
	})

	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "done", CreatedHeight: 900,
	}); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "done")
	assert.Equal(t, SignComplete, doc.Status)
	assert.Equal(t, "signedsig", doc.Sig)
}

func TestSetSignedRequest_ResetsFailedToUnsigned(t *testing.T) {
	reqs := newTestRequestsDb(t)
	insertRaw(t, reqs, bson.M{
		"key_id": "k1", "msg": "retryme",
		"status":              string(SignFailed),
		"sig":                 "stale",
		"created_height":      uint64(500),
		"attempt_count":       5,
		"next_attempt_height": uint64(1_200),
	})

	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "retryme", CreatedHeight: 9_000,
	}); err != nil {
		t.Fatal(err)
	}

	doc := findOneTyped(t, reqs, "k1", "retryme")
	assert.Equal(t, SignPending, doc.Status)
	assert.Equal(t, uint(0), doc.AttemptCount)
	assert.Equal(t, uint64(9_000), doc.NextAttemptHeight)
	assert.Equal(t, uint64(9_000), doc.CreatedHeight)
	assert.Equal(t, "", doc.Sig)
}

// --- BumpAttempt ---

func TestBumpAttempt_UpdatesTargetedRequest(t *testing.T) {
	reqs := newTestRequestsDb(t)
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k1", Msg: "aa", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "k2", Msg: "aa", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}

	if err := reqs.BumpAttempt("k1", "aa", 2, 400); err != nil {
		t.Fatal(err)
	}

	target := findOneTyped(t, reqs, "k1", "aa")
	assert.Equal(t, uint(2), target.AttemptCount)
	assert.Equal(t, uint64(400), target.NextAttemptHeight)

	// Different key, same msg is untouched.
	other := findOneTyped(t, reqs, "k2", "aa")
	assert.Equal(t, uint(0), other.AttemptCount)
	assert.Equal(t, uint64(100), other.NextAttemptHeight)
}

// --- MarkFailedByKey ---

func TestMarkFailedByKey_OnlyUnsignedForMatchingKey(t *testing.T) {
	reqs := newTestRequestsDb(t)

	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "targetKey", Msg: "m1", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reqs.SetSignedRequest(TssRequest{
		KeyId: "otherKey", Msg: "m2", CreatedHeight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	insertRaw(t, reqs, bson.M{
		"key_id": "targetKey", "msg": "done",
		"status":              string(SignComplete),
		"sig":                 "existingsig",
		"created_height":      uint64(90),
		"attempt_count":       0,
		"next_attempt_height": uint64(90),
	})

	if err := reqs.MarkFailedByKey("targetKey"); err != nil {
		t.Fatal(err)
	}

	// Unsigned targetKey → failed.
	failed := findOneTyped(t, reqs, "targetKey", "m1")
	assert.Equal(t, SignFailed, failed.Status)

	// Completed targetKey → still complete, sig intact.
	still := findOneTyped(t, reqs, "targetKey", "done")
	assert.Equal(t, SignComplete, still.Status)
	assert.Equal(t, "existingsig", still.Sig)

	// Other key untouched.
	other := findOneTyped(t, reqs, "otherKey", "m2")
	assert.Equal(t, SignPending, other.Status)
}
