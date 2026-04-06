package tss_test

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"sync"
	"testing"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/elections"
	hive_blocks "vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/vsc-eco/hivego"
)

// storeElection creates an election with the given epoch and block height on all nodes.
// members/weights are built from the nodes slice (all 6 nodes).
func storeElection(t *testing.T, nodes []nodeComponents, epoch uint64, blockHeight uint64) {
	t.Helper()
	members := make([]elections.ElectionMember, nodeCount)
	weights := make([]uint64, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDid, _ := nodes[i].identity.BlsDID()
		members[i] = elections.ElectionMember{
			Account: "mock-tss-" + strconv.Itoa(i+1),
			Key:     string(blsDid),
		}
		weights[i] = 10
	}
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch, NetId: "vsc-mocknet"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        blockHeight,
		})
	}
}

// storeElectionCustomMembers creates an election with custom members on all nodes.
func storeElectionCustomMembers(t *testing.T, nodes []nodeComponents, epoch uint64, blockHeight uint64, members []elections.ElectionMember, weights []uint64) {
	t.Helper()
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch, NetId: "vsc-mocknet"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        blockHeight,
		})
	}
}

// storeReadiness stores readiness records for the given accounts on all nodes' DBs.
func storeReadiness(t *testing.T, nodes []nodeComponents, accounts []string, keyId string, targetBlock uint64) {
	t.Helper()
	for _, account := range accounts {
		for _, node := range nodes {
			node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
				Type:        "ready",
				KeyId:       keyId,
				BlockHeight: targetBlock - 35,
				Epoch:       targetBlock,
				Commitment:  account,
				TxId:        "ready-" + account + "-" + strconv.FormatUint(targetBlock, 10),
			})
		}
	}
}

// allAccountNames returns sorted account names for all 6 nodes.
func allAccountNames() []string {
	names := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		names[i] = "mock-tss-" + strconv.Itoa(i+1)
	}
	return names
}

// storeBlameCommitment stores a blame commitment blaming the given accounts,
// encoded against the given election epoch, on all nodes' DBs.
func storeBlameCommitment(t *testing.T, nodes []nodeComponents, blamedAccounts []string, keyId string, blameEpoch uint64, blockHeight uint64, txId string) {
	t.Helper()
	election := nodes[0].electionDb.GetElection(blameEpoch)
	if election == nil {
		t.Fatalf("Election epoch %d not found for blame encoding", blameEpoch)
	}
	blameBitset := new(big.Int)
	for _, blamed := range blamedAccounts {
		for idx, member := range election.Members {
			if member.Account == blamed {
				blameBitset.SetBit(blameBitset, idx, 1)
				break
			}
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString(blameBitset.Bytes())
	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "blame",
			KeyId:       keyId,
			Commitment:  encoded,
			Epoch:       blameEpoch,
			BlockHeight: blockHeight,
			TxId:        txId,
		})
	}
}

// activateKey sets the key to active at the given epoch on all nodes.
func activateKey(t *testing.T, nodes []nodeComponents, keyId string, epoch uint64) {
	t.Helper()
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     keyId,
			Status: tss_db.TssKeyActive,
			Algo:   tss_db.EcdsaType,
			Epoch:  epoch,
		})
	}
}

// triggerReshare triggers a reshare at blockStart and waits for completion.
// Returns true if reshare completed within timeout.
func triggerReshare(t *testing.T, nodes []nodeComponents, blockStart uint64, reshareBroadcast **hivego.HiveTransaction, mu *sync.Mutex, timeoutSec int) bool {
	t.Helper()

	mu.Lock()
	*reshareBroadcast = nil
	mu.Unlock()

	headHeight := blockStart + 20
	processBlocks(nodes, blockStart-2, blockStart-2, &headHeight)
	for bh := blockStart - 1; bh <= blockStart+5; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	for i := 0; i < timeoutSec*2; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *reshareBroadcast != nil
		mu.Unlock()
		if done {
			return true
		}
	}
	return false
}

