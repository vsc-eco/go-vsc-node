package tss

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"

	btss "github.com/bnb-chain/tss-lib/v3/tss"
	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/stretchr/testify/require"
)

// A node may report keygen/reshare success, and so BLS-sign the commitment that
// names it a holder of the key, only once its share is saved. A failed keystore
// write used to be logged while the node still reported success: the commitment
// could name holders that have no share (the key has less margin than its record
// shows), and a reshare most new holders failed to save could still be committed
// over the old epoch. THORChain fails the keygen on a failed save (go-tss
// keygen/ecdsa/tss_keygen.go:239).

const shareStoreEpoch = 3

func shareStoreManager(t *testing.T, writable, encKeyOK bool) *TssManager {
	t.Helper()
	mgr := newTestTssManager(t, "magi.test1")
	mgr.electionDb = &test_utils.MockElectionDb{Elections: map[uint64]*elections.ElectionResult{
		shareStoreEpoch: makeElection(shareStoreEpoch, []string{"magi.test1", "magi.test2", "magi.test3"}),
	}}
	dir := t.TempDir()
	ds, err := flatfs.CreateOrOpen(dir, flatfs.Prefix(1), false)
	require.NoError(t, err)
	t.Cleanup(func() { ds.Close() })
	if !writable {
		// The disk refuses the write (full, read-only, permissions).
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { os.Chmod(dir, 0o700) })
	}
	mgr.keyStore = ds
	mgr.keystoreKeyOnce.Do(func() {
		if encKeyOK {
			mgr.keystoreKey = bytes.Repeat([]byte{7}, 32)
		} else {
			mgr.keystoreKeyErr = errors.New("BLS seed unavailable")
		}
	})
	return mgr
}

func shareStoreBase(t *testing.T, mgr *TssManager, sessionId string) BaseDispatcher {
	_, cancel := context.WithCancel(context.Background())
	p2p := make(chan btss.Message)
	t.Cleanup(func() { close(p2p) })
	return BaseDispatcher{
		tssMgr: mgr,
		participants: []Participant{
			{Account: "magi.test1"}, {Account: "magi.test2"}, {Account: "magi.test3"},
		},
		sessionId:   sessionId,
		keyId:       "vsc1Vault-mainv2",
		epoch:       shareStoreEpoch,
		blockHeight: 2600,
		done:        make(chan struct{}, 1),
		cancelMsgs:  cancel,
		p2pMsg:      p2p,
		keystore:    mgr.keyStore,
	}
}

// The session also saw a late protocol error naming a culprit. A node that kept
// no share must not emit that blame either: no other node resolves it.
func withLateTssError(d *BaseDispatcher) {
	d.recordTssError(btss.NewError(errors.New("late message"), "test", 1, nil, btss.NewPartyID("magi.test2", "", big.NewInt(2))))
}

func TestKeygen_ShareMustBeSavedBeforeSuccess(t *testing.T) {
	share := []byte(`{"share":"node one"}`)
	pub := []byte{0x02, 0x01}
	run := func(t *testing.T, mgr *TssManager, lateErr bool) (DispatcherResult, error) {
		d := &KeyGenDispatcher{BaseDispatcher: shareStoreBase(t, mgr, "keygen-2600-0-vsc1Vault-mainv2")}
		if lateErr {
			withLateTssError(&d.BaseDispatcher)
		}
		d.storeKeygenResult(pub, share)
		res, err := d.Done().Await(context.Background())
		if err != nil {
			return nil, err
		}
		return *res, nil
	}

	t.Run("saved: success, and the share reads back", func(t *testing.T) {
		mgr := shareStoreManager(t, true, true)
		res, err := run(t, mgr, false)
		require.NoError(t, err)
		require.IsType(t, KeyGenResult{}, res)
		got, err := keystoreGetDecrypted(context.Background(), mgr.keyStore, makeKey("key", "vsc1Vault-mainv2", shareStoreEpoch), mgr.keystoreKey)
		require.NoError(t, err)
		require.Equal(t, share, got)
	})
	for _, tc := range []struct {
		name               string
		writable, encKeyOK bool
		lateErr            bool
	}{
		{"keystore write fails", false, true, false},
		{"keystore encryption key unavailable", true, false, false},
		{"write fails and the session saw a late TSS error", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := run(t, shareStoreManager(t, tc.writable, tc.encKeyOK), tc.lateErr)
			require.Error(t, err, "a node with no saved share resolved %T and would sign it", res)
		})
	}
}

func TestReshare_ShareMustBeSavedBeforeSuccess(t *testing.T) {
	share := []byte(`{"share":"node one, new epoch"}`)
	run := func(t *testing.T, mgr *TssManager, lateErr bool) (DispatcherResult, error) {
		d := &ReshareDispatcher{
			BaseDispatcher:  shareStoreBase(t, mgr, "reshare-2600-0-vsc1Vault-mainv2"),
			newParticipants: []Participant{{Account: "magi.test1"}, {Account: "magi.test2"}, {Account: "magi.test3"}},
			newEpoch:        shareStoreEpoch,
		}
		d.epoch = shareStoreEpoch - 1
		if lateErr {
			withLateTssError(&d.BaseDispatcher)
		}
		d.storeReshareResult("ECDSA", share)
		res, err := d.Done().Await(context.Background())
		if err != nil {
			return nil, err
		}
		return *res, nil
	}

	t.Run("saved: success, and the share reads back", func(t *testing.T) {
		mgr := shareStoreManager(t, true, true)
		res, err := run(t, mgr, false)
		require.NoError(t, err)
		require.IsType(t, ReshareResult{}, res)
		got, err := keystoreGetDecrypted(context.Background(), mgr.keyStore, makeKey("key", "vsc1Vault-mainv2", shareStoreEpoch), mgr.keystoreKey)
		require.NoError(t, err)
		require.Equal(t, share, got)
	})
	for _, tc := range []struct {
		name               string
		writable, encKeyOK bool
		lateErr            bool
	}{
		{"keystore write fails", false, true, false},
		{"keystore encryption key unavailable", true, false, false},
		{"write fails and the session saw a late TSS error", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := run(t, shareStoreManager(t, tc.writable, tc.encKeyOK), tc.lateErr)
			require.Error(t, err, "a node with no saved share resolved %T and would sign it", res)
		})
	}
}
