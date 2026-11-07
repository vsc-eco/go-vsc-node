package tss

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"

	tss_helpers "vsc-node/modules/tss/helpers"

	keyGenSecp256k1 "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
	keyGenEddsa "github.com/bnb-chain/tss-lib/v3/eddsa/keygen"
	btss "github.com/bnb-chain/tss-lib/v3/tss"
	"github.com/decred/dcrd/dcrec/edwards/v2"
	"github.com/eager7/dogd/btcec"
	"github.com/ipfs/go-datastore"
)

// TSS-KG-1: a key's keygen re-runs every rotation while the key is still
// "created", so several keygens can succeed before the first commitment lands,
// and the one the chain activates need not be the one whose share sits in the
// (keyId, epoch) slot: same-epoch keygens write the same slot. From 0.13.0
// every keygen result is therefore also kept under its public key
// (keygenShareKey), and a share loaded for a keygen-born epoch that is not the
// key's on-chain public key is replaced by the activating keygen's copy.

// keygenShareKey is where storeKeygenResult keeps a keygen's share, by its
// public key (hex, as committed on chain), which names one keygen's output.
// The key id is left out: flatfs keeps each key as one file, and the EVM vault
// key's id plus a public key would encode past the 255-byte file name limit.
func keygenShareKey(pubHex string) datastore.Key {
	return makeKey("kgpub", pubHex)
}

// sharePubHex is the public key a stored share belongs to, encoded as the
// keygen commits it (compressed secp256k1 / compressed edwards).
func sharePubHex(algo tss_helpers.SigningAlgo, raw []byte) (string, error) {
	switch algo {
	case tss_helpers.SigningAlgoEcdsa:
		var kd keyGenSecp256k1.LocalPartySaveData
		if err := json.Unmarshal(raw, &kd); err != nil || kd.ECDSAPub == nil {
			return "", fmt.Errorf("not an ECDSA share: %v", err)
		}
		return pointPubHex(algo, kd.ECDSAPub.X(), kd.ECDSAPub.Y())
	case tss_helpers.SigningAlgoEddsa:
		var kd keyGenEddsa.LocalPartySaveData
		if err := json.Unmarshal(raw, &kd); err != nil || kd.EDDSAPub == nil {
			return "", fmt.Errorf("not an EdDSA share: %v", err)
		}
		return pointPubHex(algo, kd.EDDSAPub.X(), kd.EDDSAPub.Y())
	}
	return "", fmt.Errorf("unknown algo %q", algo)
}

// pointPubHex encodes the public key (x, y) on algo's curve as a keygen
// commits it (compressed secp256k1 / compressed edwards).
func pointPubHex(algo tss_helpers.SigningAlgo, x, y *big.Int) (string, error) {
	switch algo {
	case tss_helpers.SigningAlgoEcdsa:
		pk := btcec.PublicKey{Curve: btss.S256(), X: x, Y: y}
		return hex.EncodeToString(pk.SerializeCompressed()), nil
	case tss_helpers.SigningAlgoEddsa:
		return hex.EncodeToString(edwards.NewPublicKey(x, y).SerializeCompressed()), nil
	}
	return "", fmt.Errorf("unknown algo %q", algo)
}

// loadKeyShare reads this node's share of keyId at epoch. With check set (the
// share was born of the key's keygen, from 0.13.0) and the slot's public key
// not the key's on-chain one, the activating keygen's copy is written back to
// the slot and returned, so every reader of the slot sees it. Without a copy
// the slot's share is returned as before.
func (dispatcher *BaseDispatcher) loadKeyShare(epoch uint64, check bool) ([]byte, error) {
	encKey, err := dispatcher.tssMgr.keystoreEncryptionKey()
	if err != nil {
		return nil, err
	}
	slot := makeKey("key", dispatcher.keyId, int(epoch))
	raw, err := keystoreGetDecrypted(context.Background(), dispatcher.keystore, slot, encKey)
	if err != nil || !check {
		return raw, err
	}
	key, kerr := dispatcher.tssMgr.tssKeys.FindKey(dispatcher.keyId)
	if kerr != nil || key.PublicKey == "" {
		return raw, nil
	}
	if pub, perr := sharePubHex(dispatcher.algo, raw); perr == nil && pub == key.PublicKey {
		return raw, nil
	}
	copyRaw, cerr := keystoreGetDecrypted(context.Background(), dispatcher.keystore, keygenShareKey(key.PublicKey), encKey)
	if cerr != nil {
		log.Error("TSS-KG-1: this node's share is not the key's and no copy of the activating keygen's share exists",
			"keyId", dispatcher.keyId, "epoch", epoch, "keyPubKey", key.PublicKey, "sessionId", dispatcher.sessionId)
		return raw, nil
	}
	if pub, perr := sharePubHex(dispatcher.algo, copyRaw); perr != nil || pub != key.PublicKey {
		return raw, nil
	}
	if err := keystorePutEncrypted(context.Background(), dispatcher.keystore, slot, copyRaw, encKey); err != nil {
		return nil, fmt.Errorf("restore the activating keygen's share: %w", err)
	}
	log.Warn("TSS-KG-1: restored the activating keygen's share over another keygen's",
		"keyId", dispatcher.keyId, "epoch", epoch, "keyPubKey", key.PublicKey, "sessionId", dispatcher.sessionId)
	return copyRaw, nil
}