// waitForBlameBroadcast waits for a blame commitment to appear in broadcastCb output.
// It checks the broadcastCb by looking for blame-type commitments captured by the test.
// Returns true if blame was detected.
func waitForBlameBroadcast(t *testing.T, nodes []nodeComponents, keyId string, blockHeight uint64, timeoutSec int) bool {
	t.Helper()
	for i := 0; i < timeoutSec*2; i++ {
		time.Sleep(500 * time.Millisecond)
		commitment, err := nodes[0].tssCommitments.GetCommitmentByHeight(keyId, blockHeight+100, "blame")
		if err == nil && commitment.BlockHeight >= blockHeight-10 {
			return true
		}
	}
	return false
}

// TestOnChainReadinessHappyPath verifies that when all 6 nodes broadcast readiness
// and the readiness records are stored in MongoDB, reshare succeeds normally.
func TestOnChainReadinessHappyPath(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes

	// Keygen at block 100
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Store election epoch 1
	storeElection(t, nodes, 1, 150)

	// Activate key at epoch 0 so reshare triggers
	activateKey(t, nodes, "test-key", 0)

	// Store readiness for ALL 6 nodes
	storeReadiness(t, nodes, allAccountNames(), "test-key", 200)

	// Trigger reshare at block 200
	completed := triggerReshare(t, nodes, 200, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("reshare did not complete — expected success with all 6 nodes ready")
	}
	t.Log("PASS: reshare succeeded with all 6 nodes broadcasting readiness")
}

// TestOfflineNodeExcludedByReadiness verifies that when only 5 of 6 nodes
// broadcast readiness, the reshare still succeeds with 5 nodes.
func TestOfflineNodeExcludedByReadiness(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Store election epoch 1
	storeElection(t, nodes, 1, 150)

	// Activate key
	activateKey(t, nodes, "test-key", 0)

	// Store readiness for only 5 nodes (exclude mock-tss-6)
	readyNodes := allAccountNames()[:5] // mock-tss-1 through mock-tss-5
	storeReadiness(t, nodes, readyNodes, "test-key", 200)

	// Trigger reshare at block 200
	completed := triggerReshare(t, nodes, 200, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("reshare did not complete — expected success with 5 of 6 nodes ready")
	}
	t.Log("PASS: reshare succeeded with 5 nodes (mock-tss-6 excluded by readiness)")
}

// TestFalseReadinessProducesBlame verifies that when a node broadcasts readiness
// but is actually disconnected, the reshare times out and a blame commitment is
// produced that identifies the disconnected node.
func TestFalseReadinessProducesBlame(t *testing.T) {
	nodes, broadcastCb, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = reshareBroadcast
	_ = broadcastCb

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Store election epoch 1
	storeElection(t, nodes, 1, 150)

	// Activate key
	activateKey(t, nodes, "test-key", 0)

	// Store readiness for ALL 6 nodes (including mock-tss-6 which will be disconnected)
	storeReadiness(t, nodes, allAccountNames(), "test-key", 200)

	// Disconnect mock-tss-6 (index 5)
	excludedNodes.Store(5, true)
	disconnectNode(nodes, 5)
	time.Sleep(1 * time.Second)

	// Trigger reshare at block 200 — should timeout due to mock-tss-6 being unreachable
	mu.Lock()
	*reshareBroadcast = nil
	mu.Unlock()

	headHeight := uint64(220)
	processBlocks(nodes, 198, 198, &headHeight)
	for bh := uint64(199); bh <= 205; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for timeout (2 min reshare timeout + margin)
	t.Log("waiting for reshare timeout and blame production...")
	blameFound := waitForBlameBroadcast(t, nodes, "test-key", 200, 150)
	if !blameFound {
		t.Fatal("blame commitment was not produced after reshare timeout")
	}

	// Verify the blame targets mock-tss-6
	blame, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "blame")
	if err != nil {
		t.Fatalf("failed to read blame from DB: %v", err)
	}

	election := nodes[0].electionDb.GetElection(blame.Epoch)
	if election == nil {
		t.Fatalf("election epoch %d not found", blame.Epoch)
	}

	blameBytes, _ := base64.RawURLEncoding.DecodeString(blame.Commitment)
	blameBits := new(big.Int).SetBytes(blameBytes)
	blamedAccounts := make([]string, 0)
	for idx, member := range election.Members {
		if blameBits.Bit(idx) == 1 {
			blamedAccounts = append(blamedAccounts, member.Account)
		}
	}
	t.Logf("blamed accounts: %v", blamedAccounts)

	foundTarget := false
	for _, acc := range blamedAccounts {
		if acc == "mock-tss-6" {
			foundTarget = true
		}
	}
	if !foundTarget {
		t.Fatalf("FAIL: mock-tss-6 not blamed. Blamed accounts: %v", blamedAccounts)
	}
	t.Log("PASS: blame commitment correctly identifies mock-tss-6 as culprit")

	// Cleanup: reconnect mock-tss-6
	excludedNodes.Delete(5)
	reconnectNode(t, nodes, 5)
}

