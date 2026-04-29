package sdk

import (
	"encoding/hex"
	"os"
	"testing"
)

// Test fixtures from SP1 contracts v6.0.0 Solidity test:
// https://github.com/succinctlabs/sp1-contracts/blob/main/contracts/test/SP1VerifierGroth16V6.sol
// VK from SP1 repo tag v6.0.0: crates/verifier/vk-artifacts/groth16_vk.bin
// SHA256(VK)[0:4] = 0e78f4db, verified to match proof prefix.

const (
	testProofHex = "0e78f4db" +
		"0000000000000000000000000000000000000000000000000000000000000000" + // exit_code
		"008cd56e10c2fe24795cff1e1d1f40d3a324528d315674da45d26afb376e8670" + // vk_root
		"0000000000000000000000000000000000000000000000000000000000000000" + // nonce
		"1e2e0ff3f06238b1a0f13c92e9e6dc64bd103006ab92764445220fecc884ac6f" + // ar (G1, 64 bytes)
		"2c7c5c82d3914e85262103f1e262b35d8d3c1e513672ef8482861f21241a8618" +
		"031f773e85f625d0d3dd3f24b5dcc8489de1b12f38921c7fe55bc2b6d9abc6e7" + // bs (G2, 128 bytes)
		"16ed0979ff2668a4d50b2f2af2d0513a008e21823a93a19ce53287206191b5e7" +
		"1206f6bd7829711609fa90159683133d43d5e7ca12fb42d99bfd84d0961c0228" +
		"2dea228329dc9f774d214eb74f5c583e63035a9e6b7aae3b5c1f9e6b8929deca" +
		"2d494130d03ce620238c428eb75c605aff699b779d51b21e93065ffda48c7e5b" + // krs (G1, 64 bytes)
		"0c188f0e786e474cbf2263fb9fe2b9260fb4b08332b7ab331fc3f4f9b247a393"

	testPublicValuesHex = "0000000000000000000000000000000000000000000000000000000000000014" +
		"0000000000000000000000000000000000000000000000000000000000001a6d" +
		"0000000000000000000000000000000000000000000000000000000000002ac2"

	testSp1VkeyHashHex = "004a55ed3c7a07d0233a027278a8b7ff8681ffbd5d1ec4795c18966f6e693090"

	testVkRootHex = "008cd56e10c2fe24795cff1e1d1f40d3a324528d315674da45d26afb376e8670"
)

func loadTestVk(t *testing.T) []byte {
	t.Helper()
	vk, err := os.ReadFile("testdata/groth16_vk.bin")
	if err != nil {
		t.Fatalf("Failed to read test VK: %v", err)
	}
	return vk
}

func mustHexDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("Invalid test hex: %v", err)
	}
	return b
}

func TestSp1VerifyGroth16_ValidProof(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err != nil {
		t.Fatalf("Valid proof rejected: %v", err)
	}
}

func TestSp1VerifyGroth16_TamperedProof(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	// Flip one bit in the raw Gnark proof section
	proof[200] ^= 0x01

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err == nil {
		t.Fatal("Tampered proof should have been rejected")
	}
	t.Logf("Tampered proof correctly rejected: %v", err)
}

func TestSp1VerifyGroth16_WrongVkey(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	// Corrupt the vkey hash
	vkeyHash[0] ^= 0xFF

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err == nil {
		t.Fatal("Wrong vkey should have been rejected")
	}
	t.Logf("Wrong vkey correctly rejected: %v", err)
}

func TestSp1VerifyGroth16_WrongPublicInputs(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	// Corrupt public inputs
	publicInputs[10] ^= 0x01

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err == nil {
		t.Fatal("Wrong public inputs should have been rejected")
	}
	t.Logf("Wrong public inputs correctly rejected: %v", err)
}

func TestSp1VerifyGroth16_WrongVkRoot(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	// Wrong VK root
	vkRoot[0] ^= 0xFF

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err == nil {
		t.Fatal("Wrong VK root should have been rejected")
	}
	t.Logf("Wrong VK root correctly rejected: %v", err)
}

func TestSp1VerifyGroth16_WrongGroth16Vk(t *testing.T) {
	proof := mustHexDecode(t, testProofHex)
	publicInputs := mustHexDecode(t, testPublicValuesHex)
	vkeyHash := mustHexDecode(t, testSp1VkeyHashHex)
	vkRoot := mustHexDecode(t, testVkRootHex)
	groth16Vk := loadTestVk(t)

	// Corrupt the Groth16 VK — prefix check will fail
	groth16Vk[0] ^= 0xFF

	err := Sp1VerifyGroth16(proof, publicInputs, vkeyHash, groth16Vk, vkRoot)
	if err == nil {
		t.Fatal("Wrong Groth16 VK should have been rejected")
	}
	t.Logf("Wrong Groth16 VK correctly rejected: %v", err)
}

func TestSp1VerifyGroth16_ShortProof(t *testing.T) {
	err := Sp1VerifyGroth16([]byte{1, 2, 3}, []byte{}, []byte{}, []byte{}, []byte{})
	if err == nil {
		t.Fatal("Short proof should have been rejected")
	}
}
