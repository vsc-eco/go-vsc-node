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
//
// CRITICAL BYTE-ORDER NOTE — fixes audit finding
// `canonical-message-txid-byte-order-drift`:
//
//   The canonical message embeds txid and rawTxHash in INTERNAL byte order
//   (the raw output of sha256d(rawTxBytes)), NOT Bitcoin's display order.
//   Display order is internal-reversed; dashd's getrawtransaction returns
//   txids in display form.
//
//   Wire format (req.TxId, req.RawTxHashHex) carries DISPLAY-ORDER hex so
//   operators / logs can copy-paste txids straight from explorers. The
//   reversal happens here, inside the signing-message builder, on BOTH
//   the sign and verify paths so the BLS aggregate computed by validators
//   matches what the dash-mapping-contract recomputes via
//   sha256.Sum256(sha256.Sum256(rawTxBytes)).
func CanonicalSigningMessage(req IsLockAttestationRequest) ([]byte, error) {
	txidDisplay, err := hex.DecodeString(req.TxId)
	if err != nil || len(txidDisplay) != 32 {
		return nil, fmt.Errorf("txid must be 32-byte hex, got %q (decoded %d bytes)", req.TxId, len(txidDisplay))
	}
	rawTxHashDisplay, err := hex.DecodeString(req.RawTxHashHex)
	if err != nil || len(rawTxHashDisplay) != 32 {
		return nil, fmt.Errorf("rawTxHash must be 32-byte hex")
	}
	instrHash, err := hex.DecodeString(req.InstructionHashHex)
	if err != nil || len(instrHash) != 32 {
		return nil, fmt.Errorf("instructionHash must be 32-byte hex")
	}
	if req.ChainId == "" {
		return nil, errors.New("chainId must not be empty")
	}

	// Reverse to internal sha256d byte order — matches the contract's
	// CanonicalAttestationMessage recomputation.
	txidInternal := ReverseBytesCopy(txidDisplay)
	rawTxHashInternal := ReverseBytesCopy(rawTxHashDisplay)

	var buf bytes.Buffer
	buf.WriteString(dids.DashISLockDomainPrefix)
	buf.WriteString(req.ChainId)
	var epochBuf [8]byte
	binary.BigEndian.PutUint64(epochBuf[:], req.Epoch)
	buf.Write(epochBuf[:])
	buf.Write(txidInternal)
	buf.Write(rawTxHashInternal)
	buf.Write(instrHash)

	digest := sha256.Sum256(buf.Bytes())
	return digest[:], nil
}

// ReverseBytesCopy returns a NEW slice with bytes in reverse order.
// Public helper because the IS service's orchestrator + the contract
// integration tests both need the same primitive when constructing /
// asserting canonical messages.
func ReverseBytesCopy(in []byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[len(in)-1-i] = b
	}
	return out
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
// returns a populated response with the validator's DID, pubkey, and sig.
//
// PubkeyHex is included in the response so the IS-service-side
// aggregator can hand it to the contract without a separate validator-set
// query. The contract is the authority on whether the pubkey actually
// matches the registered one for ValidatorDID at the claimed epoch.
func BuildResponse(
	req IsLockAttestationRequest,
	validatorDID string,
	sk *bls.SecretKey,
) (IsLockAttestationResponse, error) {
	sigHex, err := Sign(req, sk)
	if err != nil {
		return IsLockAttestationResponse{}, err
	}
	pk, err := bls.SkToPk(sk)
	if err != nil {
		return IsLockAttestationResponse{}, fmt.Errorf("derive pubkey: %w", err)
	}
	pkBytes := pk.Serialize()
	return IsLockAttestationResponse{
		TxId:         req.TxId,
		ValidatorDID: validatorDID,
		PubkeyHex:    hex.EncodeToString(pkBytes[:]),
		Epoch:        req.Epoch,
		BlsSigHex:    sigHex,
	}, nil
}

// VerifyAttestation checks a single response's BlsSigHex against its
// PubkeyHex over the canonical message for req. Returns (true, nil)
// only when the BLS pairing verifies. Round-2 audit R2-N5: gating
// aggregation behind this check prevents a single malicious peer with
// QuorumThreshold=1 from forcing the operator to broadcast junk to L2
// and burn RC.
//
// The contract still re-checks aggregate verification + per-DID pubkey
// matching against the registered validator set; this is a cheap
// off-chain pre-filter so the operator only pays RC for plausible
// aggregates.
func VerifyAttestation(req IsLockAttestationRequest, resp IsLockAttestationResponse) (bool, error) {
	if len(resp.PubkeyHex) != 96 {
		return false, fmt.Errorf("PubkeyHex must be 48 bytes (96 hex chars)")
	}
	pkBytes, err := hex.DecodeString(resp.PubkeyHex)
	if err != nil {
		return false, fmt.Errorf("PubkeyHex decode: %w", err)
	}
	var pk bls.Pubkey
	if err := pk.Deserialize((*[48]byte)(pkBytes)); err != nil {
		return false, fmt.Errorf("pubkey deserialize: %w", err)
	}
	return Verify(req, resp.BlsSigHex, &pk)
}

// AggregateSignatures takes the BlsSigHex of each response and returns
// hex(aggregated 96-byte BLS signature). Empty input → error. Used by
// the IS-service-side orchestrator to build the aggregate sig the
// contract verifies via crypto.bls_verify_aggregate.
func AggregateSignatures(responses []IsLockAttestationResponse) (string, error) {
	if len(responses) == 0 {
		return "", errors.New("cannot aggregate zero signatures")
	}
	sigs := make([]*bls.Signature, 0, len(responses))
	for i, r := range responses {
		raw, err := hex.DecodeString(r.BlsSigHex)
		if err != nil || len(raw) != 96 {
			return "", fmt.Errorf("response %d: BlsSigHex must be 96-byte hex", i)
		}
		var s bls.Signature
		if err := s.Deserialize((*[96]byte)(raw)); err != nil {
			return "", fmt.Errorf("response %d: sig deserialize: %w", i, err)
		}
		sigs = append(sigs, &s)
	}
	agg, err := bls.Aggregate(sigs)
	if err != nil {
		return "", fmt.Errorf("bls.Aggregate: %w", err)
	}
	raw := agg.Serialize()
	return hex.EncodeToString(raw[:]), nil
}
