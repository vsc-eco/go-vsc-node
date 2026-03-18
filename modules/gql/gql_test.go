package gql_test

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/gql/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTssKeyLifecycleResolver(t *testing.T) {
	keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}
	require.NoError(t, keys.InsertKey("key-1", tss_db.EcdsaType, 12))
	require.NoError(t, keys.SetKey(tss_db.TssKey{
		Id:               "key-1",
		Status:           tss_db.TssKeyDeprecated,
		PublicKey:        "02deadbeef",
		CreatedHeight:    123,
		Epoch:            5,
		Epochs:           12,
		ExpiryEpoch:      17,
		DeprecatedHeight: 456,
	}))

	resolver := &gqlgen.Resolver{
		TssKeys: keys,
	}

	ctx := context.Background()
	key, err := resolver.Query().GetTssKey(ctx, "key-1")
	require.NoError(t, err)
	require.NotNil(t, key)

	createdHeight, err := resolver.TssKey().CreatedHeight(ctx, key)
	require.NoError(t, err)
	epoch, err := resolver.TssKey().Epoch(ctx, key)
	require.NoError(t, err)
	epochs, err := resolver.TssKey().Epochs(ctx, key)
	require.NoError(t, err)
	expiryEpoch, err := resolver.TssKey().ExpiryEpoch(ctx, key)
	require.NoError(t, err)
	deprecatedHeight, err := resolver.TssKey().DeprecatedHeight(ctx, key)
	require.NoError(t, err)

	assert.Equal(t, "key-1", key.Id)
	assert.Equal(t, tss_db.TssKeyDeprecated, key.Status)
	assert.Equal(t, "02deadbeef", key.PublicKey)
	assert.Equal(t, model.Int64(123), createdHeight)
	assert.Equal(t, model.Uint64(5), epoch)
	assert.Equal(t, model.Uint64(12), epochs)
	assert.Equal(t, model.Uint64(17), expiryEpoch)
	assert.Equal(t, model.Int64(456), deprecatedHeight)
}

func TestTssCommitmentsResolver(t *testing.T) {
	pubKey := "02cafebabe"
	commitments := &test_utils.MockTssCommitmentsDb{
		Commitments: map[string]tss_db.TssCommitment{
			"key-1:5:keygen": {
				Type:        "keygen",
				BlockHeight: 100,
				Epoch:       5,
				Commitment:  "bitset-a",
				KeyId:       "key-1",
				TxId:        "tx-a",
				PublicKey:   &pubKey,
			},
			"key-1:6:reshare": {
				Type:        "reshare",
				BlockHeight: 120,
				Epoch:       6,
				Commitment:  "bitset-b",
				KeyId:       "key-1",
				TxId:        "tx-b",
			},
		},
	}

	resolver := &gqlgen.Resolver{
		TssCommitments: commitments,
	}

	ctx := context.Background()
	all, err := resolver.Query().GetTssCommitments(ctx, "key-1", nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "reshare", all[0].Type)
	assert.Equal(t, uint64(120), all[0].BlockHeight)
	assert.Equal(t, "keygen", all[1].Type)
	assert.Equal(t, uint64(100), all[1].BlockHeight)

	blockHeight, err := resolver.TssCommitment().BlockHeight(ctx, &all[0])
	require.NoError(t, err)
	epoch, err := resolver.TssCommitment().Epoch(ctx, &all[0])
	require.NoError(t, err)
	assert.Equal(t, model.Uint64(120), blockHeight)
	assert.Equal(t, model.Uint64(6), epoch)

	filterEpoch := model.Uint64(6)
	filterFromBlock := model.Uint64(100)
	filtered, err := resolver.Query().GetTssCommitments(ctx, "key-1", []string{"reshare"}, &filterEpoch, &filterFromBlock)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, "reshare", filtered[0].Type)
	assert.Equal(t, "tx-b", filtered[0].TxId)
}

func TestLatestTssCommitmentResolver(t *testing.T) {
	pubKey := "02cafebabe"
	commitments := &test_utils.MockTssCommitmentsDb{
		Commitments: map[string]tss_db.TssCommitment{
			"key-1:5:keygen": {
				Type:        "keygen",
				BlockHeight: 100,
				Epoch:       5,
				Commitment:  "bitset-a",
				KeyId:       "key-1",
				TxId:        "tx-a",
				PublicKey:   &pubKey,
			},
			"key-1:6:timeout": {
				Type:        "timeout",
				BlockHeight: 130,
				Epoch:       6,
				Commitment:  "timeout-a",
				KeyId:       "key-1",
				TxId:        "tx-timeout",
			},
			"key-1:7:reshare": {
				Type:        "reshare",
				BlockHeight: 140,
				Epoch:       7,
				Commitment:  "bitset-b",
				KeyId:       "key-1",
				TxId:        "tx-b",
			},
		},
	}

	resolver := &gqlgen.Resolver{TssCommitments: commitments}
	ctx := context.Background()

	latest, err := resolver.Query().GetLatestTssCommitment(ctx, "key-1", nil)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, "reshare", latest.Type)
	assert.Equal(t, uint64(140), latest.BlockHeight)
	assert.Equal(t, "tx-b", latest.TxId)

	timeoutType := "timeout"
	latestTimeout, err := resolver.Query().GetLatestTssCommitment(ctx, "key-1", &timeoutType)
	require.NoError(t, err)
	require.NotNil(t, latestTimeout)
	assert.Equal(t, "timeout", latestTimeout.Type)
	assert.Equal(t, uint64(130), latestTimeout.BlockHeight)
	assert.Equal(t, "tx-timeout", latestTimeout.TxId)
}

func TestRecentBlameTimeoutCommitmentsResolver(t *testing.T) {
	commitments := &test_utils.MockTssCommitmentsDb{
		Commitments: map[string]tss_db.TssCommitment{
			"key-1:4:keygen": {
				Type:        "keygen",
				BlockHeight: 90,
				Epoch:       4,
				Commitment:  "bitset-a",
				KeyId:       "key-1",
				TxId:        "tx-keygen",
			},
			"key-1:5:blame": {
				Type:        "blame",
				BlockHeight: 110,
				Epoch:       5,
				Commitment:  "blame-a",
				KeyId:       "key-1",
				TxId:        "tx-blame",
			},
			"key-2:6:timeout": {
				Type:        "timeout",
				BlockHeight: 125,
				Epoch:       6,
				Commitment:  "timeout-a",
				KeyId:       "key-2",
				TxId:        "tx-timeout",
			},
			"key-3:7:reshare": {
				Type:        "reshare",
				BlockHeight: 140,
				Epoch:       7,
				Commitment:  "bitset-b",
				KeyId:       "key-3",
				TxId:        "tx-reshare",
			},
		},
	}

	resolver := &gqlgen.Resolver{TssCommitments: commitments}
	ctx := context.Background()
	fromBlock := model.Uint64(100)

	recent, err := resolver.Query().GetRecentTssCommitments(ctx, []string{"blame", "timeout"}, &fromBlock)
	require.NoError(t, err)
	require.Len(t, recent, 2)

	assert.Equal(t, "timeout", recent[0].Type)
	assert.Equal(t, "key-2", recent[0].KeyId)
	assert.Equal(t, uint64(125), recent[0].BlockHeight)

	assert.Equal(t, "blame", recent[1].Type)
	assert.Equal(t, "key-1", recent[1].KeyId)
	assert.Equal(t, uint64(110), recent[1].BlockHeight)
}
