package tss_db

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// A23/18k regression for the TssKey finders.
//
// FindNewKeys, FindEpochKeys, FindDeprecatingKeys and FindNewlyRetired all
// drained their cursor the same unsafe way: the Decode error was discarded (so
// an undecodable row entered key scheduling as a ZERO-VALUE TssKey with an
// empty id), cursor.Err() was never checked (so a cursor dying mid-iteration
// returned a SHORT list with a nil error), and the cursor was never closed.
// FindNewKeys and FindEpochKeys additionally dropped the Find error with `_`.
//
// "No keys" is the dangerous default: it makes a node schedule no keygen or
// reshare at a rotate block where its peers schedule them, so one Mongo blip
// becomes a node that quietly stops doing its share of key management. All four
// now route through collectKeys, which reports instead of truncating.

func cursorOf(t *testing.T, docs []interface{}, err error) *mongo.Cursor {
	t.Helper()
	cur, cErr := mongo.NewCursorFromDocuments(docs, err, nil)
	require.NoError(t, cErr)
	return cur
}

func TestCollectKeys_CleanCursorReturnsEveryRow(t *testing.T) {
	docs := []interface{}{
		bson.D{{Key: "id", Value: "vsc1Vault-mainv2"}, {Key: "status", Value: "active"}, {Key: "epoch", Value: int64(4)}},
		bson.D{{Key: "id", Value: "vsc1Eth-mainv1"}, {Key: "status", Value: "active"}, {Key: "epoch", Value: int64(4)}},
	}

	keys, err := collectKeys(context.Background(), cursorOf(t, docs, nil), "test")
	require.NoError(t, err)
	require.Len(t, keys, 2)
	require.Equal(t, "vsc1Vault-mainv2", keys[0].Id)
	require.Equal(t, "vsc1Eth-mainv1", keys[1].Id)
}

// A cursor that dies after yielding some rows must NOT look like a short but
// successful read. This is the case that silently under-reported keys.
func TestCollectKeys_MidIterationFailureIsReported(t *testing.T) {
	docs := []interface{}{
		bson.D{{Key: "id", Value: "vsc1Vault-mainv2"}, {Key: "status", Value: "active"}},
	}
	boom := errors.New("incomplete read of message header")

	keys, err := collectKeys(context.Background(), cursorOf(t, docs, boom), "FindEpochKeys")
	require.Error(t, err, "a cursor failure must not be reported as a complete read")
	require.Nil(t, keys, "no partial key list may escape alongside the error")
	require.ErrorIs(t, err, boom, "the underlying cause must stay wrapped for classification upstream")
	require.Contains(t, err.Error(), "FindEpochKeys", "the error should name which finder failed")
}

// An empty result set is a legitimate answer and must stay distinguishable from
// a failure -- otherwise the fix would stall rotation on every quiet epoch.
func TestCollectKeys_EmptyIsNotAnError(t *testing.T) {
	keys, err := collectKeys(context.Background(), cursorOf(t, nil, nil), "test")
	require.NoError(t, err)
	require.Empty(t, keys)
}

// A row that cannot be decoded into a TssKey used to be appended as a
// zero-value key: an empty-id entry flowing into keygen/reshare scheduling.
func TestCollectKeys_UndecodableRowIsReportedNotZeroValued(t *testing.T) {
	docs := []interface{}{
		// `epoch` is a uint64 on TssKey; a string cannot decode into it.
		bson.D{{Key: "id", Value: "vsc1Vault-mainv2"}, {Key: "epoch", Value: "not-a-number"}},
	}

	keys, err := collectKeys(context.Background(), cursorOf(t, docs, nil), "FindNewKeys")
	require.Error(t, err, "an undecodable row must be reported, not appended as a zero-value key")
	require.Nil(t, keys)
	for _, k := range keys {
		require.NotEmpty(t, k.Id, "a zero-value key must never reach the caller")
	}
}
