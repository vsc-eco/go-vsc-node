package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AddressSignerEd25519 signs (depositAddress || \0 || instruction)
// with an Ed25519 private key loaded from disk. Public key gets
// pinned in the Altera client via PUBLIC_IS_SERVICE_SIGNER_PUBKEY.
//
// Spec §5.7 mandates an HSM/KMS-backed asymmetric signer for
// production. This implementation is a step beyond the HMAC stub
// but NOT a full HSM solution — the private key still lives on the
// IS-service host's filesystem. Operators should treat the key
// file with at-rest encryption (sealed via host-level KMS) +
// filesystem ACLs and rotate via the planned key-rotation workflow.
//
// The HSM/KMS-backed implementation is the next workstream — it
// will share THIS file's AddressSigner interface so the IS service
// gains HSM support via a constructor swap, no caller changes.
//
// On-disk format: 32-byte raw Ed25519 seed, hex-encoded (64 ASCII
// chars), one line, optional trailing newline. The seed expands
// to the 64-byte private key via ed25519.NewKeyFromSeed (RFC 8032
// canonical derivation). Matches the existing IS-login spec
// §5.7's signer-pubkey pinning shape (hex on the wire, base64 raw
// on the AddressSignature response field).
type AddressSignerEd25519 struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewAddressSignerEd25519FromFile loads a 32-byte seed (hex-
// encoded) from `path`. Returns the constructed signer + the
// derived public key as hex (the operator pins this hex in
// PUBLIC_IS_SERVICE_SIGNER_PUBKEY).
//
// Refuses files with permissive permissions (>0o600) on POSIX-like
// hosts — Ed25519 seed exposure is fatal.
func NewAddressSignerEd25519FromFile(path string) (*AddressSignerEd25519, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("stat signer key file: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return nil, "", fmt.Errorf(
			"signer key file %s is world/group-readable (mode %o); refusing to load — "+
				"set to 0o600 and re-run", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read signer key file: %w", err)
	}
	seedHex := strings.TrimSpace(string(raw))
	if len(seedHex) != 64 {
		return nil, "", fmt.Errorf(
			"signer key file must contain 32 hex bytes (64 chars), got %d", len(seedHex))
	}
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return nil, "", fmt.Errorf("decoding seed hex: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &AddressSignerEd25519{priv: priv, pub: pub}, hex.EncodeToString(pub), nil
}

// Sign produces an Ed25519 signature over the canonical message:
//
//	depositAddress || 0x00 || instruction
//
// Matches AddressSignerHMAC's byte-layout so the addressSignature
// field semantics are stable across signer kinds. Returns
// base64-StdEncoding (matching HMAC's wire shape) so Altera can
// decode through the same code path.
func (s *AddressSignerEd25519) Sign(depositAddress, instruction string) (string, error) {
	if s.priv == nil {
		return "", errors.New("address signer not initialised")
	}
	buf := make([]byte, 0, len(depositAddress)+1+len(instruction))
	buf = append(buf, []byte(depositAddress)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(instruction)...)
	sig := ed25519.Sign(s.priv, buf)
	return base64.StdEncoding.EncodeToString(sig), nil
}

// PubkeyHex returns the signer's public key as 64-char hex. The
// operator pins this in Altera's PUBLIC_IS_SERVICE_SIGNER_PUBKEY
// so the client can verify each /session/start response's
// addressSignature.
func (s *AddressSignerEd25519) PubkeyHex() string {
	return hex.EncodeToString(s.pub)
}
