package tss

import (
	"bytes"
	"context"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/require"
)

// allActiveKeys returns every key, as the real FindEpochKeys does for an epoch
// above all of them (the shared mock matches the epoch exactly).
type allActiveKeys struct {
	*test_utils.MockTssKeysDb
}

func (a allActiveKeys) FindEpochKeys(uint64) ([]tss_db.TssKey, error) {
	out := make([]tss_db.TssKey, 0, len(a.Keys))
	for _, k := range a.Keys {
		out = append(out, k)
	}
	return out, nil
}

// newestCommitment returns each key's newest keygen/reshare commitment, as the
// real GetCommitmentByHeight does (the shared mock matches the height exactly).
type newestCommitment struct {
	*test_utils.MockTssCommitmentsDb
	byKey map[string]tss_db.TssCommitment
}

func (n newestCommitment) GetCommitmentByHeight(keyId string, _ uint64, _ ...string) (tss_db.TssCommitment, error) {
	c, ok := n.byKey[keyId]
	if !ok {
		return tss_db.TssCommitment{}, context.Canceled // any error: no commitment
	}
	return c, nil
}

func heldSharesRig(t *testing.T) (*TssManager, newestCommitment) {
	t.Helper()
	members := []string{"magi.test1", "magi.test2", "magi.test3"}
	mgr := shareStoreManager(t, true, true)
	elec := makeElection(shareStoreEpoch, members)
	prev := makeElection(shareStoreEpoch-1, members)
	mgr.electionDb = &test_utils.MockElectionDb{
		Elections:         map[uint64]*elections.ElectionResult{shareStoreEpoch: elec, shareStoreEpoch - 1: prev},
		ElectionsByHeight: map[uint64]elections.ElectionResult{2600: *elec},
	}
	mgr.tssKeys = allActiveKeys{&test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}}
	nc := newestCommitment{&test_utils.MockTssCommitmentsDb{}, map[string]tss_db.TssCommitment{}}
	mgr.tssCommitments = nc
	return mgr, nc
}

// bitsetOf encodes member positions the way setToCommitment does.
func bitsetOf(idx ...int) string {
	bv := new(big.Int)
	for _, i := range idx {
		bv.SetBit(bv, i, 1)
	}
	return base64.RawURLEncoding.EncodeToString(bv.Bytes())
}

func TestHeldShares_ListedHolderWithoutItsShareIsFlagged(t *testing.T) {
	mgr, nc := heldSharesRig(t)
	keys := mgr.tssKeys.(allActiveKeys).Keys
	save := func(keyId string, epoch uint64, encKey []byte) {
		require.NoError(t, keystorePutEncrypted(context.Background(), mgr.keyStore, makeKey("key", keyId, int(epoch)), []byte(`{"share":1}`), encKey))
	}
	add := func(keyId string, epoch uint64, bits string) {
		keys[keyId] = tss_db.TssKey{Id: keyId, Status: "active", Epoch: epoch}
		nc.byKey[keyId] = tss_db.TssCommitment{KeyId: keyId, Type: "reshare", Epoch: epoch, Commitment: bits}
	}

	// magi.test1 is member 0 of both elections.
	add("held", shareStoreEpoch, bitsetOf(0, 1, 2))
	save("held", shareStoreEpoch, mgr.keystoreKey)

	add("lost", shareStoreEpoch, bitsetOf(0, 1, 2)) // listed, nothing on disk

	add("not-mine", shareStoreEpoch, bitsetOf(1, 2)) // not listed, nothing on disk

	// Listed in the newest reshare, but only the previous epoch's share is on disk
	// (the reshare was committed while this node failed to save).
	add("stale", shareStoreEpoch, bitsetOf(0, 1))
	save("stale", shareStoreEpoch-1, mgr.keystoreKey)

	// On disk, but not readable with this node's keystore key.
	add("unreadable", shareStoreEpoch, bitsetOf(0, 2))
	save("unreadable", shareStoreEpoch, bytes.Repeat([]byte{9}, 32))

	got := map[string]uint64{}
	for _, m := range mgr.heldSharesMissing(2600) {
		require.Error(t, m.Err)
		got[m.KeyId] = m.Epoch
	}
	require.Equal(t, map[string]uint64{
		"lost":       shareStoreEpoch,
		"stale":      shareStoreEpoch,
		"unreadable": shareStoreEpoch,
	}, got)

	// A node that cannot derive its keystore key cannot load any share it holds.
	noKey := shareStoreManager(t, true, false)
	noKey.electionDb, noKey.tssKeys, noKey.tssCommitments = mgr.electionDb, mgr.tssKeys, mgr.tssCommitments
	require.Len(t, noKey.heldSharesMissing(2600), 4, "held, lost, stale, unreadable")

	// A manager without a keystore (tests, tools) reports nothing and never panics.
	require.Empty(t, newTestTssManager(t, "magi.test1").heldSharesMissing(2600))
}

func TestHeldShares_ReportedAgainHourly(t *testing.T) {
	mgr := newTestTssManager(t, "magi.test1")
	now := time.Unix(1_800_000_000, 0)
	require.True(t, mgr.missingShareDue("k@3", now))
	require.False(t, mgr.missingShareDue("k@3", now.Add(59*time.Minute)))
	require.True(t, mgr.missingShareDue("k@4", now.Add(time.Minute)), "another epoch is its own report")
	require.True(t, mgr.missingShareDue("k@3", now.Add(time.Hour)))
}
