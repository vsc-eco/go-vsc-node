package sdk

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"lukechampine.com/blake3"
)

// SP1 Groth16 proof format constants.
// These define the WIRE FORMAT, not version-specific data.
// Stable since SP1 v2.0 (Oct 2024).
const (
	sp1VkHashPrefixLen = 4
	sp1Groth16ProofLen = 256
	sp1ProofMinLen     = sp1VkHashPrefixLen + 32 + 32 + 32 + sp1Groth16ProofLen // 356
	sp1VkMinLen        = 292                                                     // fixed fields before IC array
)

var errSp1InvalidProof = errors.New("sp1: invalid proof")

// Sp1VerifyGroth16 verifies an SP1 Groth16 proof.
// All verification parameters come from the caller — zero hardcoded constants.
//
// Parameters:
//   - proof: the full SP1 proof (356 bytes minimum)
//   - publicInputs: the SP1 public values (ABI-encoded ProofOutputs)
//   - sp1VkeyHash: the SP1 program verification key hash (32 bytes)
//   - groth16Vk: the Groth16 verification key bytes (492 bytes for SP1 v5.2.4)
//   - vkRoot: the SP1 recursion VK root (32 bytes, version-specific)
//
// Returns nil on valid proof, error on invalid proof or bad input.
func Sp1VerifyGroth16(proof, publicInputs, sp1VkeyHash, groth16Vk, vkRoot []byte) error {
	if len(proof) < sp1ProofMinLen {
		return fmt.Errorf("%w: proof too short (%d < %d)", errSp1InvalidProof, len(proof), sp1ProofMinLen)
	}
	if len(sp1VkeyHash) != 32 {
		return fmt.Errorf("%w: sp1 vkey hash must be 32 bytes", errSp1InvalidProof)
	}
	if len(groth16Vk) < sp1VkMinLen {
		return fmt.Errorf("%w: groth16 vk too short (%d < %d)", errSp1InvalidProof, len(groth16Vk), sp1VkMinLen)
	}
	if len(vkRoot) != 32 {
		return fmt.Errorf("%w: vk root must be 32 bytes", errSp1InvalidProof)
	}

	// --- Parse proof prefix ---
	// [0:4]    SHA256(groth16_vk) prefix
	// [4:36]   exit_code
	// [36:68]  vk_root
	// [68:100] nonce
	// [100:]   raw Gnark Groth16 proof (256 bytes)

	// 1. Verify Groth16 VK hash prefix
	vkHash := sha256.Sum256(groth16Vk)
	for i := 0; i < sp1VkHashPrefixLen; i++ {
		if vkHash[i] != proof[i] {
			return fmt.Errorf("%w: groth16 vk hash prefix mismatch", errSp1InvalidProof)
		}
	}

	// 2. Extract prefix fields
	exitCode := proof[sp1VkHashPrefixLen : sp1VkHashPrefixLen+32]
	proofVkRoot := proof[sp1VkHashPrefixLen+32 : sp1VkHashPrefixLen+64]
	proofNonce := proof[sp1VkHashPrefixLen+64 : sp1VkHashPrefixLen+96]
	rawProof := proof[sp1VkHashPrefixLen+96:]

	// 3. Verify exit code is all zeros (success)
	for _, b := range exitCode {
		if b != 0 {
			return fmt.Errorf("%w: non-zero exit code", errSp1InvalidProof)
		}
	}

	// 4. Verify VK root matches expected
	for i := 0; i < 32; i++ {
		if proofVkRoot[i] != vkRoot[i] {
			return fmt.Errorf("%w: vk root mismatch", errSp1InvalidProof)
		}
	}

	// 5. Build the 5 Groth16 public inputs:
	//   [0] sp1_vkey_hash
	//   [1] hash(public_inputs) — try SHA256 first, then Blake3
	//   [2] exit_code
	//   [3] vk_root
	//   [4] nonce
	var input0, input2, input3, input4 [32]byte
	copy(input0[:], sp1VkeyHash)
	copy(input2[:], exitCode)
	copy(input3[:], proofVkRoot)
	copy(input4[:], proofNonce)

	// Try SHA256 hash of public inputs first.
	// Mask to 253 bits (clear top 3 bits) for BN254 field compatibility.
	// Matches: Solidity hashPublicValues() and Rust hash_public_inputs().
	piHashSha := sha256.Sum256(publicInputs)
	piHashSha[0] &= 0x1F
	inputs := [][32]byte{input0, piHashSha, input2, input3, input4}
	if err := sp1VerifyGnarkGroth16(rawProof, inputs, groth16Vk); err == nil {
		return nil
	}

	// Fallback: Blake3 hash of public inputs (same 253-bit masking)
	piHashBlake := sp1Blake3Sum256(publicInputs)
	piHashBlake[0] &= 0x1F
	inputs[1] = piHashBlake
	return sp1VerifyGnarkGroth16(rawProof, inputs, groth16Vk)
}

