package islock_attestation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"vsc-node/lib/dids"

	bls "github.com/protolambda/bls12-381-util"
)

// CanonicalSigningMessage computes the bytes a validator signs for an
// IS-lock attestation. Includes the domain-separation prefix from
// lib/dids/dash.go to prevent cross-domain confusion with block voting.
//
//	H( "dash-is-lock-v1\0" || chainId || epoch_be8 || txid || rawTxHash || instructionHash )
//
// Where:
//   - epoch_be8 is the 8-byte big-endian uint64 representation of epoch
//   - txid is the 32-byte raw Dash txid (decoded from hex)
//   - rawTxHash is the 32-byte sha256d of the raw Dash tx
//   - instructionHash is the 32-byte sha256 of the instruction bytes
//
// The result is a 32-byte SHA-256 digest, suitable input to bls.Sign /
// bls.Verify. Domain-separation reasoning: see lib/dids/dash.go's
// DashISLockDomainPrefix and the workstream-0 scoping memo (Q3).
func CanonicalSigningMessage(req IsLockAttestationRequest) ([]byte, error) {
	txid, err := hex.DecodeString(req.TxId)
	if err != nil || len(txid) != 32 {
		return nil, fmt.Errorf("txid must be 32-byte hex, got %q (decoded %d bytes)", req.TxId, len(txid))
	}
	rawTxHash, err := hex.DecodeString(req.RawTxHashHex)
	if err != nil || len(rawTxHash) != 32 {
		return nil, fmt.Errorf("rawTxHash must be 32-byte hex")
	}
	instrHash, err := hex.DecodeString(req.InstructionHashHex)
	if err != nil || len(instrHash) != 32 {
		return nil, fmt.Errorf("instructionHash must be 32-byte hex")
	}
	if req.ChainId == "" {
		return nil, errors.New("chainId must not be empty")
	}

	var buf bytes.Buffer
	buf.WriteString(dids.DashISLockDomainPrefix)
	buf.WriteString(req.ChainId)
	var epochBuf [8]byte
	binary.BigEndian.PutUint64(epochBuf[:], req.Epoch)
	buf.Write(epochBuf[:])
	buf.Write(txid)
	buf.Write(rawTxHash)
	buf.Write(instrHash)

	digest := sha256.Sum256(buf.Bytes())
	return digest[:], nil
}

// Sign produces a BLS signature over the canonical message using the
// validator's consensus secret key. Returns hex-encoded 96-byte sig.
//
// Domain separation is in the canonical message construction
// (CanonicalSigningMessage) — the prefix prevents an attacker from
// crafting a block CID that hashes to the same value as an IS-lock
// message.
func Sign(req IsLockAttestationRequest, sk *bls.SecretKey) (string, error) {
	msg, err := CanonicalSigningMessage(req)
	if err != nil {
		return "", err
	}
	sig := bls.Sign(sk, msg)
	raw := sig.Serialize()
	return hex.EncodeToString(raw[:]), nil
}

// Verify checks that sigHex is a valid BLS signature over the canonical
// message by validatorPubkey.
func Verify(req IsLockAttestationRequest, sigHex string, validatorPubkey *bls.Pubkey) (bool, error) {
	msg, err := CanonicalSigningMessage(req)
	if err != nil {
		return false, err
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != 96 {
		return false, fmt.Errorf("sig must be 96-byte hex")
	}
	var sig bls.Signature
	if err := sig.Deserialize((*[96]byte)(sigBytes)); err != nil {
		return false, fmt.Errorf("sig deserialize: %w", err)
	}
	return bls.Verify(validatorPubkey, msg, &sig), nil
}

// BuildResponse is a convenience: validates the request, signs it,
// returns a populated response with the validator's DID and sig.
func BuildResponse(
	req IsLockAttestationRequest,
	validatorDID string,
	sk *bls.SecretKey,
) (IsLockAttestationResponse, error) {
	sigHex, err := Sign(req, sk)
	if err != nil {
		return IsLockAttestationResponse{}, err
	}
	return IsLockAttestationResponse{
		TxId:         req.TxId,
		ValidatorDID: validatorDID,
		Epoch:        req.Epoch,
		BlsSigHex:    sigHex,
	}, nil
}
