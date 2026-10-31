package sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// The verifier runs inside every validator for any submitted proof, so no
// input may panic it (Code4rena 2025-09 Succinct M-02/M-04: SP1's own verifiers
// panicked on malformed proofs and VKs). These fuzz the proof against the real
// VK, and the VK with the proof's VK-hash prefix recomputed so the parse runs.

func sp1FuzzFixture(f *testing.F) (proof, pv, vkey, vk, root []byte) {
	f.Helper()
	vk, err := os.ReadFile("testdata/groth16_vk.bin")
	if err != nil {
		f.Fatal(err)
	}
	dec := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	return dec(testProofHex), dec(testPublicValuesHex), dec(testSp1VkeyHashHex), vk, dec(testVkRootHex)
}

func FuzzSp1VerifyGroth16Proof(f *testing.F) {
	proof, pv, vkey, vk, root := sp1FuzzFixture(f)
	f.Add(proof, pv)
	f.Add(proof[:sp1ProofMinLen], []byte{})
	f.Fuzz(func(t *testing.T, proof, pv []byte) {
		_ = Sp1VerifyGroth16(proof, pv, vkey, vk, root)
	})
}

func FuzzSp1VerifyGroth16Vk(f *testing.F) {
	proof, pv, vkey, vk, root := sp1FuzzFixture(f)
	f.Add(vk)
	f.Add(vk[:sp1VkMinLen])
	f.Fuzz(func(t *testing.T, vk []byte) {
		p := append([]byte(nil), proof...)
		h := sha256.Sum256(vk)
		copy(p[:sp1VkHashPrefixLen], h[:sp1VkHashPrefixLen])
		_ = Sp1VerifyGroth16(p, pv, vkey, vk, root)
	})
}
