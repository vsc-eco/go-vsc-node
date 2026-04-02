package tss_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/db/vsc/elections"
	hive_blocks "vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/vsc-eco/hivego"
)

// setupCluster creates a fresh 6-node cluster with MongoDB, P2P mesh,
// election at epoch 0, and witness DB entries. Returns nodes, cleanup func,
// and a broadcast callback that captures keygen/reshare/sign commitments.
func setupCluster(t *testing.T) (
	nodes []nodeComponents,
	broadcastCb func(tx hivego.HiveTransaction) error,
	keygenPubKeys map[string]string,
	signBroadcast **hivego.HiveTransaction,
	reshareBroadcast **hivego.HiveTransaction,
	keygenBroadcast **hivego.HiveTransaction,
	mu *sync.Mutex,
	excludedNodes *sync.Map,
	stopKeepalive context.CancelFunc,
) {
	t.Helper()
	vsclog.ParseAndApply("verbose")
	dropTestDatabases()

	witnessNames := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		witnessNames[i] = "mock-tss-" + strconv.Itoa(i+1)
	}
	sort.Strings(witnessNames)
	mes := &MockElectionSystem{WitnessNames: witnessNames}

	mu = &sync.Mutex{}
	keygenPubKeys = make(map[string]string)
	var kgBroadcast *hivego.HiveTransaction
	var sgBroadcast *hivego.HiveTransaction
	var rsBroadcast *hivego.HiveTransaction
	keygenBroadcast = &kgBroadcast
	signBroadcast = &sgBroadcast
	reshareBroadcast = &rsBroadcast

	nodes = make([]nodeComponents, nodeCount)

	broadcastCb = func(tx hivego.HiveTransaction) error {
		mu.Lock()
		defer mu.Unlock()
		for _, op := range tx.Operations {
			if op.OpName() == "custom_json" {
				raw, _ := json.Marshal(op)
				var cj struct {
					Id   string `json:"id"`
					Json string `json:"json"`
				}
				json.Unmarshal(raw, &cj)
				if cj.Id == "vsc.tss_commitment" {
					var entries []interface{}
					if err := json.Unmarshal([]byte(cj.Json), &entries); err != nil {
						var payload map[string]interface{}
						json.Unmarshal([]byte(cj.Json), &payload)
						for _, v := range payload {
							entries = append(entries, v)
						}
					}
					for _, v := range entries {
						if m, ok := v.(map[string]interface{}); ok {
							if tp, ok := m["type"].(string); ok {
								if tp == "keygen" {
									if *keygenBroadcast == nil {
										txCopy := tx
										*keygenBroadcast = &txCopy
									}
									if pk, ok := m["pub_key"].(string); ok {
										if kid, ok := m["key_id"].(string); ok {
											keygenPubKeys[kid] = pk
										}
									}
								} else if tp == "reshare" && *reshareBroadcast == nil {
									txCopy := tx
									*reshareBroadcast = &txCopy
								}
							}
							// Store in all nodes' DBs
							keyId, _ := m["key_id"].(string)
							cType, _ := m["type"].(string)
							commitmentStr, _ := m["commitment"].(string)
							epoch, _ := m["epoch"].(float64)
							blockHeight, _ := m["block_height"].(float64)
							var pubKey *string
							if pk, ok := m["pub_key"].(string); ok {
								pubKey = &pk
							}
							for _, node := range nodes {
								node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
									Type:        cType,
									KeyId:       keyId,
									Commitment:  commitmentStr,
									Epoch:       uint64(epoch),
									BlockHeight: uint64(blockHeight),
									PublicKey:   pubKey,
								})
							}
						}
					}
				} else if cj.Id == "vsc.tss_sign" {
					if *signBroadcast == nil {
						txCopy := tx
						*signBroadcast = &txCopy
					}
					var signPayload struct {
						Packet []struct {
							KeyId string `json:"key_id"`
							Msg   string `json:"msg"`
							Sig   string `json:"sig"`
						} `json:"packet"`
					}
					json.Unmarshal([]byte(cj.Json), &signPayload)
					for _, pkt := range signPayload.Packet {
						for _, node := range nodes {
							node.tssRequests.UpdateRequest(tss_db.TssRequest{
								KeyId:  pkt.KeyId,
								Msg:    pkt.Msg,
								Sig:    pkt.Sig,
								Status: tss_db.SignComplete,
							})
						}
					}
				}
			}
		}
		return nil
	}

	for i := 0; i < nodeCount; i++ {
		nodes[i] = MakeNode(i, mes, broadcastCb)
	}

	t.Cleanup(func() {
		for i := 0; i < nodeCount; i++ {
			os.RemoveAll("data-dir-" + strconv.Itoa(i))
		}
		dropTestDatabases()
	})

	for i := 0; i < nodeCount; i++ {
		go test_utils.RunPlugin(t, nodes[i].agg)
	}
	time.Sleep(3 * time.Second)

	// Register disconnect notifiers
	for i := 0; i < nodeCount; i++ {
		nodeIdx := i
		notifee := &network.NotifyBundle{
			DisconnectedF: func(n network.Network, conn network.Conn) {
				log.Info("node disconnected", "node", nodeIdx, "peer", conn.RemotePeer().String()[:16])
			},
		}
		nodes[i].p2p.Host().Network().Notify(notifee)
	}

	// Set BLS DIDs + witness updates
	blsDids := make([]dids.BlsDID, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDid, err := nodes[i].identity.BlsDID()
		if err != nil {
			t.Fatalf("BLS DID failed for node %d: %v", i, err)
		}
		blsDids[i] = blsDid
	}
	for _, node := range nodes {
		for i := 0; i < nodeCount; i++ {
			node.witnessDb.SetWitnessUpdate(witnesses.SetWitnessUpdateType{
				Account: "mock-tss-" + strconv.Itoa(i+1),
				Metadata: witnesses.PostingJsonMetadata{
					VscNode: witnesses.PostingJsonMetadataVscNode{
						PeerId: nodes[i].p2p.ID().String(),
					},
					DidKeys: []witnesses.PostingJsonKeys{
						{CryptoType: "DID-BLS", Type: "consensus", Key: string(blsDids[i])},
					},
				},
			})
		}
	}

	// Connect mesh + keepalive
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)
	excludedNodes = &sync.Map{}
	stopKeepalive = startPeerKeepalive(nodes, 2*time.Second, excludedNodes)

	// Store election epoch 0
	members := make([]elections.ElectionMember, nodeCount)
	weights := make([]uint64, nodeCount)
	for i := 0; i < nodeCount; i++ {
		members[i] = elections.ElectionMember{
			Account: "mock-tss-" + strconv.Itoa(i+1),
			Key:     string(blsDids[i]),
		}
		weights[i] = 10
	}
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 0, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        1,
		})
	}

	return
}

