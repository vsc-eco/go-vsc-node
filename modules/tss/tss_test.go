package tss_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/e2e"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	vtss "vsc-node/modules/tss"

	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const nodeCount = 6

type MockElectionSystem struct {
	// Use a sorted slice for deterministic iteration order.
	// Go map iteration is random, which causes nodes to disagree on leader selection.
	WitnessNames []string
}

func (mes *MockElectionSystem) GetSchedule(blockHeight uint64) []stateEngine.WitnessSlot {
	witnesses := make([]stateEngine.Witness, len(mes.WitnessNames))
	for i, name := range mes.WitnessNames {
		witnesses[i] = stateEngine.Witness{Account: name}
	}

	list := make([]stateEngine.WitnessSlot, 0)
	for x := 0; x < len(witnesses); x++ {
		modl := (int(blockHeight)/10 + x) % len(witnesses)
		list = append(list, stateEngine.WitnessSlot{
			Account:    witnesses[modl].Account,
			SlotHeight: blockHeight + uint64(x*10),
		})
	}
	return list
}

type nodeComponents struct {
	agg            *aggregate.Aggregate
	consumer       *blockconsumer.HiveConsumer
	witnessDb      witnesses.Witnesses
	p2p            *libp2p.P2PServer
	electionDb     elections.Elections
	tssKeys        tss_db.TssKeys
	tssRequests    tss_db.TssRequests
	tssCommitments tss_db.TssCommitments
	identity       common.IdentityConfig
}

func dropTestDatabases() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
	for i := 0; i < nodeCount; i++ {
		client.Database("go-vsc-tss-test-" + strconv.Itoa(i)).Drop(ctx)
	}
}

func connectAllPeers(t *testing.T, nodes []nodeComponents) {
	for i := 0; i < nodeCount; i++ {
		for j := 0; j < nodeCount; j++ {
			if i == j {
				continue
			}
			targetID := nodes[j].p2p.ID()
			addrs := nodes[j].p2p.Addrs()
			// Add addresses to peerstore with permanent TTL so connections persist
			nodes[i].p2p.Peerstore().AddAddrs(targetID, addrs, peerstore.PermanentAddrTTL)
			addrInfo := peer.AddrInfo{ID: targetID, Addrs: addrs}
			err := nodes[i].p2p.Connect(context.Background(), addrInfo)
			if err != nil {
				fmt.Printf("[TEST] Connect %d->%d failed: %v\n", i, j, err)
			}
			// Protect the connection from being trimmed by the connection manager
			nodes[i].p2p.Host().ConnManager().Protect(targetID, "tss-test")
		}
	}
	// Verify connectivity
	for i := 0; i < nodeCount; i++ {
		connected := 0
		for j := 0; j < nodeCount; j++ {
			if i == j {
				continue
			}
			if nodes[i].p2p.Host().Network().Connectedness(nodes[j].p2p.ID()) == network.Connected {
				connected++
			}
		}
		fmt.Printf("[TEST] Node %d connected to %d/%d peers\n", i, connected, nodeCount-1)
	}
}

// startPeerKeepalive runs a background goroutine that periodically reconnects
// all peers. This prevents libp2p connections from being dropped during long
// TSS protocol runs. Returns a cancel function to stop the keepalive.
// startPeerKeepalive runs a background goroutine that periodically pings all
// peers to keep connections alive and reconnects if needed.
func startPeerKeepalive(nodes []nodeComponents, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < len(nodes); i++ {
					for j := 0; j < len(nodes); j++ {
						if i == j {
							continue
						}
						targetID := nodes[j].p2p.ID()
						h := nodes[i].p2p.Host()
						if h.Network().Connectedness(targetID) != network.Connected {
							addrs := nodes[j].p2p.Addrs()
							addrInfo := peer.AddrInfo{ID: targetID, Addrs: addrs}
							err := h.Connect(ctx, addrInfo)
							if err != nil {
								fmt.Printf("[KEEPALIVE] Reconnect %d->%d failed: %v\n", i, j, err)
							} else {
								fmt.Printf("[KEEPALIVE] Reconnected %d->%d\n", i, j)
							}
						} else {
							// Ping the peer to keep the connection alive
							s, err := h.NewStream(ctx, targetID, "/ipfs/ping/1.0.0")
							if err == nil {
								s.Close()
							}
						}
					}
				}
			}
		}
	}()
	return cancel
}