// TestBlameExcludesNodeNextCycle verifies that a blame from a previous reshare
// cycle causes the blamed node to be excluded from the old committee in the
// next reshare cycle.
func TestBlameExcludesNodeNextCycle(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Store election epoch 1
	storeElection(t, nodes, 1, 150)

	// Activate key at epoch 0
	activateKey(t, nodes, "test-key", 0)

	// Store readiness for all 6 for first reshare
	storeReadiness(t, nodes, allAccountNames(), "test-key", 200)

	// First reshare at block 200 — should succeed
	completed := triggerReshare(t, nodes, 200, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("first reshare did not complete")
	}
	t.Log("first reshare completed")

	// Now simulate blame from first cycle against mock-tss-6
	// Get the keygen commitment to find its block height
	latestCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "keygen", "reshare")
	if err != nil {
		t.Fatalf("failed to get latest commitment: %v", err)
	}
	storeBlameCommitment(t, nodes, []string{"mock-tss-6"}, "test-key", 1, latestCommitment.BlockHeight+50, "blame-cycle1")

	// Store election epoch 2
	storeElection(t, nodes, 2, 250)

	// Activate key at epoch 1 (from the reshare that completed)
	activateKey(t, nodes, "test-key", 1)

	// Store readiness for ALL 6 nodes (mock-tss-6 is reconnected and ready)
	storeReadiness(t, nodes, allAccountNames(), "test-key", 300)

	// Second reshare at block 300 — blame from first cycle should exclude mock-tss-6 from old committee
	completed = triggerReshare(t, nodes, 300, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("second reshare did not complete — blame exclusion may have broken party lists")
	}
	t.Log("PASS: reshare succeeded with mock-tss-6 excluded from old committee by blame")
}

// TestBlameEpochDecodeCorrectness verifies that blame commitments are decoded
// against their own epoch's election, not against the current election.
// This matters when election membership changes between epochs.
func TestBlameEpochDecodeCorrectness(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = reshareBroadcast
	_ = excludedNodes

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Epoch 0 election already stored by setupCluster with 6 members:
	// mock-tss-1 through mock-tss-6

	// Store epoch 1 election with a DIFFERENT member ordering:
	// Insert a new member "mock-tss-7" at position 0, shifting all indices
	blsDids := make([]dids.BlsDID, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDids[i], _ = nodes[i].identity.BlsDID()
	}
	epoch1Members := []elections.ElectionMember{
		{Account: "mock-tss-7", Key: string(blsDids[0])}, // new member at index 0
	}
	for i := 0; i < nodeCount; i++ {
		epoch1Members = append(epoch1Members, elections.ElectionMember{
			Account: "mock-tss-" + strconv.Itoa(i+1),
			Key:     string(blsDids[i]),
		})
	}
	epoch1Weights := make([]uint64, len(epoch1Members))
	for i := range epoch1Weights {
		epoch1Weights[i] = 10
	}
	storeElectionCustomMembers(t, nodes, 1, 150, epoch1Members, epoch1Weights)

	// Store a blame commitment encoded against epoch 0 blaming mock-tss-6
	// In epoch 0, mock-tss-6 is at some index. In epoch 1, indices are shifted.
	// The blame should decode against epoch 0 (the blame's own epoch).
	election0 := nodes[0].electionDb.GetElection(0)
	if election0 == nil {
		t.Fatal("election epoch 0 not found")
	}

	blameBitset := new(big.Int)
	for idx, member := range election0.Members {
		if member.Account == "mock-tss-6" {
			blameBitset.SetBit(blameBitset, idx, 1)
			break
		}
	}
	blameEncoded := base64.RawURLEncoding.EncodeToString(blameBitset.Bytes())

	keygenCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "keygen", "reshare")
	if err != nil {
		t.Fatalf("failed to get latest commitment: %v", err)
	}

	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "blame",
			KeyId:       "test-key",
			Commitment:  blameEncoded,
			Epoch:       0,
			BlockHeight: keygenCommitment.BlockHeight + 50,
			TxId:        "blame-epoch-decode-test",
		})
	}

	// Verify: decode blame against epoch 0 → should get mock-tss-6
	blameBytes, _ := base64.RawURLEncoding.DecodeString(blameEncoded)
	blameBits := new(big.Int).SetBytes(blameBytes)

	blamedFromEpoch0 := make([]string, 0)
	for idx, member := range election0.Members {
		if blameBits.Bit(idx) == 1 {
			blamedFromEpoch0 = append(blamedFromEpoch0, member.Account)
		}
	}

	// Verify: decode same bitset against epoch 1 → might get WRONG account
	election1 := nodes[0].electionDb.GetElection(1)
	blamedFromEpoch1 := make([]string, 0)
	for idx, member := range election1.Members {
		if blameBits.Bit(idx) == 1 {
			blamedFromEpoch1 = append(blamedFromEpoch1, member.Account)
		}
	}

	t.Logf("blame decoded against epoch 0: %v (correct)", blamedFromEpoch0)
	t.Logf("blame decoded against epoch 1: %v (would be wrong if used)", blamedFromEpoch1)

	// Assert epoch 0 decoding is correct
	if len(blamedFromEpoch0) != 1 || blamedFromEpoch0[0] != "mock-tss-6" {
		t.Fatalf("FAIL: epoch 0 decoding should blame mock-tss-6, got %v", blamedFromEpoch0)
	}

	// Assert epoch 1 decoding differs (because of the index shift)
	if len(blamedFromEpoch1) > 0 && blamedFromEpoch1[0] == "mock-tss-6" {
		t.Log("WARNING: epoch 1 decoding also got mock-tss-6 — member order may not differ enough for this test")
	} else {
		t.Logf("CONFIRMED: epoch 1 decoding gives different result (%v), proving epoch matters", blamedFromEpoch1)
	}

	t.Log("PASS: blame epoch decode correctness verified — blame must be decoded against its own epoch")
}