func sp1Blake3Sum256(data []byte) [32]byte {
	h := blake3.New(32, nil)
	h.Write(data)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// sp1VerifyGnarkGroth16 performs the raw Gnark Groth16 pairing check.
//
// Proof format (256 bytes, Gnark uncompressed):
//
//	[0:64]    ar  — G1 uncompressed
//	[64:192]  bs  — G2 uncompressed
//	[192:256] krs — G1 uncompressed
//
// VK format (292 + num_k*32 bytes, Gnark compressed):
//
//	[0:32]    alpha — G1 compressed
//	[32:64]   (padding)
//	[64:128]  beta  — G2 compressed
//	[128:192] gamma — G2 compressed
//	[192:224] (padding)
//	[224:288] delta — G2 compressed
//	[288:292] num_k — uint32 big-endian
//	[292:]    IC[0..num_k] — G1 compressed, 32 bytes each
func sp1VerifyGnarkGroth16(proof []byte, publicInputs [][32]byte, groth16Vk []byte) error {
	if len(proof) != sp1Groth16ProofLen {
		return fmt.Errorf("%w: raw proof must be %d bytes, got %d", errSp1InvalidProof, sp1Groth16ProofLen, len(proof))
	}

	// --- Deserialize proof points (uncompressed) ---
	var ar bn254.G1Affine
	if _, err := ar.SetBytes(proof[:64]); err != nil {
		return fmt.Errorf("%w: decode ar: %v", errSp1InvalidProof, err)
	}
	var bs bn254.G2Affine
	if _, err := bs.SetBytes(proof[64:192]); err != nil {
		return fmt.Errorf("%w: decode bs: %v", errSp1InvalidProof, err)
	}
	var krs bn254.G1Affine
	if _, err := krs.SetBytes(proof[192:256]); err != nil {
		return fmt.Errorf("%w: decode krs: %v", errSp1InvalidProof, err)
	}

	// --- Deserialize VK points (compressed) ---
	var alpha bn254.G1Affine
	if _, err := alpha.SetBytes(groth16Vk[:32]); err != nil {
		return fmt.Errorf("%w: decode vk.alpha: %v", errSp1InvalidProof, err)
	}

	// beta at VK[64:128]
	// SP1 converter.rs:63 negates beta during deserialization: vk.beta = -g2_beta
	// SP1 verify.rs:64 negates again in pairing: -vk.beta = -(-g2_beta) = g2_beta
	// We replicate both negations for exact behavioral match.
	var beta bn254.G2Affine
	if _, err := beta.SetBytes(groth16Vk[64:128]); err != nil {
		return fmt.Errorf("%w: decode vk.beta: %v", errSp1InvalidProof, err)
	}
	beta.Neg(&beta) // first negation: matches converter.rs:63

	var gamma bn254.G2Affine
	if _, err := gamma.SetBytes(groth16Vk[128:192]); err != nil {
		return fmt.Errorf("%w: decode vk.gamma: %v", errSp1InvalidProof, err)
	}

	// skip VK[192:224] padding

	var delta bn254.G2Affine
	if _, err := delta.SetBytes(groth16Vk[224:288]); err != nil {
		return fmt.Errorf("%w: decode vk.delta: %v", errSp1InvalidProof, err)
	}

	// IC points
	numK := int(groth16Vk[288])<<24 | int(groth16Vk[289])<<16 | int(groth16Vk[290])<<8 | int(groth16Vk[291])
	expectedLen := 292 + numK*32
	if len(groth16Vk) < expectedLen {
		return fmt.Errorf("%w: vk too short for %d IC points (need %d, have %d)",
			errSp1InvalidProof, numK, expectedLen, len(groth16Vk))
	}
	if numK != len(publicInputs)+1 {
		return fmt.Errorf("%w: IC count %d != public inputs %d + 1",
			errSp1InvalidProof, numK, len(publicInputs))
	}

	ic := make([]bn254.G1Affine, numK)
	for i := 0; i < numK; i++ {
		off := 292 + i*32
		if _, err := ic[i].SetBytes(groth16Vk[off : off+32]); err != nil {
			return fmt.Errorf("%w: decode vk.IC[%d]: %v", errSp1InvalidProof, i, err)
		}
	}

	// --- Prepare inputs: vkX = IC[0] + Σ(input[i] * IC[i+1]) ---
	// Matches verify.rs:42-46
	vkX := ic[0]
	for i, input := range publicInputs {
		var scalar fr.Element
		scalar.SetBytes(input[:])
		if scalar.IsZero() {
			continue
		}
		var scalarBI big.Int
		scalar.BigInt(&scalarBI)
		var term bn254.G1Affine
		term.ScalarMultiplication(&ic[i+1], &scalarBI)
		vkX.Add(&vkX, &term)
	}

	// --- Pairing check ---
	// Standard Groth16: e(-A, B) · e(vkX, γ) · e(C, δ) · e(α, β) = 1
	//
	// verify.rs:60-65:
	//   pairing_batch(&[
	//     (-ar, bs),                    ← e(-A, B)
	//     (prepared_inputs, gamma),     ← e(vkX, γ)
	//     (krs, delta),                 ← e(C, δ)
	//     (alpha, -vk.g2.beta),         ← e(α, -(-β_raw)) = e(α, β_raw)
	//   ])
	var negAr bn254.G1Affine
	negAr.Neg(&ar)

	var negBeta bn254.G2Affine
	negBeta.Neg(&beta) // second negation: matches verify.rs:64

	ok, err := bn254.PairingCheck(
		[]bn254.G1Affine{negAr, vkX, krs, alpha},
		[]bn254.G2Affine{bs, gamma, delta, negBeta},
	)
	if err != nil {
		return fmt.Errorf("%w: pairing error: %v", errSp1InvalidProof, err)
	}
	if !ok {
		return fmt.Errorf("%w: pairing check failed", errSp1InvalidProof)
	}

	return nil
}
