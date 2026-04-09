package tss

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"testing"

	"vsc-node/modules/db/vsc/elections"

	blsu "github.com/protolambda/bls12-381-util"
)

// generateTestBLSKey creates a BLS keypair from a deterministic seed for testing.
func generateTestBLSKey(seed byte) (blsu.SecretKey, blsu.Pubkey) {
	var arr [32]byte
	arr[0] = seed
	var sk blsu.SecretKey
	sk.Deserialize(&arr)
	pk, _ := blsu.SkToPk(&sk)
	return sk, *pk
}

// blsPubKeyToDID converts a BLS public key to a did:key string for election members.
// Uses a simple hex encoding — real DIDs use multibase, but for testing we just need
// the Identifier() method to parse it back. We sign/verify at the raw level instead.
func testPubKeyHex(pk blsu.Pubkey) string {
	b := pk.Serialize()
	return hex.EncodeToString(b[:])
}

// signAttestationDirect signs an attestation using the raw BLS key, bypassing
// TssManager.config (which needs the full config infrastructure).
func signAttestationDirect(sk *blsu.SecretKey, account string, targetBlock uint64) (*ReadyAttestation, error) {
	c, err := attestationCID(account, targetBlock)
	if err != nil {
		return nil, err
	}
	sig := blsu.Sign(sk, c.Bytes())
	sigBytes := sig.Serialize()

	return &ReadyAttestation{
		Account:     account,
		TargetBlock: targetBlock,
		Sig:         base64.URLEncoding.EncodeToString(sigBytes[:]),
	}, nil
}

// verifyAttestationDirect verifies an attestation using a raw BLS pubkey,
// bypassing BlsDID parsing (which needs real did:key format).
func verifyAttestationDirect(att ReadyAttestation, pk *blsu.Pubkey) bool {
	c, err := attestationCID(att.Account, att.TargetBlock)
	if err != nil {
		return false
	}
	sigBytes, err := base64.URLEncoding.DecodeString(att.Sig)
	if err != nil || len(sigBytes) != 96 {
		return false
	}
	sig := new(blsu.Signature)
	sig.Deserialize((*[96]byte)(sigBytes))
	return blsu.Verify(pk, c.Bytes(), sig)
}

func TestAttestationSignVerifyRoundTrip(t *testing.T) {
	sk, pk := generateTestBLSKey(42)

	att, err := signAttestationDirect(&sk, "alice", 500)
	if err != nil {
		t.Fatalf("signAttestationDirect failed: %v", err)
	}
	if att.Account != "alice" || att.TargetBlock != 500 {
		t.Fatalf("attestation fields mismatch: %+v", att)
	}

	if !verifyAttestationDirect(*att, &pk) {
		t.Fatal("verifyAttestationDirect returned false for valid attestation")
	}

	// Tamper with account — should fail
	tampered := *att
	tampered.Account = "bob"
	if verifyAttestationDirect(tampered, &pk) {
		t.Fatal("should reject tampered account")
	}

	// Tamper with target block — should fail
	tampered2 := *att
	tampered2.TargetBlock = 501
	if verifyAttestationDirect(tampered2, &pk) {
		t.Fatal("should reject tampered target block")
	}
}

func TestAttestationRejectsWrongKey(t *testing.T) {
	sk1, _ := generateTestBLSKey(42)
	_, pk2 := generateTestBLSKey(99)

	att, err := signAttestationDirect(&sk1, "alice", 500)
	if err != nil {
		t.Fatal(err)
	}

	// Verify with wrong public key — should fail
	if verifyAttestationDirect(*att, &pk2) {
		t.Fatal("should reject attestation verified with wrong key")
	}
}

func TestAttestationCIDDeterministic(t *testing.T) {
	c1, err := attestationCID("alice", 500)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := attestationCID("alice", 500)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("CIDs should be identical: %s != %s", c1, c2)
	}

	// Different inputs must produce different CIDs
	c3, _ := attestationCID("bob", 500)
	if c1 == c3 {
		t.Fatal("different account should produce different CID")
	}
	c5, _ := attestationCID("alice", 501)
	if c1 == c5 {
		t.Fatal("different block should produce different CID")
	}
}

func TestVerifyAttestationViaElection(t *testing.T) {
	// Test the full verifyAttestation path that uses election member lookup.
	sk, pk := generateTestBLSKey(42)

	att, _ := signAttestationDirect(&sk, "alice", 500)

	mgr := &TssManager{}

	// Create a proper BlsDID from the public key
	blsDID := testBlsDIDFromPubKey(pk)

	election := elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "alice", Key: string(blsDID)},
				{Account: "bob", Key: string(blsDID)},
			},
		},
	}

	if !mgr.verifyAttestation(*att, election) {
		t.Fatal("verifyAttestation should accept valid attestation with matching election member")
	}

	// Non-member should be rejected
	election2 := elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "bob", Key: string(blsDID)},
			},
		},
	}
	if mgr.verifyAttestation(*att, election2) {
		t.Fatal("verifyAttestation should reject attestation from non-member")
	}
}