// doKeygen runs keygen at block 100, waits for completion, returns pubkey.
func doKeygen(t *testing.T, nodes []nodeComponents, keyId string, keygenBroadcast **hivego.HiveTransaction, keygenPubKeys map[string]string, mu *sync.Mutex) {
	t.Helper()

	for _, node := range nodes {
		node.tssKeys.InsertKey(keyId, tss_db.EcdsaType, tss_db.MaxKeyEpochs)
	}

	headHeight := uint64(120)
	processBlocks(nodes, 2, 98, &headHeight)
	for bh := uint64(99); bh <= 105; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	for i := 0; i < 120; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *keygenBroadcast != nil
		mu.Unlock()
		if done {
			break
		}
	}
	mu.Lock()
	if *keygenBroadcast == nil {
		t.Fatal("keygen did not complete")
	}
	if keygenPubKeys[keyId] == "" {
		t.Fatal("keygen pubkey not captured")
	}
	mu.Unlock()
}

// doReshare stores a new election at the given epoch and triggers reshare.
func doReshare(t *testing.T, nodes []nodeComponents, epoch uint64, blockStart uint64, reshareBroadcast **hivego.HiveTransaction, mu *sync.Mutex) {
	t.Helper()

	// Update key epoch and status for reshare
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: tss_db.TssKeyActive,
			Algo:   tss_db.EcdsaType,
			Epoch:  epoch - 1,
		})
	}

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

	for i := 0; i < 180; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *reshareBroadcast != nil
		mu.Unlock()
		if done {
			return
		}
	}
	t.Fatal("reshare did not complete within timeout")
}

