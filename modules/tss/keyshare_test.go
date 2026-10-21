package tss

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"

	"vsc-node/lib/test_utils"
	tss_db "vsc-node/modules/db/vsc/tss"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/bnb-chain/tss-lib/v3/crypto"
	keyGenSecp256k1 "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
	keyGenEddsa "github.com/bnb-chain/tss-lib/v3/eddsa/keygen"
	btss "github.com/bnb-chain/tss-lib/v3/tss"
	"github.com/stretchr/testify/require"
)

// TSS-KG-1 (0.12.0): same-epoch keygens write one share slot, so the slot can
// hold another keygen's share than the one the chain activated, and the node
// then signs with a key the chain does not hold. A share loaded for a
// keygen-born epoch must be the key's on-chain public key's; the activating
// keygen's copy, kept per public key, replaces it.

// testShare is a share whose public key is k*G on algo's curve.
func testShare(t *testing.T, algo tss_helpers.SigningAlgo, k int64) ([]byte, string) {
	t.Helper()
	var raw []byte
	var err error
	switch algo {
	case tss_helpers.SigningAlgoEcdsa:
		x, y := btss.S256().ScalarBaseMult(big.NewInt(k).Bytes())
		p, perr := crypto.NewECPoint(btss.S256(), x, y)
		require.NoError(t, perr)
		raw, err = json.Marshal(keyGenSecp256k1.LocalPartySaveData{ECDSAPub: p})
	case tss_helpers.SigningAlgoEddsa:
		x, y := btss.Edwards().ScalarBaseMult(big.NewInt(k).Bytes())
		p, perr := crypto.NewECPoint(btss.Edwards(), x, y)
		require.NoError(t, perr)
		raw, err = json.Marshal(keyGenEddsa.LocalPartySaveData{EDDSAPub: p})
	}
	require.NoError(t, err)
	pub, err := sharePubHex(algo, raw)
	require.NoError(t, err)
	return raw, pub
}

func keyshareBase(t *testing.T, algo tss_helpers.SigningAlgo, chainPub string) (*TssManager, *BaseDispatcher) {
	t.Helper()
	mgr := shareStoreManager(t, true, true)
	mgr.tssKeys = &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{
		"vsc1Vault-mainv2": {Id: "vsc1Vault-mainv2", Status: tss_db.TssKeyActive, PublicKey: chainPub, Epoch: shareStoreEpoch},
	}}
	d := shareStoreBase(t, mgr, "sign-2600-0-vsc1Vault-mainv2")
	d.algo = algo
	return mgr, &d
}

func putShare(t *testing.T, mgr *TssManager, key string, raw []byte, pub string) {
	t.Helper()
	k := makeKey("key", "vsc1Vault-mainv2", shareStoreEpoch)
	if key == "copy" {
		k = keygenShareKey(pub)
	}
	require.NoError(t, keystorePutEncrypted(context.Background(), mgr.keyStore, k, raw, mgr.keystoreKey))
}

func slotPub(t *testing.T, mgr *TssManager, algo tss_helpers.SigningAlgo) string {
	t.Helper()
	raw, err := keystoreGetDecrypted(context.Background(), mgr.keyStore, makeKey("key", "vsc1Vault-mainv2", shareStoreEpoch), mgr.keystoreKey)
	require.NoError(t, err)
	pub, err := sharePubHex(algo, raw)
	require.NoError(t, err)
	return pub
}

func TestTSSKG1_ShareOfTheActivatingKeygenIsUsed(t *testing.T) {
	for _, algo := range []tss_helpers.SigningAlgo{tss_helpers.SigningAlgoEcdsa, tss_helpers.SigningAlgoEddsa} {
		activating, pubA := testShare(t, algo, 7)
		later, pubL := testShare(t, algo, 11)

		// The slot holds the later keygen's share; the chain activated the first.
		mgr, d := keyshareBase(t, algo, pubA)
		putShare(t, mgr, "slot", later, pubL)
		putShare(t, mgr, "copy", activating, pubA)
		got, err := d.loadKeyShare(shareStoreEpoch, true)
		require.NoError(t, err)
		require.Equal(t, activating, got, "%s: the activating keygen's share is used", algo)
		require.Equal(t, pubA, slotPub(t, mgr, algo), "%s: the slot is repaired for every other reader", algo)

		// The slot already matches: untouched.
		mgr, d = keyshareBase(t, algo, pubA)
		putShare(t, mgr, "slot", activating, pubA)
		putShare(t, mgr, "copy", later, pubA) // a bogus copy must not be read
		got, err = d.loadKeyShare(shareStoreEpoch, true)
		require.NoError(t, err)
		require.Equal(t, activating, got)

		// Not checked (below 0.12.0, or a reshare-born epoch): the slot as before.
		mgr, d = keyshareBase(t, algo, pubA)
		putShare(t, mgr, "slot", later, pubL)
		putShare(t, mgr, "copy", activating, pubA)
		got, err = d.loadKeyShare(shareStoreEpoch, false)
		require.NoError(t, err)
		require.Equal(t, later, got)
		require.Equal(t, pubL, slotPub(t, mgr, algo))

		// No copy (keygen ran on an older binary): the slot as before, no error.
		mgr, d = keyshareBase(t, algo, pubA)
		putShare(t, mgr, "slot", later, pubL)
		got, err = d.loadKeyShare(shareStoreEpoch, true)
		require.NoError(t, err)
		require.Equal(t, later, got)

		// A copy that is not the key's (corrupt) is never used.
		other, _ := testShare(t, algo, 13)
		mgr, d = keyshareBase(t, algo, pubA)
		putShare(t, mgr, "slot", later, pubL)
		require.NoError(t, keystorePutEncrypted(context.Background(), mgr.keyStore, keygenShareKey(pubA), other, mgr.keystoreKey))
		got, err = d.loadKeyShare(shareStoreEpoch, true)
		require.NoError(t, err)
		require.Equal(t, later, got)
		require.Equal(t, pubL, slotPub(t, mgr, algo))
	}
}