// testBlsDIDFromPubKey creates a BlsDID string from a raw pubkey using the
// same encoding that the real system uses.
func testBlsDIDFromPubKey(pk blsu.Pubkey) string {
	compressed := pk.Serialize()
	// The BLS DID format is did:key:z<multibase-btc-encoded-multicodec-prefixed-key>
	// The multicodec prefix for BLS12-381 G1 public key is 0xea01
	prefix := []byte{0xea, 0x01}
	data := append(prefix, compressed[:]...)
	// Multibase base58btc encoding (z prefix)
	encoded := base58Encode(data)
	return "did:key:z" + encoded
}

// base58Encode is a minimal base58btc encoder for test DID construction.
func base58Encode(data []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	// Convert bytes to big integer
	x := new(big.Int).SetBytes(data)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for x.Cmp(zero) > 0 {
		x.DivMod(x, base, mod)
		result = append(result, alphabet[mod.Int64()])
	}
	// Leading zeros
	for _, b := range data {
		if b != 0 {
			break
		}
		result = append(result, alphabet[0])
	}
	// Reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

func TestGossipStateAccumulatesAttestations(t *testing.T) {
	mgr := &TssManager{
		gossipAttestations: make(map[string]map[string]ReadyAttestation),
	}

	dedupKey := "500"

	mgr.gossipLock.Lock()
	mgr.gossipAttestations[dedupKey] = make(map[string]ReadyAttestation)
	mgr.gossipAttestations[dedupKey]["alice"] = ReadyAttestation{Account: "alice", TargetBlock: 500, Sig: "a"}
	mgr.gossipAttestations[dedupKey]["bob"] = ReadyAttestation{Account: "bob", TargetBlock: 500, Sig: "b"}
	mgr.gossipAttestations[dedupKey]["carol"] = ReadyAttestation{Account: "carol", TargetBlock: 500, Sig: "c"}
	mgr.gossipLock.Unlock()

	mgr.gossipLock.RLock()
	attMap := mgr.gossipAttestations[dedupKey]
	readyAccounts := make(map[string]bool, len(attMap))
	for account := range attMap {
		readyAccounts[account] = true
	}
	mgr.gossipLock.RUnlock()

	if len(readyAccounts) != 3 {
		t.Fatalf("expected 3 ready accounts, got %d", len(readyAccounts))
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		if !readyAccounts[name] {
			t.Fatalf("expected %s in ready set", name)
		}
	}
}

func TestGossipCleanupEvictsOldEntries(t *testing.T) {
	mgr := &TssManager{
		gossipAttestations: make(map[string]map[string]ReadyAttestation),
	}

	mgr.gossipAttestations["100"] = map[string]ReadyAttestation{
		"alice": {Account: "alice"},
	}
	mgr.gossipAttestations["200"] = map[string]ReadyAttestation{
		"bob": {Account: "bob"},
	}

	mgr.cleanupGossipState(150)

	if _, exists := mgr.gossipAttestations["100"]; exists {
		t.Fatal("expected block 100 entry to be evicted")
	}
	if _, exists := mgr.gossipAttestations["200"]; !exists {
		t.Fatal("expected block 200 entry to be kept")
	}
}

func TestGossipReadySetDeterministicFromSameAttestations(t *testing.T) {
	buildReadySet := func(attestations map[string]ReadyAttestation) []string {
		accounts := make([]string, 0, len(attestations))
		for account := range attestations {
			accounts = append(accounts, account)
		}
		sort.Strings(accounts)
		return accounts
	}

	attSet := map[string]ReadyAttestation{
		"carol": {Account: "carol"},
		"alice": {Account: "alice"},
		"bob":   {Account: "bob"},
	}

	set1 := buildReadySet(attSet)
	set2 := buildReadySet(attSet)

	if fmt.Sprintf("%v", set1) != fmt.Sprintf("%v", set2) {
		t.Fatalf("ready sets should be identical: %v != %v", set1, set2)
	}
	if set1[0] != "alice" || set1[1] != "bob" || set1[2] != "carol" {
		t.Fatalf("expected alphabetical order, got %v", set1)
	}
}

func TestPerHeightGossipSharedAcrossKeys(t *testing.T) {
	// Verify that a single per-height gossip entry is usable for multiple keys.
	mgr := &TssManager{
		gossipAttestations: make(map[string]map[string]ReadyAttestation),
	}

	targetBlock := uint64(500)
	heightKey := strconv.FormatUint(targetBlock, 10)

	mgr.gossipAttestations[heightKey] = map[string]ReadyAttestation{
		"alice": {Account: "alice", TargetBlock: targetBlock},
		"bob":   {Account: "bob", TargetBlock: targetBlock},
	}

	// Both key1 and key2 should see the same ready set from one gossip entry
	attMap := mgr.gossipAttestations[heightKey]
	readyAccounts := make(map[string]bool, len(attMap))
	for account := range attMap {
		readyAccounts[account] = true
	}

	if !readyAccounts["alice"] || !readyAccounts["bob"] {
		t.Fatal("expected both alice and bob in ready set")
	}
	if len(readyAccounts) != 2 {
		t.Fatalf("expected 2 ready accounts, got %d", len(readyAccounts))
	}
}
