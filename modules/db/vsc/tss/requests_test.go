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
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestTssRequests(t *testing.T) {
	// connect to database
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

	// mock data
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

	// test queries
	// first 2 msgHex
	hexQueries := make([]string, 2)
	hexQueries[0] = mockData[0].Msg
	hexQueries[1] = mockData[1].Msg

	result, err := tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:2], result)

	// first and last hex
	hexQueries[0] = mockData[0].Msg
	hexQueries[1] = mockData[dataCount-1].Msg

	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, []TssRequest{mockData[0], mockData[dataCount-1]}, result)

	// all tss
	hexQueries = []string{}
	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:], result)

	hexQueries = nil
	result, err = tssDb.FindRequests(testTssKeyID, hexQueries)
	assert.NoError(t, err)
	assert.Equal(t, mockData[:], result)

	// non matching
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
