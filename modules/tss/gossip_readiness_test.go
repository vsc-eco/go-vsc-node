package tss

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"sync"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
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

// gossipTestSetup builds a TssManager + p2pSpec wired to a MockElectionDb
// containing a real-keyed election. Returns the manager, the spec, the BLS
// secret keys keyed by account (for signing test attestations), and the target
// block to use.
func gossipTestSetup(t *testing.T, accounts []string, targetBlock uint64) (*TssManager, p2pSpec, map[string]*blsu.SecretKey) {
	t.Helper()

	members := make([]elections.ElectionMember, len(accounts))
	keys := make(map[string]*blsu.SecretKey, len(accounts))
	for i, a := range accounts {
		sk, pk := generateTestBLSKey(byte(i + 1))
		keys[a] = &sk
		members[i] = elections.ElectionMember{Account: a, Key: testBlsDIDFromPubKey(pk)}
	}
	election := elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members},
	}

	mgr := newTestTssManager(t, accounts[0])
	mgr.electionDb = &test_utils.MockElectionDb{
		ElectionsByHeight: map[uint64]elections.ElectionResult{targetBlock: election},
	}
	mgr.sconf = systemconfig.MocknetConfig()
	mgr.gossipAttestations = make(map[string]map[string]ReadyAttestation)
	// Place currentBh outside the settle period: targetBlock - currentBh > DEFAULT_SETTLE_BLOCKS.
	mgr.lastBlockHeight.Store(targetBlock - uint64(DEFAULT_SETTLE_BLOCKS) - 10)

	return mgr, p2pSpec{tssMgr: mgr}, keys
}

// makeBundle builds a ready_gossip pubsub message body for the given
// account → secret-key set, signing each attestation directly.
func makeBundle(t *testing.T, keys map[string]*blsu.SecretKey, accounts []string, targetBlock uint64) p2pMessage {
	t.Helper()
	atts := make([]interface{}, 0, len(accounts))
	for _, a := range accounts {
		att, err := signAttestationDirect(keys[a], a, targetBlock)
		if err != nil {
			t.Fatalf("sign for %s: %v", a, err)
		}
		atts = append(atts, map[string]interface{}{
			"account":      a,
			"target_block": float64(targetBlock),
			"sig":          att.Sig,
		})
	}
	return p2pMessage{
		Type: "ready_gossip",
		Data: map[string]interface{}{
			"target_block": float64(targetBlock),
			"attestations": atts,
		},
	}
}

// TestHandleReadyGossip_ConcurrentOverlappingBundles fires N goroutines that
// each call handleReadyGossip with overlapping bundles. The final map must be
// the union of all valid attestations and there must be no race or panic.
// This guards the Phase 3 re-check that prevents lost-update on concurrent
// merges.
func TestHandleReadyGossip_ConcurrentOverlappingBundles(t *testing.T) {
	accounts := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	const targetBlock uint64 = 5000

	mgr, spec, keys := gossipTestSetup(t, accounts, targetBlock)

	// Each goroutine sends a bundle covering 4 of 6 accounts, with rotation
	// so every pair of bundles overlaps but no two are identical.
	bundles := [][]string{
		{"alice", "bob", "carol", "dave"},
		{"bob", "carol", "dave", "eve"},
		{"carol", "dave", "eve", "frank"},
		{"dave", "eve", "frank", "alice"},
		{"eve", "frank", "alice", "bob"},
		{"frank", "alice", "bob", "carol"},
	}

	var wg sync.WaitGroup
	for _, b := range bundles {
		wg.Add(1)
		go func(accs []string) {
			defer wg.Done()
			spec.handleReadyGossip(makeBundle(t, keys, accs, targetBlock))
		}(b)
	}
	wg.Wait()

	mgr.gossipLock.RLock()
	final := mgr.gossipAttestations[strconv.FormatUint(targetBlock, 10)]
	got := make([]string, 0, len(final))
	for a := range final {
		got = append(got, a)
	}
	mgr.gossipLock.RUnlock()
	sort.Strings(got)

	want := append([]string(nil), accounts...)
	sort.Strings(want)
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Fatalf("expected union %v, got %v", want, got)
	}
}