// From 0.12.0 storeKeygenResult keeps every keygen's share under its public key
// and leaves the slot alone once another keygen activated the key; below it,
// nothing changes and no copy is written.
func TestTSSKG1_LateKeygenKeepsTheSlot(t *testing.T) {
	activating, pubA := testShare(t, tss_helpers.SigningAlgoEcdsa, 7)
	later, pubL := testShare(t, tss_helpers.SigningAlgoEcdsa, 11)
	for _, tc := range []struct {
		name     string
		check    bool
		status   string
		chainPub string
		wantSlot string
	}{
		{"key still created: the slot is written", true, tss_db.TssKeyCreated, "", pubL},
		{"key active with another keygen's pubkey: the slot is kept", true, tss_db.TssKeyActive, pubA, pubA},
		// A node slower than the 2/3 that signed this keygen's commitment
		// finishes after the commitment activated the key with this keygen's
		// own pubkey: its share is the key's and must be written.
		{"key active with this keygen's pubkey: the slot is written", true, tss_db.TssKeyActive, pubL, pubL},
		{"below 0.12.0: the slot is written as before", false, tss_db.TssKeyActive, pubA, pubL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _ := keyshareBase(t, tss_helpers.SigningAlgoEcdsa, pubA)
			mgr.tssKeys.(*test_utils.MockTssKeysDb).Keys["vsc1Vault-mainv2"] = tss_db.TssKey{Id: "vsc1Vault-mainv2", Status: tc.status, PublicKey: tc.chainPub}
			putShare(t, mgr, "slot", activating, pubA)
			d := &KeyGenDispatcher{BaseDispatcher: shareStoreBase(t, mgr, "keygen-2620-0-vsc1Vault-mainv2")}
			d.keygenShareCheck = tc.check
			pubBytes, err := hex.DecodeString(pubL)
			require.NoError(t, err)
			d.storeKeygenResult(pubBytes, later)
			res, err := d.Done().Await(context.Background())
			require.NoError(t, err, "the keygen still reports success")
			require.IsType(t, KeyGenResult{}, *res)
			require.Equal(t, tc.wantSlot, slotPub(t, mgr, tss_helpers.SigningAlgoEcdsa))
			cp, err := keystoreGetDecrypted(context.Background(), mgr.keyStore, keygenShareKey(pubL), mgr.keystoreKey)
			if !tc.check {
				require.Error(t, err, "below 0.12.0 no copy is written")
				return
			}
			require.NoError(t, err, "from 0.12.0 the copy is written")
			require.Equal(t, later, cp)
		})
	}
}

// The copy must fit a file name for the real EVM bridge vault key id: flatfs
// stores a key as one file, and a name over 255 bytes is refused, so the
// copy would silently not exist and the repair could never run.
func TestTSSKG1_RepairWorksForTheEvmVaultKeyId(t *testing.T) {
	const evmKeyId = "vsc1BorxvTnARoWaTerQgAzNZH7D2r3YjobMFL-primary"
	activating, pubA := testShare(t, tss_helpers.SigningAlgoEcdsa, 7)
	later, pubL := testShare(t, tss_helpers.SigningAlgoEcdsa, 11)
	mgr := shareStoreManager(t, true, true)
	keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{
		evmKeyId: {Id: evmKeyId, Status: tss_db.TssKeyCreated, Epoch: shareStoreEpoch},
	}}
	mgr.tssKeys = keys
	for _, kg := range []struct {
		session string
		pub     string
		share   []byte
	}{{"keygen-500-0-" + evmKeyId, pubA, activating}, {"keygen-520-0-" + evmKeyId, pubL, later}} {
		d := &KeyGenDispatcher{BaseDispatcher: shareStoreBase(t, mgr, kg.session)}
		d.keyId = evmKeyId
		d.keygenShareCheck = true
		pubBytes, err := hex.DecodeString(kg.pub)
		require.NoError(t, err)
		d.storeKeygenResult(pubBytes, kg.share)
		_, err = d.Done().Await(context.Background())
		require.NoError(t, err)
	}
	// keygen-500's commitment activates the key; the slot holds keygen-520's share.
	keys.Keys[evmKeyId] = tss_db.TssKey{Id: evmKeyId, Status: tss_db.TssKeyActive, PublicKey: pubA, Epoch: shareStoreEpoch}
	sign := shareStoreBase(t, mgr, "sign-600-0-"+evmKeyId)
	sign.keyId = evmKeyId
	sign.algo = tss_helpers.SigningAlgoEcdsa
	got, err := sign.loadKeyShare(shareStoreEpoch, true)
	require.NoError(t, err)
	require.Equal(t, activating, got, "the activating keygen's share is used for the EVM vault key")
}