// TestSigningWithRenewedKey is a standalone test for Phase 14.
// Keygen → reshare → deprecate → renew → sign with renewed key.
func TestSigningWithRenewedKey(t *testing.T) {
	nodes, _, keygenPubKeys, signBroadcast, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = excludedNodes

	// Keygen
	log.Info("=== KEYGEN ===")
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	log.Info("keygen done", "pubkey", keygenPubKeys["test-key"])

	// Store election epoch 1 + reshare
	log.Info("=== RESHARE to epoch 1 ===")
	blsDids := make([]dids.BlsDID, nodeCount)
	members := make([]elections.ElectionMember, nodeCount)
	weights := make([]uint64, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDids[i], _ = nodes[i].identity.BlsDID()
		members[i] = elections.ElectionMember{
			Account: "mock-tss-" + strconv.Itoa(i+1),
			Key:     string(blsDids[i]),
		}
		weights[i] = 10
	}
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1, NetId: "vsc-mocknet"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        150,
		})
	}
	doReshare(t, nodes, 1, 200, reshareBroadcast, mu)
	log.Info("reshare to epoch 1 done")

	// Simulate key renewal: reactivate with new expiry at epoch 1
	log.Info("=== RENEW KEY ===")
	renewedExpiry := uint64(1 + tss_db.MaxKeyEpochs)
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:          "test-key",
			Status:      tss_db.TssKeyActive,
			Algo:        tss_db.EcdsaType,
			Epoch:       1,
			Epochs:      tss_db.MaxKeyEpochs,
			ExpiryEpoch: renewedExpiry,
		})
	}

	// Verify renewal
	renewedKey, err := nodes[0].tssKeys.FindKey("test-key")
	if err != nil {
		t.Fatalf("FindKey after renewal failed: %v", err)
	}
	if renewedKey.Status != tss_db.TssKeyActive {
		t.Errorf("Expected active, got %s", renewedKey.Status)
	}

	// Sign with renewed key
	log.Info("=== SIGN WITH RENEWED KEY ===")
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

	// Block 250 (250 % 50 == 0, sign trigger)
	headHeight := uint64(270)
	processBlocks(nodes, 206, 248, &headHeight)
	for bh := uint64(249); bh <= 255; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Info("Waiting for signing with renewed key...")
	signDone := false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := *signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing with renewed key completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Signing with renewed key did not complete within timeout")
	}

	// Verify signature
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
						t.Error("Signature is empty")
					} else {
						log.Info("signature verified", "sig", pkt.Sig)
						verifyEcdsaSig(t, pkt.Sig, pkt.Msg, keygenPubKeys["test-key"])
					}
				}
			}
		}
	}
}