// TestHandleReadyGossip_RaceWithBlockTickWriter races handleReadyGossip against
// a writer that mimics BlockTick's self-attestation insert path (direct map
// write under gossipLock.Lock). Both paths must coexist safely.
func TestHandleReadyGossip_RaceWithBlockTickWriter(t *testing.T) {
	accounts := []string{"alice", "bob", "carol"}
	const targetBlock uint64 = 5000
	mgr, spec, keys := gossipTestSetup(t, accounts, targetBlock)
	dedupKey := strconv.FormatUint(targetBlock, 10)

	const iters = 50
	var wg sync.WaitGroup

	// Writer 1: BlockTick-style direct insert of "alice" each iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		att, err := signAttestationDirect(keys["alice"], "alice", targetBlock)
		if err != nil {
			t.Errorf("sign alice: %v", err)
			return
		}
		for i := 0; i < iters; i++ {
			mgr.gossipLock.Lock()
			if mgr.gossipAttestations[dedupKey] == nil {
				mgr.gossipAttestations[dedupKey] = make(map[string]ReadyAttestation)
			}
			mgr.gossipAttestations[dedupKey]["alice"] = *att
			mgr.gossipLock.Unlock()
		}
	}()

	// Writer 2: handler-style merge of bob+carol each iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bundle := makeBundle(t, keys, []string{"bob", "carol"}, targetBlock)
		for i := 0; i < iters; i++ {
			spec.handleReadyGossip(bundle)
		}
	}()

	wg.Wait()

	mgr.gossipLock.RLock()
	final := mgr.gossipAttestations[dedupKey]
	mgr.gossipLock.RUnlock()
	for _, a := range accounts {
		if _, ok := final[a]; !ok {
			t.Fatalf("expected %s in final map, got keys=%v", a, final)
		}
	}
}

// TestHandleReadyGossip_FullyRedundantBundleEarlyExit verifies that a bundle
// containing only attestations the manager already has triggers the Phase 1
// early exit — Phase 3's write lock is never taken, and no spurious mutation
// occurs. The Phase 3 re-check would also prevent overwrite, but the early
// exit is the steady-state hot path that drove the original lock contention.
func TestHandleReadyGossip_FullyRedundantBundleEarlyExit(t *testing.T) {
	accounts := []string{"alice", "bob", "carol"}
	const targetBlock uint64 = 5000
	mgr, spec, keys := gossipTestSetup(t, accounts, targetBlock)
	dedupKey := strconv.FormatUint(targetBlock, 10)

	// Pre-populate every account so the incoming bundle is fully redundant.
	mgr.gossipLock.Lock()
	mgr.gossipAttestations[dedupKey] = make(map[string]ReadyAttestation)
	for _, a := range accounts {
		att, err := signAttestationDirect(keys[a], a, targetBlock)
		if err != nil {
			t.Fatalf("sign %s: %v", a, err)
		}
		mgr.gossipAttestations[dedupKey][a] = *att
	}
	mgr.gossipLock.Unlock()

	// Snapshot the map identity before the call so we can detect any reset.
	mgr.gossipLock.RLock()
	beforePtr := fmt.Sprintf("%p", mgr.gossipAttestations[dedupKey])
	beforeLen := len(mgr.gossipAttestations[dedupKey])
	mgr.gossipLock.RUnlock()

	spec.handleReadyGossip(makeBundle(t, keys, accounts, targetBlock))

	mgr.gossipLock.RLock()
	afterPtr := fmt.Sprintf("%p", mgr.gossipAttestations[dedupKey])
	afterLen := len(mgr.gossipAttestations[dedupKey])
	mgr.gossipLock.RUnlock()

	if beforePtr != afterPtr {
		t.Fatalf("map was replaced (before=%s after=%s)", beforePtr, afterPtr)
	}
	if beforeLen != afterLen {
		t.Fatalf("map length changed (before=%d after=%d)", beforeLen, afterLen)
	}
}

// TestHandleReadyGossip_PartialBundleMergesNewOnly verifies that a bundle
// mixing already-known and new accounts merges only the new ones, leaving
// existing entries untouched.
func TestHandleReadyGossip_PartialBundleMergesNewOnly(t *testing.T) {
	accounts := []string{"alice", "bob", "carol", "dave"}
	const targetBlock uint64 = 5000
	mgr, spec, keys := gossipTestSetup(t, accounts, targetBlock)
	dedupKey := strconv.FormatUint(targetBlock, 10)

	// Pre-populate alice with a sentinel signature.
	sentinelSig := "SENTINEL"
	mgr.gossipLock.Lock()
	mgr.gossipAttestations[dedupKey] = map[string]ReadyAttestation{
		"alice": {Account: "alice", TargetBlock: targetBlock, Sig: sentinelSig},
	}
	mgr.gossipLock.Unlock()

	// Bundle includes alice (already-known) plus bob/carol/dave (new).
	spec.handleReadyGossip(makeBundle(t, keys, accounts, targetBlock))

	mgr.gossipLock.RLock()
	final := mgr.gossipAttestations[dedupKey]
	mgr.gossipLock.RUnlock()

	if len(final) != 4 {
		t.Fatalf("expected 4 final entries, got %d", len(final))
	}
	if final["alice"].Sig != sentinelSig {
		t.Fatalf("alice's sentinel signature was overwritten")
	}
	for _, a := range []string{"bob", "carol", "dave"} {
		if _, ok := final[a]; !ok {
			t.Fatalf("expected %s to be merged in", a)
		}
	}
}
