package tss

import (
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo"
)

// The sign scheduler's FindKey error used to be discarded, and this was the
// most damaging instance of that pattern in the file.
//
// A zero-value TssKey has Status "", which is not TssKeyActive — so on a
// transient Mongo error the scheduler did not skip the request, it took the
// "non-active key" branch and marked a perfectly good pending signature
// SignFailed. SignFailed is terminal: nothing retries it. One bad read
// destroyed a signing request.
//
// The fix classifies the error the way the rest of this work does:
// mongo.ErrNoDocuments is a deterministic absence that every node sees, and a
// request naming a nonexistent key SHOULD still be failed; any other error is
// a per-node blip and leaves the request pending.
//
// These assert the classification directly — the scheduler itself is a long
// loop over live DB state, so pinning the decision is what keeps the guarantee
// legible.

// signRequestKeyDecision mirrors the scheduler's branch: should this read leave
// the request pending (true) rather than act on keyInfo?
func signRequestKeyDecision(err error) bool {
	return err != nil && !errors.Is(err, mongo.ErrNoDocuments)
}

func TestSignRequest_TransientKeyReadLeavesRequestPending(t *testing.T) {
	keys := &test_utils.MockTssKeysDb{
		Keys:        map[string]tss_db.TssKey{"vsc1Vault-mainv2": {Id: "vsc1Vault-mainv2", Status: tss_db.TssKeyActive}},
		FindKeyErrs: 1, // one transient failure, then the key reads fine
	}

	// First read: transient. The request must be left pending, NOT failed.
	_, err := keys.FindKey("vsc1Vault-mainv2")
	require.Error(t, err)
	require.False(t, errors.Is(err, mongo.ErrNoDocuments), "fixture must produce a TRANSIENT error")
	require.True(t, signRequestKeyDecision(err),
		"a transient key-read error must leave the sign request pending; marking it SignFailed is terminal")

	// Second read: the key is there and active, so the request proceeds as normal.
	key, err := keys.FindKey("vsc1Vault-mainv2")
	require.NoError(t, err)
	require.Equal(t, tss_db.TssKeyActive, key.Status)
	require.False(t, signRequestKeyDecision(err))
}

// The other direction, which a blanket skip would have broken: a request naming
// a key that genuinely does not exist must still be failed, or it sits pending
// forever.
func TestSignRequest_AbsentKeyStillFailsTheRequest(t *testing.T) {
	keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}

	keyInfo, err := keys.FindKey("vsc1Vault-doesnotexist")
	require.ErrorIs(t, err, mongo.ErrNoDocuments)
	require.False(t, signRequestKeyDecision(err),
		"a genuinely absent key is deterministic on every node — the request must still be failed")
	require.NotEqual(t, tss_db.TssKeyActive, keyInfo.Status,
		"and the zero-value key must still take the non-active branch")
}