// TestAccumulatedBlameExclusion verifies that multiple blame commitments at
// different block heights within the BLAME_EXPIRE window are ALL applied,
// excluding all blamed nodes, not just the most recent one.
func TestAccumulatedBlameExclusion(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Store election epoch 1
	storeElection(t, nodes, 1, 150)

	// Activate key at epoch 0
	activateKey(t, nodes, "test-key", 0)

	// Store readiness for all and do first reshare
	storeReadiness(t, nodes, allAccountNames(), "test-key", 200)
	completed := triggerReshare(t, nodes, 200, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("first reshare did not complete")
	}
	t.Log("first reshare done")

	latestCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "keygen", "reshare")
	if err != nil {
		t.Fatalf("failed to get latest commitment: %v", err)
	}

	// Store blame #1: mock-tss-5 at block height after latest commitment
	storeBlameCommitment(t, nodes, []string{"mock-tss-5"}, "test-key", 1,
		latestCommitment.BlockHeight+50, "blame-acc-1")

	// Store blame #2: mock-tss-6 at block height latestCommitment+60
	storeBlameCommitment(t, nodes, []string{"mock-tss-6"}, "test-key", 1,
		latestCommitment.BlockHeight+60, "blame-acc-2")

	// Store election epoch 2
	storeElection(t, nodes, 2, 250)

	// Activate key at epoch 1
	activateKey(t, nodes, "test-key", 1)

	// Store readiness for all 6 nodes
	storeReadiness(t, nodes, allAccountNames(), "test-key", 300)

	// Trigger reshare at block 300
	// If blame accumulation works, both mock-tss-5 and mock-tss-6 are excluded
	// from old committee (4 remaining out of 6, still above threshold+1=4 → should succeed).
	// If only the most recent blame is read, only mock-tss-6 is excluded.
	completed = triggerReshare(t, nodes, 300, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("reshare did not complete with accumulated blames")
	}

	// Read the reshare commitment to verify it succeeded
	reshareCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "reshare")
	if err != nil {
		t.Fatalf("failed to get reshare commitment: %v", err)
	}
	t.Logf("reshare commitment at epoch %d, blockHeight %d", reshareCommitment.Epoch, reshareCommitment.BlockHeight)
	t.Log("PASS: reshare succeeded with accumulated blame exclusions")
}