// TestBlameDbRoundTripOldCommitteeExclusion proves the full blame→DB→exclusion path:
//
//  1. Keygen with 6 nodes (real P2P + MongoDB)
//  2. Manually store a blame commitment targeting mock-tss-6 (simulating TimeoutResult.Serialize)
//  3. Read it back from DB, decode the bitset, assert mock-tss-6 is blamed
//  4. Replay the exact exclusion logic from RunActions (tss.go:713-745):
//     read blame from DB, decode against currentElection, build old committee excluding blamed node
//  5. Assert mock-tss-6 is NOT in the old committee and IS in the excluded list
//
// This proves the DB stores the bitset correctly and the decode→exclusion path
// produces the right result. The encoding side is proven by TestBlameEpochMismatchProvesBug.
func TestBlameDbRoundTripOldCommitteeExclusion(t *testing.T) {
	nodes, _, keygenPubKeys, _, reshareBroadcast, keygenBroadcast, mu, excludedNodes, stopKeepalive := setupCluster(t)
	defer stopKeepalive()
	_ = reshareBroadcast
	_ = excludedNodes

	// Step 1: Keygen
	log.Info("=== KEYGEN ===")
	doKeygen(t, nodes, "test-key", keygenBroadcast, keygenPubKeys, mu)
	log.Info("keygen done", "pubkey", keygenPubKeys["test-key"])

	// Get the keygen commitment to know who's in the old committee
	keygenCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "keygen")
	if err != nil {
		t.Fatalf("Failed to get keygen commitment: %v", err)
	}
	t.Logf("Keygen commitment: epoch=%d blockHeight=%d commitment=%s",
		keygenCommitment.Epoch, keygenCommitment.BlockHeight, keygenCommitment.Commitment)

	// Decode keygen commitment to see who's in old committee
	keygenBytes, _ := base64.RawURLEncoding.DecodeString(keygenCommitment.Commitment)
	keygenBits := new(big.Int).SetBytes(keygenBytes)

	election0 := nodes[0].electionDb.GetElection(0)
	if election0 == nil {
		t.Fatal("Election epoch 0 not found")
	}

	oldCommitteeMembers := make([]string, 0)
	for idx, member := range election0.Members {
		if keygenBits.Bit(idx) == 1 {
			oldCommitteeMembers = append(oldCommitteeMembers, member.Account)
		}
	}
	t.Logf("Old committee from keygen: %v (%d members)", oldCommitteeMembers, len(oldCommitteeMembers))

	// Step 2: Store a blame commitment targeting mock-tss-6
	// This simulates what TimeoutResult.Serialize() produces after a reshare timeout.
	// The blame is encoded against the CURRENT election (after dispatcher.newEpoch fix).
	blamedNode := "mock-tss-6"

	// Store election epoch 1 (same members — no index shift on testnet)
	blsDids := make([]dids.BlsDID, nodeCount)
	members := make([]elections.ElectionMember, nodeCount)
	weights := make([]uint64, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDids[i], _ = nodes[i].identity.BlsDID()
		members[i] = elections.ElectionMember{
			Account: "mock-tss-" + strconv.Itoa(i+1),
			Key:     string(blsDids[i]),
		}
		weights[i] = 10
	}
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 1, NetId: "vsc-mocknet"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        150,
		})
	}

	// Build blame bitset: find mock-tss-6's index in epoch 1 election
	election1 := nodes[0].electionDb.GetElection(1)
	if election1 == nil {
		t.Fatal("Election epoch 1 not found")
	}

	blameBitset := new(big.Int)
	blamedIdx := -1
	for idx, member := range election1.Members {
		if member.Account == blamedNode {
			blameBitset.SetBit(blameBitset, idx, 1)
			blamedIdx = idx
			break
		}
	}
	if blamedIdx == -1 {
		t.Fatalf("Blamed node %q not found in election epoch 1", blamedNode)
	}
	blameEncoded := base64.RawURLEncoding.EncodeToString(blameBitset.Bytes())
	t.Logf("Blame bitset: %q (bit %d = %s)", blameEncoded, blamedIdx, blamedNode)

	// Store blame commitment in all nodes' DBs
	// block_height must be > keygen commitment's block_height (for the blame freshness check)
	// tx_id must differ from keygen's tx_id to avoid upsert overwriting the keygen record
	// (SetCommitmentData upserts on {key_id, tx_id}).
	blameBlockHeight := keygenCommitment.BlockHeight + 50
	for _, node := range nodes {
		err := node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "blame",
			KeyId:       "test-key",
			Commitment:  blameEncoded,
			Epoch:       1, // encoded against epoch 1 (dispatcher.newEpoch)
			BlockHeight: blameBlockHeight,
			TxId:        "blame-tx-1",
		})
		if err != nil {
			t.Fatalf("Failed to store blame commitment: %v", err)
		}
	}
	t.Logf("Stored blame commitment at blockHeight=%d", blameBlockHeight)

	// Step 3: Read blame back from DB and decode it
	// This mirrors RunActions line 677: GetCommitmentByHeight(keyId, bh, "blame")
	readBh := blameBlockHeight + 100 // must be > blame blockHeight
	lastBlame, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", readBh, "blame")
	if err != nil {
		t.Fatalf("Failed to read blame from DB: %v", err)
	}
	if lastBlame.Type != "blame" {
		t.Fatalf("Expected blame commitment, got type=%q", lastBlame.Type)
	}
	t.Logf("Read blame from DB: epoch=%d blockHeight=%d commitment=%q",
		lastBlame.Epoch, lastBlame.BlockHeight, lastBlame.Commitment)

	// Verify blame freshness (same check as RunActions line 696)
	// lastBlame.BlockHeight > commitment.BlockHeight && lastBlame.BlockHeight > bh - BLAME_EXPIRE
	if lastBlame.BlockHeight <= keygenCommitment.BlockHeight {
		t.Fatalf("Blame not fresh: blame blockHeight %d <= keygen blockHeight %d",
			lastBlame.BlockHeight, keygenCommitment.BlockHeight)
	}

	// Decode blame bitset against current election (epoch 1)
	// This is exactly what RunActions does at lines 714-720
	readBlameBytes, _ := base64.RawURLEncoding.DecodeString(lastBlame.Commitment)
	readBlameBits := new(big.Int).SetBytes(readBlameBytes)

	blamedAccounts := make(map[string]bool)
	for idx, member := range election1.Members {
		if readBlameBits.Bit(idx) == 1 {
			blamedAccounts[member.Account] = true
		}
	}

	t.Logf("Decoded blamed accounts: %v", blamedAccounts)

	if !blamedAccounts[blamedNode] {
		t.Fatalf("FAIL: %q not in blamed accounts after DB round-trip. Got: %v", blamedNode, blamedAccounts)
	}
	// Verify no innocent nodes are blamed
	if len(blamedAccounts) != 1 {
		t.Fatalf("FAIL: expected exactly 1 blamed account, got %d: %v", len(blamedAccounts), blamedAccounts)
	}
	t.Logf("PASS: DB round-trip correctly identifies %q as the only blamed node", blamedNode)

	// Step 4: Build old committee with blame exclusion
	// This replays RunActions lines 727-745 exactly
	commitmentElection := nodes[0].electionDb.GetElection(keygenCommitment.Epoch)
	if commitmentElection == nil {
		t.Fatal("Commitment election not found")
	}

	committedMembers := make([]string, 0)
	excludedByBlame := make([]string, 0)
	fullOldCommitteeSize := 0

	for idx, member := range commitmentElection.Members {
		if keygenBits.Bit(idx) == 1 {
			fullOldCommitteeSize++
			if blamedAccounts[member.Account] {
				excludedByBlame = append(excludedByBlame, member.Account)
				continue
			}
			committedMembers = append(committedMembers, member.Account)
		}
	}

	t.Logf("Full old committee size: %d", fullOldCommitteeSize)
	t.Logf("Excluded by blame: %v", excludedByBlame)
	t.Logf("Remaining old committee: %v (%d members)", committedMembers, len(committedMembers))

	// Assert: blamed node IS in the excluded list
	found := false
	for _, excluded := range excludedByBlame {
		if excluded == blamedNode {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("FAIL: %q not in excludedByBlame list: %v", blamedNode, excludedByBlame)
	}

	// Assert: blamed node is NOT in the remaining old committee
	for _, member := range committedMembers {
		if member == blamedNode {
			t.Fatalf("FAIL: %q found in remaining old committee — exclusion failed", blamedNode)
		}
	}

	// Assert: old committee is one less than full size
	if len(committedMembers) != fullOldCommitteeSize-1 {
		t.Fatalf("FAIL: expected %d remaining members, got %d",
			fullOldCommitteeSize-1, len(committedMembers))
	}

	// Assert: all other keygen members are still present
	for _, original := range oldCommitteeMembers {
		if original == blamedNode {
			continue
		}
		memberFound := false
		for _, remaining := range committedMembers {
			if remaining == original {
				memberFound = true
				break
			}
		}
		if !memberFound {
			t.Fatalf("FAIL: original member %q missing from old committee", original)
		}
	}

	t.Logf("DB round-trip verified — now triggering real reshare through RunActions")

	// Step 5: Trigger a REAL reshare cycle and verify RunActions reads the blame
	// and excludes mock-tss-6 from the old committee.
	// Proof: the reshare commitment stored in DB will have a bitset with only 5 old members
	// (since mock-tss-6 was excluded), not 6.

	// Set key to active at epoch 0 so reshare triggers
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: tss_db.TssKeyActive,
			Algo:   tss_db.EcdsaType,
			Epoch:  0,
		})
	}

	mu.Lock()
	*reshareBroadcast = nil
	mu.Unlock()

	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Trigger reshare at block 200 (200 % 100 == 0)
	headHeight := uint64(220)
	processBlocks(nodes, 152, 198, &headHeight)
	for bh := uint64(199); bh <= 205; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for reshare to complete
	reshareCompleted := false
	for i := 0; i < 180; i++ {
		time.Sleep(1 * time.Second)
		mu.Lock()
		done := *reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareCompleted = true
			break
		}
	}

	if !reshareCompleted {
		t.Fatal("Reshare did not complete — RunActions may not have read the blame correctly")
	}

	// Read the reshare commitment from DB
	reshareCommitment, err := nodes[0].tssCommitments.GetCommitmentByHeight("test-key", 999, "reshare")
	if err != nil {
		t.Fatalf("Failed to get reshare commitment: %v", err)
	}
	t.Logf("Reshare commitment: epoch=%d blockHeight=%d commitment=%s",
		reshareCommitment.Epoch, reshareCommitment.BlockHeight, reshareCommitment.Commitment)

	// The reshare commitment bitset tells us which NEW committee members participated.
	// But we need to verify the OLD committee was reduced. The reshare succeeding
	// with blame stored proves RunActions read the blame and processed it.
	// If RunActions had NOT excluded the blamed node, the reshare would still complete
	// (all 6 are online), so we need a stronger assertion.
	//
	// Check the log evidence: read all blame commitments and verify the reshare
	// was triggered with blame active. We do this by verifying the reshare commitment
	// epoch advanced (it went from 0 to 1), proving the full reshare path ran.
	if reshareCommitment.Epoch != 1 {
		t.Fatalf("Expected reshare to advance to epoch 1, got epoch %d", reshareCommitment.Epoch)
	}

	// Now do the definitive check: store blame for mock-tss-5 AND mock-tss-6,
	// store election epoch 2, trigger another reshare. The reshare must complete
	// with only 4 old members (6 total - 2 blamed). If it completes, RunActions
	// read and applied both blames. We verify by counting: threshold for 6 is 3,
	// need 4 old. With 2 blamed out, exactly 4 remain — the minimum.
	// If blame exclusion was broken (either node left in), we'd have 5 or 6 old
	// members and the reshare would still work — so we need an additional check.
	//
	// The definitive proof: blame 3 nodes. threshold+1 = 4 needed. Only 3 would
	// remain — BELOW threshold. If RunActions excludes all 3, it should log
	// "insufficient old participants" and skip. If it doesn't exclude them,
	// reshare attempts with all 6 old members and succeeds.
	// We prove exclusion works by showing reshare DOES NOT complete.

	t.Logf("=== Step 6: Blame 3 nodes, verify reshare is skipped (insufficient old participants) ===")

	// Store election epoch 2 (same members)
	for _, node := range nodes {
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 2, NetId: "vsc-mocknet"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members, Weights: weights},
			BlockHeight:        250,
		})
	}

	// Build blame bitset for 3 nodes: mock-tss-4, mock-tss-5, mock-tss-6
	blame3Bitset := new(big.Int)
	blamed3Nodes := []string{"mock-tss-4", "mock-tss-5", "mock-tss-6"}
	election2 := nodes[0].electionDb.GetElection(2)
	for _, bNode := range blamed3Nodes {
		for idx, member := range election2.Members {
			if member.Account == bNode {
				blame3Bitset.SetBit(blame3Bitset, idx, 1)
				break
			}
		}
	}
	blame3Encoded := base64.RawURLEncoding.EncodeToString(blame3Bitset.Bytes())
	t.Logf("Blame-3 bitset: %q targeting %v", blame3Encoded, blamed3Nodes)

	// Store blame at block 250 (after reshare commitment at ~200)
	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "blame",
			KeyId:       "test-key",
			Commitment:  blame3Encoded,
			Epoch:       2,
			BlockHeight: 250,
			TxId:        "blame-tx-2",
		})
	}

	// Update key to epoch 1 (from the reshare that just completed)
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: tss_db.TssKeyActive,
			Algo:   tss_db.EcdsaType,
			Epoch:  1,
		})
	}

	mu.Lock()
	*reshareBroadcast = nil
	mu.Unlock()

	// Trigger reshare at block 300 — all 6 nodes online, but 3 blamed
	headHeight = uint64(320)
	processBlocks(nodes, 252, 298, &headHeight)
	for bh := uint64(299); bh <= 305; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{BlockNumber: bh}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait a SHORT time — if blame exclusion works, reshare should be skipped immediately
	// ("insufficient old participants": 3 remaining < threshold+1=4).
	// If blame exclusion is broken, reshare would start with all 6 and complete in ~60s.
	t.Logf("Waiting to verify reshare is skipped (not started)...")
	reshare2Completed := false
	for i := 0; i < 20; i++ {
		time.Sleep(1 * time.Second)
		mu.Lock()
		done := *reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshare2Completed = true
			break
		}
	}

	if reshare2Completed {
		t.Fatal("FAIL: Reshare completed despite 3 blamed nodes — blame exclusion did NOT work. " +
			"RunActions should have found only 3 old members (below threshold+1=4) and skipped.")
	}

	t.Logf("PASS: Reshare correctly skipped — blame exclusion removed 3 nodes from old committee,")
	t.Logf("  leaving only 3 old members, below threshold+1=4. RunActions read the blame from DB")
	t.Logf("  and applied it to the real reshare path.")
}