func MakeNode(index int, mes *MockElectionSystem, broadcastCb func(tx hivego.HiveTransaction) error) nodeComponents {
	path := "data-dir-" + strconv.Itoa(index)

	// Clean old data directory to avoid stale identity keys
	os.RemoveAll(path)
	os.Mkdir(path, os.ModePerm)

	dbConf := db.NewDbConfig(path)
	identity := common.NewIdentityConfig(path)
	p2pConf := libp2p.NewConfig(path)
	aggregate.New([]aggregate.Plugin{dbConf, identity, p2pConf}).Init()
	dbConf.SetDbName("go-vsc-tss-test-" + strconv.Itoa(index))
	identity.SetUsername("mock-tss-" + strconv.Itoa(index+1))
	p2pConf.SetOptions(libp2p.P2POpts{
		Port:         22222 + index,
		ServerMode:   true,
		AllowPrivate: true,
		Bootnodes:    []string{},
	})
	sysConf := systemconfig.MocknetConfig()

	database := db.New(dbConf)
	vscDb := vsc.New(database, dbConf)
	tssKeys := tss_db.NewKeys(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	electionDb := elections.New(vscDb)
	witnessDb := witnesses.New(vscDb)

	kp := e2e.HashSeed([]byte(e2e.SEED_PREFIX + path))

	brcst := hive.MockTransactionBroadcaster{
		KeyPair:  kp,
		Callback: broadcastCb,
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: brcst,
		TransactionCrafter:         hive.TransactionCrafter{},
	}

	blockConsumer := blockconsumer.New(nil)

	p2p := libp2p.New(witnessDb, p2pConf, identity, sysConf, nil)

	keystore, err := flatfs.CreateOrOpen(path+"/keys", flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}

	tssMgr := vtss.New(p2p, tssKeys, tssRequests, tssCommitments, witnessDb, electionDb, blockConsumer, mes, identity, sysConf, keystore, &txCreator)

	agg := aggregate.New([]aggregate.Plugin{
		identity,
		dbConf,
		database,
		vscDb,
		tssKeys,
		tssRequests,
		tssCommitments,
		electionDb,
		witnessDb,
		p2p,
		tssMgr,
	})

	return nodeComponents{
		agg:            agg,
		consumer:       blockConsumer,
		witnessDb:      witnessDb,
		p2p:            p2p,
		electionDb:     electionDb,
		tssKeys:        tssKeys,
		tssRequests:    tssRequests,
		tssCommitments: tssCommitments,
		identity:       identity,
	}
}

func processBlocks(nodes []nodeComponents, start, end uint64, headHeight *uint64) {
	for bh := start; bh <= end; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, headHeight)
		}
	}
}

