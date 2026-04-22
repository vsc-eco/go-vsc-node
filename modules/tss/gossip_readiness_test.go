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

// TestSignWindowGossipTarget_BeforeWindow verifies that no target is primed
// before the readiness window opens (i.e. when bh is more than readinessOffset
// blocks away from the next signInterval boundary).
func TestSignWindowGossipTarget_BeforeWindow(t *testing.T) {
	// signInterval=50, readinessOffset=30, bh=2000 (bh%50=0)
	// blocksUntilSign=50, windowStart=2050, 50 > 30 → not in window.
	if _, ok := signWindowGossipTarget(2000, 50, 30); ok {
		t.Fatal("expected no target before readiness window")
	}
}

// TestSignWindowGossipTarget_WindowEntry verifies the windowStart is primed at
// the moment the readiness window opens.
func TestSignWindowGossipTarget_WindowEntry(t *testing.T) {
	// bh=2020 (bh%50=20), blocksUntilSign=30, windowStart=2050, 30 ≤ 30 → in.
	got, ok := signWindowGossipTarget(2020, 50, 30)
	if !ok || got != 2050 {
		t.Fatalf("want (2050, true), got (%d, %v)", got, ok)
	}
}

// TestSignWindowGossipTarget_DeepInWindow verifies that a single windowStart
// is returned even when bh is well inside the readiness window.
func TestSignWindowGossipTarget_DeepInWindow(t *testing.T) {
	// bh=2045 (bh%50=45), blocksUntilSign=5, windowStart=2050.
	got, ok := signWindowGossipTarget(2045, 50, 30)
	if !ok || got != 2050 {
		t.Fatalf("want (2050, true), got (%d, %v)", got, ok)
	}
}

// TestSignWindowGossipTarget_AtBoundary verifies that at bh = signInterval
// boundary, priming is deferred: the next windowStart is a full signInterval
// away which is outside the readiness offset.
func TestSignWindowGossipTarget_AtBoundary(t *testing.T) {
	// bh=2050, bh%50=0, blocksUntilSign=50, windowStart=2100. 50 > 30.
	if _, ok := signWindowGossipTarget(2050, 50, 30); ok {
		t.Fatal("expected no target at boundary (next window outside offset)")
	}
}

// TestSignWindowGossipTarget_MidWindowDispatch verifies that at a staggered
// dispatch block (e.g. bh=2075), priming targets the *next* window.
func TestSignWindowGossipTarget_MidWindowDispatch(t *testing.T) {
	// bh=2075 (bh%50=25), blocksUntilSign=25, windowStart=2100, 25 ≤ 30.
	got, ok := signWindowGossipTarget(2075, 50, 30)
	if !ok || got != 2100 {
		t.Fatalf("want (2100, true), got (%d, %v)", got, ok)
	}
}

// TestSignWindowGossipTarget_ConvergesOverTicks walks the tick range from
// before window entry through window close and asserts that exactly one
// windowStart is returned across all in-range ticks (no slot fragmentation).
func TestSignWindowGossipTarget_ConvergesOverTicks(t *testing.T) {
	const signInterval uint64 = 50
	const readinessOffset uint64 = 30
	primed := map[uint64]bool{}
	for bh := uint64(2019); bh <= 2049; bh++ {
		if target, ok := signWindowGossipTarget(bh, signInterval, readinessOffset); ok {
			primed[target] = true
		}
	}
	// Exactly one windowStart should be primed for the whole 30-block window.
	if len(primed) != 1 || !primed[2050] {
		t.Fatalf("expected exactly {2050}, got %v", primed)
	}
}

// TestSignWindow_AllSlotsResolveToSameKey confirms that the sign pre-flight
// gate rounds bh down to windowStart so every staggered slot in the window
// resolves to the same gossip key and returns the same ready set.
func TestSignWindow_AllSlotsResolveToSameKey(t *testing.T) {
	const signInterval uint64 = 50
	windowStart := uint64(2050)
	mgr := &TssManager{
		gossipAttestations: make(map[string]map[string]ReadyAttestation),
	}
	key := strconv.FormatUint(windowStart, 10)
	mgr.gossipAttestations[key] = map[string]ReadyAttestation{
		"alice": {Account: "alice", TargetBlock: windowStart},
		"bob":   {Account: "bob", TargetBlock: windowStart},
		"carol": {Account: "carol", TargetBlock: windowStart},
	}

	for slot := uint64(0); slot < uint64(signStaggerCount); slot++ {
		bh := windowStart + slot*signStaggerStep
		lookup := bh - (bh % signInterval)
		attMap := mgr.gossipAttestations[strconv.FormatUint(lookup, 10)]
		if len(attMap) != 3 {
			t.Fatalf("slot %d (bh=%d): expected 3 ready accounts via windowStart key, got %d",
				slot, bh, len(attMap))
		}
	}
}

// TestSignWindow_MixedVersionDetectorSkipsNonSlotZero exercises the rollout
// detector: when an old-code node has emitted a per-block attestation for a
// non-slot-0 offset within the current window, new-code dispatchers at that
// slot must skip to avoid stalling a session with cohort-local party lists.
func TestSignWindow_MixedVersionDetectorSkipsNonSlotZero(t *testing.T) {
	const signInterval uint64 = 50
	windowStart := uint64(2050)
	mgr := &TssManager{
		gossipAttestations: make(map[string]map[string]ReadyAttestation),
	}
	// New-code node's windowStart attestation.
	mgr.gossipAttestations[strconv.FormatUint(windowStart, 10)] = map[string]ReadyAttestation{
		"new-node": {Account: "new-node", TargetBlock: windowStart},
	}
	// Old-code node's per-slot attestation for slot 1.
	slot1Block := windowStart + signStaggerStep
	mgr.gossipAttestations[strconv.FormatUint(slot1Block, 10)] = map[string]ReadyAttestation{
		"old-node": {Account: "old-node", TargetBlock: slot1Block},
	}

	isMixed := func(bh uint64) bool {
		wstart := bh - (bh % signInterval)
		if bh == wstart {
			return false
		}
		for offset := uint64(1); offset < uint64(signStaggerCount); offset++ {
			k := strconv.FormatUint(wstart+offset*signStaggerStep, 10)
			if len(mgr.gossipAttestations[k]) > 0 {
				return true
			}
		}
		return false
	}

	if isMixed(windowStart) {
		t.Fatal("slot 0 must not trip the detector (it always works cross-cohort)")
	}
	if !isMixed(windowStart + signStaggerStep) {
		t.Fatal("slot 1 must trip the detector when old-code slot-1 entry is present")
	}
	if !isMixed(windowStart + 5*signStaggerStep) {
		t.Fatal("slot 5 must trip the detector as long as any later-slot key exists")
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
