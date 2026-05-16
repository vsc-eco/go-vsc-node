package tss

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	datastore "github.com/ipfs/go-datastore"
)

// review2 CRITICAL #4: TSS key shares (ECDSA Xi, Paillier SK N/P/Q, etc.)
// were written to the flatfs keystore as plaintext json.Marshal output. These
// helpers add AES-256-GCM encryption at rest. The data-encryption key is
// derived from the node's existing 32-byte BLS seed (already required at TSS
// init, stable across restarts, never leaves the host) via HKDF-SHA256 with a
// fixed context label so the keystore key is domain-separated from the BLS
// key itself.
//
// keystoreEncMagic prefixes every encrypted blob. It contains a NUL byte so
// it can never be confused with the legacy plaintext payloads, which are JSON
// and therefore always start with '{' or '['. decryptKeystoreBlob treats any
// blob lacking the prefix as legacy plaintext and returns it unchanged — so
// pre-existing on-disk shares keep loading and are transparently re-encrypted
// the next time they are written.
var keystoreEncMagic = []byte("VSCK1\x00")

const keystoreKeyHKDFInfo = "vsc-tss-keystore-at-rest-v1"

// deriveKeystoreKey turns the hex-encoded 32-byte BLS seed into a 32-byte
// AES-256 key. Deterministic: the same seed always yields the same key, so a
// node can decrypt shares written before a restart.
func deriveKeystoreKey(blsSeedHex string) ([]byte, error) {
	seed, err := hex.DecodeString(blsSeedHex)
	if err != nil {
		return nil, fmt.Errorf("decode bls seed: %w", err)
	}
	if len(seed) != 32 {
		return nil, fmt.Errorf("bls seed must be 32 bytes, got %d", len(seed))
	}
	key, err := hkdf.Key(sha256.New, seed, nil, keystoreKeyHKDFInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("hkdf derive keystore key: %w", err)
	}
	return key, nil
}

// encryptKeystoreBlob returns keystoreEncMagic || nonce || AES-256-GCM(plaintext).
func encryptKeystoreBlob(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	out := make([]byte, 0, len(keystoreEncMagic)+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, keystoreEncMagic...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// decryptKeystoreBlob reverses encryptKeystoreBlob. A blob without the magic
// prefix is a legacy plaintext key file and is returned unchanged.
func decryptKeystoreBlob(key, data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, keystoreEncMagic) {
		// Legacy plaintext share — load as-is (migrated on next write).
		return data, nil
	}
	body := data[len(keystoreEncMagic):]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("keystore blob truncated: shorter than nonce")
	}
	nonce := body[:gcm.NonceSize()]
	ciphertext := body[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("gcm open (wrong key or tampered keystore blob): %w", err)
	}
	return plaintext, nil
}

// keystoreEncryptionKey returns the node's at-rest data-encryption key,
// derived once from the BLS seed and cached. An error here is fatal for any
// key-share read/write: callers MUST propagate it rather than fall back to
// plaintext.
func (tssMgr *TssManager) keystoreEncryptionKey() ([]byte, error) {
	tssMgr.keystoreKeyOnce.Do(func() {
		tssMgr.keystoreKey, tssMgr.keystoreKeyErr = deriveKeystoreKey(tssMgr.config.Get().BlsPrivKeySeed)
	})
	return tssMgr.keystoreKey, tssMgr.keystoreKeyErr
}

// keystorePutEncrypted encrypts plaintext and writes it to the datastore.
// Every TSS key-share write MUST go through this so nothing is persisted in
// the clear.
func keystorePutEncrypted(ctx context.Context, ds datastore.Datastore, k datastore.Key, plaintext, encKey []byte) error {
	blob, err := encryptKeystoreBlob(encKey, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt keystore blob: %w", err)
	}
	return ds.Put(ctx, k, blob)
}

// keystoreGetDecrypted reads a blob and decrypts it. Legacy plaintext shares
// (written before this change) pass through unchanged, so an existing node
// keeps working and is migrated to ciphertext on its next write. Every TSS
// key-share read MUST go through this — missing one read site would make the
// node silently fail to load its share.
func keystoreGetDecrypted(ctx context.Context, ds datastore.Datastore, k datastore.Key, encKey []byte) ([]byte, error) {
	blob, err := ds.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	return decryptKeystoreBlob(encKey, blob)
}