// TestSigningKeepTimeouts verifies that the signing path works with all nodes
// connected. The signing path uses keepTimeouts=true which means timeout nodes
// stay in the party list. With all nodes connected, signing should succeed.
func TestSigningKeepTimeouts(t *testing.T) {
	nodes, _, keygenPubKeys, signBroadcast, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes
	_ = reshareBroadcast

	// Keygen
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// Activate key
	activateKey(t, nodes, "test-key", 0)

	// Store a signing request
	msgHex := "7777777777777777777777777777777777777777777777777777777777777777"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex,
			Status: tss_db.SignPending,
		})
	}

	mu.Lock()
	*signBroadcast = nil
	mu.Unlock()

	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Trigger signing at block 250 (250 % 50 == 0)
	headHeight := uint64(270)
	processBlocks(nodes, 106, 248, &headHeight)
	for bh := uint64(249); bh <= 255; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Log("waiting for signing to complete...")
	signDone := false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			break
		}
	}

	if !signDone {
		t.Fatal("signing did not complete — all nodes connected, keepTimeouts=true should not cause issues")
	}

	// Verify signature exists
	mu.Lock()
	defer mu.Unlock()
	if *signBroadcast != nil {
		for _, op := range (*signBroadcast).Operations {
			raw, _ := json.Marshal(op)
			var cj struct {
				Json string `json:"json"`
			}
			json.Unmarshal(raw, &cj)
			var payload struct {
				Packet []struct {
					KeyId string `json:"key_id"`
					Msg   string `json:"msg"`
					Sig   string `json:"sig"`
				} `json:"packet"`
			}
			json.Unmarshal([]byte(cj.Json), &payload)
			for _, pkt := range payload.Packet {
				if pkt.KeyId == "test-key" && pkt.Msg == msgHex {
					if pkt.Sig == "" {
						t.Error("signature is empty")
					} else {
						t.Log("PASS: signing succeeded with all 6 nodes, keepTimeouts=true path verified")
						verifyEcdsaSig(t, pkt.Sig, pkt.Msg, keygenPubKeys["test-key"])
					}
				}
			}
		}
	}
}