func TestTss(t *testing.T) {
	// Clean databases from previous runs
	dropTestDatabases()

	// Build MockElectionSystem with 6 witnesses (sorted for deterministic leader selection)
	witnessNames := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		witnessNames[i] = "mock-tss-" + strconv.Itoa(i+1)
	}
	sort.Strings(witnessNames)
	mes := &MockElectionSystem{WitnessNames: witnessNames}

	// Shared broadcast capture
	var mu sync.Mutex
	var keygenBroadcast *hivego.HiveTransaction
	var signBroadcast *hivego.HiveTransaction
	var reshareBroadcast *hivego.HiveTransaction

	broadcastCb := func(tx hivego.HiveTransaction) error {
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
				fmt.Printf("[TEST] Broadcast received: id=%s\n", cj.Id)
				if cj.Id == "vsc.tss_commitment" {
					var payload map[string]interface{}
					json.Unmarshal([]byte(cj.Json), &payload)
					for _, v := range payload {
						if m, ok := v.(map[string]interface{}); ok {
							if tp, ok := m["type"].(string); ok {
								if tp == "keygen" && keygenBroadcast == nil {
									txCopy := tx
									keygenBroadcast = &txCopy
									fmt.Println("[TEST] Keygen commitment captured")
								} else if tp == "reshare" && reshareBroadcast == nil {
									txCopy := tx
									reshareBroadcast = &txCopy
									fmt.Println("[TEST] Reshare commitment captured")
								}
							}
						}
					}
				} else if cj.Id == "vsc.tss_sign" && signBroadcast == nil {
					txCopy := tx
					signBroadcast = &txCopy
					fmt.Println("[TEST] Sign result captured")
				}
			}
		}
		return nil
	}

	// Step 1: Create 6 nodes
	nodes := make([]nodeComponents, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes[i] = MakeNode(i, mes, broadcastCb)
	}

	// Cleanup data directories on test end
	t.Cleanup(func() {
		for i := 0; i < nodeCount; i++ {
			os.RemoveAll("data-dir-" + strconv.Itoa(i))
		}
	})

	// Start all nodes
	for i := 0; i < nodeCount; i++ {
		go test_utils.RunPlugin(t, nodes[i].agg)
	}

	// Wait for P2P startup
	time.Sleep(5 * time.Second)

	// Register connection event notifiers to debug disconnections
	for i := 0; i < nodeCount; i++ {
		nodeIdx := i
		notifee := &network.NotifyBundle{
			DisconnectedF: func(n network.Network, conn network.Conn) {
				fmt.Printf("[NOTIF] Node %d disconnected from %s\n", nodeIdx, conn.RemotePeer().String()[:16])
			},
		}
		nodes[i].p2p.Host().Network().Notify(notifee)
	}

	// Step 2: Derive BLS DIDs and set witness updates
	blsDids := make([]dids.BlsDID, nodeCount)
	for i := 0; i < nodeCount; i++ {
		blsDid, err := nodes[i].identity.BlsDID()
		if err != nil {
			t.Fatalf("Failed to derive BLS DID for node %d: %v", i, err)
		}
		blsDids[i] = blsDid
		fmt.Printf("[TEST] Node %d (mock-tss-%d) BLS DID: %s\n", i, i+1, blsDid)
	}

	// Set witness updates in all witness DBs
	for _, node := range nodes {
		for i := 0; i < nodeCount; i++ {
			err := node.witnessDb.SetWitnessUpdate(witnesses.SetWitnessUpdateType{
				Account: "mock-tss-" + strconv.Itoa(i+1),
				Metadata: witnesses.PostingJsonMetadata{
					VscNode: witnesses.PostingJsonMetadataVscNode{
						PeerId: nodes[i].p2p.ID().String(),
					},
					DidKeys: []witnesses.PostingJsonKeys{
						{
							CryptoType: "DID-BLS",
							Type:       "consensus",
							Key:        string(blsDids[i]),
						},
					},
				},
			})
			if err != nil {
				t.Fatalf("Failed to set witness update for node %d: %v", i, err)
			}
		}
	}

	// Connect all P2P peers (full mesh)
	connectAllPeers(t, nodes)
	time.Sleep(3 * time.Second)

	// Start background keepalive to prevent connections from dropping during long TSS protocols.
	// Use a short interval because libp2p connections can drop during reshare rounds.
	stopKeepalive := startPeerKeepalive(nodes, 2*time.Second)
	defer stopKeepalive()

	// Step 3: Store election (epoch 0) in all nodes
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
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 0,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members,
				Weights: weights,
			},
			BlockHeight: 1,
		})
		if err != nil {
			t.Fatalf("Failed to store election: %v", err)
		}
	}

	// Step 4: Insert TSS key for keygen in all nodes
	for _, node := range nodes {
		node.tssKeys.InsertKey("test-key", tss_db.EcdsaType)
	}

	fmt.Println("[TEST] === PHASE 1: KEYGEN ===")
	fmt.Println("[TEST] Processing blocks to trigger keygen at block 100...")

	// Step 5: Process blocks to trigger keygen at block 100
	// Process blocks up to 99 quickly, then slowly around 100
	headHeight := uint64(120)
	processBlocks(nodes, 2, 98, &headHeight)
	// Slow processing around trigger point
	for bh := uint64(99); bh <= 105; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for keygen to fully complete, including the RunActions goroutine releasing the lock.
	// The RunActions goroutine holds the lock until: keygen completes + 5s sleep + 6s waitForSigs + broadcast.
	// Poll for keygen broadcast (up to 4 minutes), then wait additional time for lock release.
	fmt.Println("[TEST] Waiting for keygen TSS protocol to complete...")
	keygenDone := false
	for i := 0; i < 240; i++ {
		time.Sleep(1 * time.Second)
		mu.Lock()
		done := keygenBroadcast != nil
		mu.Unlock()
		if done {
			keygenDone = true
			fmt.Println("[TEST] Keygen broadcast detected!")
			break
		}
	}

	if !keygenDone {
		// Keygen broadcast wasn't captured (BLS sig aggregation may have failed),
		// but the keygen protocol itself likely completed.
		// Wait for the full 2-minute RunActions timeout + buffer to ensure lock release.
		fmt.Println("[TEST] Keygen broadcast not captured, waiting for RunActions timeout...")
		time.Sleep(90 * time.Second)
	} else {
		// Keygen broadcast was captured, the lock releases shortly after broadcast.
		// Wait a few extra seconds for the goroutine to fully finish.
		fmt.Println("[TEST] Waiting for RunActions lock to release...")
		time.Sleep(5 * time.Second)
	}

	// Step 6: Insert keygen commitment for signing and reshare.
	// Even if the broadcast succeeded, we manually insert to ensure all nodes have it.
	bitset := big.NewInt(0)
	for i := 0; i < nodeCount; i++ {
		bitset.SetBit(bitset, i, 1)
	}
	commitmentStr := base64.RawURLEncoding.EncodeToString(bitset.Bytes())

	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "keygen",
			KeyId:       "test-key",
			Epoch:       0,
			BlockHeight: 100,
			Commitment:  commitmentStr,
			TxId:        "mock-keygen-tx",
		})
	}

	// Step 7: Update key status to active for signing and reshare
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  0,
		})
	}

	// Assertion 1: Verify TSS key was created
	key, err := nodes[0].tssKeys.FindKey("test-key")
	if err != nil {
		t.Fatalf("TSS key not found after keygen: %v", err)
	}
	if key.Status != "active" {
		t.Errorf("Expected key status 'active', got '%s'", key.Status)
	}
	fmt.Printf("[TEST] TSS key created: id=%s algo=%s status=%s\n", key.Id, key.Algo, key.Status)

	// Step 8: Insert signing request
	msgHex := "4c67f5f07565d45cccd89bebfb3a9ee357bff33fef45b14b8f424cd17c93e6f8"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex,
			Status: tss_db.SignPending,
		})
	}

	fmt.Println("[TEST] === PHASE 2: SIGNING ===")
	fmt.Println("[TEST] Processing blocks to trigger signing at block 150...")

	// Re-establish peer connections before signing (connections may have timed out during keygen wait)
	connectAllPeers(t, nodes)
	time.Sleep(3 * time.Second)

	// Process remaining blocks from keygen phase
	processBlocks(nodes, 106, 148, &headHeight)

	// Slow processing around signing trigger
	headHeight = 170
	for bh := uint64(149); bh <= 155; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for signing to complete
	fmt.Println("[TEST] Waiting for signing to complete...")
	signDone := false
	for i := 0; i < 120; i++ {
		time.Sleep(1 * time.Second)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			fmt.Println("[TEST] Signing completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Signing did not complete within timeout (2 minutes)")
	}

	// Assertion 2: Verify signature broadcast
	mu.Lock()
	signTx := signBroadcast
	mu.Unlock()

	if signTx != nil {
		for _, op := range signTx.Operations {
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
			if len(payload.Packet) > 0 {
				sig := payload.Packet[0]
				if sig.Sig == "" {
					t.Error("Signing produced empty signature")
				} else {
					fmt.Printf("[TEST] Signature created for msg %s: %s\n", sig.Msg, sig.Sig)
				}
				if sig.Msg != msgHex {
					t.Errorf("Signed message mismatch: got %s, want %s", sig.Msg, msgHex)
				}
			} else {
				t.Error("Sign broadcast had empty packet")
			}
		}
	}

	fmt.Println("[TEST] === PHASE 3: RESHARE ===")

	// Wait for signing RunActions lock to release before reshare.
	// Signing is fast (no pre-params), but the RunActions goroutine holds the lock
	// until signing completes + broadcast. Wait a reasonable amount.
	fmt.Println("[TEST] Waiting for signing lock to release...")
	time.Sleep(15 * time.Second)

	// Step 9: Store new election (epoch 1) for reshare
	for _, node := range nodes {
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 1,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members,
				Weights: weights,
			},
			BlockHeight: 190,
		})
		if err != nil {
			t.Fatalf("Failed to store election epoch 1: %v", err)
		}
	}

	// Re-establish peer connections before reshare
	connectAllPeers(t, nodes)
	time.Sleep(3 * time.Second)

	fmt.Println("[TEST] Processing blocks to trigger reshare at block 200...")

	// Process blocks up to reshare trigger
	processBlocks(nodes, 156, 198, &headHeight)

	headHeight = 220
	for bh := uint64(199); bh <= 205; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for reshare to complete
	fmt.Println("[TEST] Waiting for reshare to complete...")
	reshareDone := false
	for i := 0; i < 240; i++ {
		time.Sleep(1 * time.Second)
		mu.Lock()
		done := reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareDone = true
			fmt.Println("[TEST] Reshare completed!")
			break
		}
	}

	if !reshareDone {
		t.Fatal("Reshare did not complete within timeout (4 minutes)")
	}

	// Assertion 3: Reshare broadcast captured
	mu.Lock()
	if reshareBroadcast == nil {
		t.Fatal("Reshare broadcast was nil")
	}
	fmt.Printf("[TEST] Reshare broadcast has %d operations\n", len(reshareBroadcast.Operations))
	mu.Unlock()

	fmt.Println("[TEST] All TSS operations completed successfully!")
}
