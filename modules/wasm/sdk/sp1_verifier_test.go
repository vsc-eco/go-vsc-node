package sdk

import (
	"encoding/hex"
	"os"
	"testing"
)

// Test fixtures from SP1 v6.1.0 — real Groth16 proof of Sepolia block 10764834.
// VK from SP1 circuits v6.1.0: ~/.sp1/circuits/groth16/v6.1.0/groth16_vk.bin
// SHA256(VK)[0:4] = 4388a21c, verified to match proof prefix.
// Proof generated and verified on-chain 2026-05-01.

const (
	testProofHex = "4388a21c" +
		"0000000000000000000000000000000000000000000000000000000000000000" + // exit_code
		"002f850ee998974d6cc00e50cd0814b098c05bfade466d28573240d057f25352" + // vk_root
		"0000000000000000000000000000000000000000000000000000000000000000" + // nonce
		"23fabeb34922c20b7b16ed695f3bc43e36396f0e480d4be313c9b9eb3317da62" + // ar (G1, 64 bytes)
		"08af915f2337cfc24bb7a179628b5397c7b315f170007dc1951f78849288b2a5" +
		"07b1b5b385621086dff00b8a7e9c993b8cee0dd94493c52397324e22b2c5fc59" + // bs (G2, 128 bytes)
		"269de1e1164b441947f13af05da8faf0ae8da7fcd486e1fc70451280b1cb7bee" +
		"178db74faf7fc33521f434d9c80a02bd8152ef22ff113c75a0111b60967199ad" +
		"1d5b099f5e1877e5c7d123c5627221bfdbf08e8aabc6d84d41f73d4f9cd920f6" +
		"136742c699ddd7d3ca6a039257ce814cdd9c35fcde779cd92475df410ffc647e" + // krs (G1, 64 bytes)
		"1a4e4e425c202322720b71c12c90584078afb6772248658c5a2fa91d6ba123fd"

	testPublicValuesHex = "00000000000000000000000000000000000000000000000000000000000000201024268bf088fa5770d276017f20a3fa4e0cc13c5f854b3a8b1f791cc1b85c3a00000000000000000000000000000000000000000000000000000000009af1e04577257aad51ec8b4519e0ba0546f2a032b2534f87f811a19b27b4d42278787d00000000000000000000000000000000000000000000000000000000009af220999525d0726b588e6cc9bd6841eb5393bc0b5137d980b30f8ed136efdd8a348e6efab94327bc2fd7eae04922c44be6be72f4e9f402410df10131be20870dc15e7cfbfa1dcd1490246a97443bbcce8e841e432d25df8f2ad1b6258e06441a3bdf0000000000000000000000000000000000000000000000000000000000a442224577257aad51ec8b4519e0ba0546f2a032b2534f87f811a19b27b4d42278787d4293e591071f6fb96e4a99a578693578bf4a9125c17b20abb845d3861c248ee000000000000000000000000000000000000000000000000000000000000001600000000000000000000000000000000000000000000000000000000000000000"

	testSp1VkeyHashHex = "00f701535d87c17492e85e1b32649f2578a4e02e7e314401c726af21bba2523d"

	testVkRootHex = "002f850ee998974d6cc00e50cd0814b098c05bfade466d28573240d057f25352"
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