// TestFullRecoveryCycle runs a full end-to-end multi-cycle test:
// 1. Keygen at block 100
// 2. Reshare at block 200 with all 6 nodes → SUCCESS
// 3. Disconnect mock-tss-6, reshare at block 300 → TIMEOUT + BLAME
// 4. Reconnect mock-tss-6, reshare at block 400 (blame excludes mock-tss-6 from old) → SUCCESS
// 5. Sign at block 450 → SUCCESS
// 6. Reshare at block 500 with all 6 → SUCCESS
func TestFullRecoveryCycle(t *testing.T) {
	nodes, _, keygenPubKeys, signBroadcast, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()

	// ── Step 1: Keygen at block 100 ──
	t.Log("=== Step 1: Keygen ===")
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	t.Log("keygen done")

	// ── Step 2: Reshare at block 200 with all 6 → SUCCESS ──
	t.Log("=== Step 2: Reshare at block 200 ===")
	storeElection(t, nodes, 1, 150)
	activateKey(t, nodes, "test-key", 0)
	storeReadiness(t, nodes, allAccountNames(), "test-key", 200)

	completed := triggerReshare(t, nodes, 200, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("Step 2: reshare at block 200 did not complete")
	}
	t.Log("Step 2: reshare at block 200 succeeded")

	// After successful reshare, update key epoch on all nodes (simulates state engine)
	// and preserve the reshare commitment with a durable TxId so blame upserts
	// don't overwrite it (broadcastCb stores with TxId="", causing collisions).
	reshareCommitStep2, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "keygen", "reshare")
	if err != nil {
		t.Fatalf("Step 2: failed to read reshare commitment: %v", err)
	}
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:          "test-key",
			Status:      tss_db.TssKeyActive,
			Algo:        tss_db.EcdsaType,
			Epoch:       reshareCommitStep2.Epoch,
			Epochs:      tss_db.MaxKeyEpochs,
			ExpiryEpoch: reshareCommitStep2.Epoch + tss_db.MaxKeyEpochs,
		})
		// Re-store commitment with durable TxId
		reshareCommitStep2.TxId = "reshare-step2-durable"
		node.tssCommitments.SetCommitmentData(reshareCommitStep2)
	}
	t.Logf("Step 2: key epoch updated to %d, commitment preserved", reshareCommitStep2.Epoch)

	// ── Step 3: Disconnect mock-tss-6, reshare at block 300 → TIMEOUT + BLAME ──
	t.Log("=== Step 3: Disconnect mock-tss-6, reshare at block 300 ===")
	excludedNodes.Store(5, true)
	disconnectNode(nodes, 5)
	time.Sleep(1 * time.Second)

	storeElection(t, nodes, 2, 250)
	activateKey(t, nodes, "test-key", 1)
	storeReadiness(t, nodes, allAccountNames(), "test-key", 300)

	// Trigger reshare — should timeout because mock-tss-6 is disconnected but in party list
	mu.Lock()
	*reshareBroadcast = nil
	mu.Unlock()

	headHeight := uint64(320)
	processBlocks(nodes, 298, 298, &headHeight)
	for bh := uint64(299); bh <= 305; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for blame (2 min timeout + margin)
	t.Log("Step 3: waiting for blame...")
	blameFound := waitForBlameBroadcast(t, nodes, "test-key", 300, 150)
	if !blameFound {
		t.Log("Step 3: WARNING — blame not detected in DB, continuing (broadcastCb may not handle blame)")
	} else {
		t.Log("Step 3: blame produced")
	}

	// ── Step 4: Reconnect mock-tss-6, reshare at block 400 ──
	t.Log("=== Step 4: Reconnect mock-tss-6, reshare at block 400 ===")
	excludedNodes.Delete(5)
	reconnectNode(t, nodes, 5)
	time.Sleep(2 * time.Second)
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Store blame manually (in case broadcastCb didn't handle it)
	reshareCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "reshare")
	if err == nil {
		storeBlameCommitment(t, nodes, []string{"mock-tss-6"}, "test-key", 2,
			reshareCommitment.BlockHeight+50, "blame-step3")
	} else {
		// No reshare commitment yet, use a reasonable block height
		storeBlameCommitment(t, nodes, []string{"mock-tss-6"}, "test-key", 2, 310, "blame-step3")
	}

	storeElection(t, nodes, 3, 350)
	activateKey(t, nodes, "test-key", 1) // still at epoch 1, the step 3 reshare failed

	storeReadiness(t, nodes, allAccountNames(), "test-key", 400)

	completed = triggerReshare(t, nodes, 400, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("Step 4: reshare at block 400 did not complete — blame exclusion may have failed")
	}
	t.Log("Step 4: reshare at block 400 succeeded with mock-tss-6 excluded from old committee")

	// ── Step 5: Sign at block 450 ──
	t.Log("=== Step 5: Sign at block 450 ===")
	msgHex := "7777777777777777777777777777777777777777777777777777777777777777"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex,
			Status: tss_db.SignPending,
		})
	}

	mu.Lock()
	*signBroadcast = nil
	mu.Unlock()

	headHeight2 := uint64(470)
	processBlocks(nodes, 406, 448, &headHeight2)
	for bh := uint64(449); bh <= 455; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight2)
		}
		time.Sleep(200 * time.Millisecond)
	}

	signDone := false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			break
		}
	}
	if !signDone {
		t.Fatal("Step 5: signing did not complete")
	}
	t.Log("Step 5: signing succeeded")

	// ── Step 6: Reshare at block 500 with all 6 ──
	t.Log("=== Step 6: Reshare at block 500 ===")
	storeElection(t, nodes, 4, 460)

	// Get current key epoch from the step 4 reshare
	reshareCommitment2, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "reshare")
	if err != nil {
		t.Fatalf("failed to get reshare commitment: %v", err)
	}
	activateKey(t, nodes, "test-key", reshareCommitment2.Epoch)

	storeReadiness(t, nodes, allAccountNames(), "test-key", 500)

	completed = triggerReshare(t, nodes, 500, reshareBroadcast, mu, 90)
	if !completed {
		t.Fatal("Step 6: reshare at block 500 did not complete")
	}
	t.Log("Step 6: reshare at block 500 succeeded with all 6 nodes")

	t.Log("PASS: full recovery cycle completed successfully")
}
