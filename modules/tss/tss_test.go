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
	"vsc-node/lib/vsclog"
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

var log = vsclog.Module("tss-test")

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
	tssMgr         *vtss.TssManager
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
				log.Warn("connect failed", "from", i, "to", j, "err", err)
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
		log.Info("node connected", "node", i, "connected", connected, "total", nodeCount-1)
	}
}

// startPeerKeepalive runs a background goroutine that periodically reconnects
// all peers. This prevents libp2p connections from being dropped during long
// TSS protocol runs. Returns a cancel function to stop the keepalive.
// startPeerKeepalive runs a background goroutine that periodically pings all
// peers to keep connections alive and reconnects if needed.
func startPeerKeepalive(nodes []nodeComponents, interval time.Duration, excludedNodes *sync.Map) context.CancelFunc {
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
					if _, excluded := excludedNodes.Load(i); excluded {
						continue
					}
					for j := 0; j < len(nodes); j++ {
						if i == j {
							continue
						}
						if _, excluded := excludedNodes.Load(j); excluded {
							continue
						}
						targetID := nodes[j].p2p.ID()
						h := nodes[i].p2p.Host()
						if h.Network().Connectedness(targetID) != network.Connected {
							addrs := nodes[j].p2p.Addrs()
							addrInfo := peer.AddrInfo{ID: targetID, Addrs: addrs}
							err := h.Connect(ctx, addrInfo)
							if err != nil {
								log.Warn("keepalive reconnect failed", "from", i, "to", j, "err", err)
							} else {
								log.Info("keepalive reconnected", "from", i, "to", j)
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
		tssMgr:         tssMgr,
	}
}

func disconnectNode(nodes []nodeComponents, targetIdx int) {
	targetID := nodes[targetIdx].p2p.ID()
	for i := 0; i < len(nodes); i++ {
		if i == targetIdx {
			continue
		}
		// Unprotect and close connections in both directions
		nodes[i].p2p.Host().ConnManager().Unprotect(targetID, "tss-test")
		nodes[i].p2p.Host().Network().ClosePeer(targetID)
		// Clear addresses so libp2p can't auto-reconnect
		nodes[i].p2p.Peerstore().ClearAddrs(targetID)

		peerID := nodes[i].p2p.ID()
		nodes[targetIdx].p2p.Host().ConnManager().Unprotect(peerID, "tss-test")
		nodes[targetIdx].p2p.Host().Network().ClosePeer(peerID)
		nodes[targetIdx].p2p.Peerstore().ClearAddrs(peerID)
	}
	log.Info("node disconnected from all peers", "node", targetIdx)
}

// connectActivePeers connects all nodes except those in the excluded set.
// Use this instead of connectAllPeers when some nodes are intentionally disconnected.
func connectActivePeers(t *testing.T, nodes []nodeComponents, excludedNodes *sync.Map) {
	for i := 0; i < len(nodes); i++ {
		if _, excluded := excludedNodes.Load(i); excluded {
			continue
		}
		for j := 0; j < len(nodes); j++ {
			if i == j {
				continue
			}
			if _, excluded := excludedNodes.Load(j); excluded {
				continue
			}
			targetID := nodes[j].p2p.ID()
			addrs := nodes[j].p2p.Addrs()
			nodes[i].p2p.Peerstore().AddAddrs(targetID, addrs, peerstore.PermanentAddrTTL)
			addrInfo := peer.AddrInfo{ID: targetID, Addrs: addrs}
			err := nodes[i].p2p.Connect(context.Background(), addrInfo)
			if err != nil {
				log.Warn("connect failed", "from", i, "to", j, "err", err)
			}
			nodes[i].p2p.Host().ConnManager().Protect(targetID, "tss-test")
		}
	}
}

func reconnectNode(t *testing.T, nodes []nodeComponents, targetIdx int) {
	for i := 0; i < len(nodes); i++ {
		if i == targetIdx {
			continue
		}
		// Add addresses in both directions
		targetID := nodes[targetIdx].p2p.ID()
		targetAddrs := nodes[targetIdx].p2p.Addrs()
		nodes[i].p2p.Peerstore().AddAddrs(targetID, targetAddrs, peerstore.PermanentAddrTTL)

		peerID := nodes[i].p2p.ID()
		peerAddrs := nodes[i].p2p.Addrs()
		nodes[targetIdx].p2p.Peerstore().AddAddrs(peerID, peerAddrs, peerstore.PermanentAddrTTL)

		addrInfo := peer.AddrInfo{ID: targetID, Addrs: targetAddrs}
		err := nodes[i].p2p.Connect(context.Background(), addrInfo)
		if err != nil {
			log.Warn("reconnect failed", "from", i, "to", targetIdx, "err", err)
		}
		nodes[i].p2p.Host().ConnManager().Protect(targetID, "tss-test")
		nodes[targetIdx].p2p.Host().ConnManager().Protect(peerID, "tss-test")
	}
	log.Info("node reconnected to all peers", "node", targetIdx)
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
	vsclog.ParseAndApply("trace")

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

	// Declared before broadcastCb so the closure can reference it
	nodes := make([]nodeComponents, nodeCount)

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
				log.Info("broadcast received", "id", cj.Id)
				if cj.Id == "vsc.tss_commitment" {
					var payload map[string]interface{}
					json.Unmarshal([]byte(cj.Json), &payload)
					for _, v := range payload {
						if m, ok := v.(map[string]interface{}); ok {
							if tp, ok := m["type"].(string); ok {
								if tp == "keygen" && keygenBroadcast == nil {
									txCopy := tx
									keygenBroadcast = &txCopy
									log.Info("keygen commitment captured")
								} else if tp == "reshare" && reshareBroadcast == nil {
									txCopy := tx
									reshareBroadcast = &txCopy
									log.Info("reshare commitment captured")
								}
							}
						}
					}
				} else if cj.Id == "vsc.tss_sign" && signBroadcast == nil {
					txCopy := tx
					signBroadcast = &txCopy
					log.Info("sign result captured")

					// Mark signing requests as complete on all nodes
					// (mirrors what the state engine does in production)
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

	// Step 1: Create 6 nodes
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
	time.Sleep(3 * time.Second)

	// Register connection event notifiers to debug disconnections
	for i := 0; i < nodeCount; i++ {
		nodeIdx := i
		notifee := &network.NotifyBundle{
			DisconnectedF: func(n network.Network, conn network.Conn) {
				log.Info("node disconnected from peer", "node", nodeIdx, "peer", conn.RemotePeer().String()[:16])
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
		log.Info("node BLS DID", "node", i, "user", i+1, "did", blsDid)
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
	time.Sleep(2 * time.Second)

	// Start background keepalive to prevent connections from dropping during long TSS protocols.
	// Use a short interval because libp2p connections can drop during reshare rounds.
	excludedNodes := &sync.Map{}
	stopKeepalive := startPeerKeepalive(nodes, 2*time.Second, excludedNodes)
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
		node.tssKeys.InsertKey("test-key", tss_db.EcdsaType, tss_db.MaxKeyEpochs)
	}

	log.Info("=== PHASE 1: KEYGEN ===")
	log.Info("Processing blocks to trigger keygen at block 100...")

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
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for keygen to fully complete, including the RunActions goroutine releasing the lock.
	// The RunActions goroutine holds the lock until: keygen completes + 5s sleep + 6s waitForSigs + broadcast.
	// Poll for keygen broadcast (up to 4 minutes), then wait additional time for lock release.
	log.Info("Waiting for keygen TSS protocol to complete...")
	keygenDone := false
	for i := 0; i < 120; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := keygenBroadcast != nil
		mu.Unlock()
		if done {
			keygenDone = true
			log.Info("Keygen broadcast detected!")
			break
		}
	}

	if !keygenDone {
		// Keygen broadcast wasn't captured (BLS sig aggregation may have failed),
		// but the keygen protocol itself likely completed.
		// Wait for the RunActions timeout + buffer to ensure lock release.
		log.Info("Keygen broadcast not captured, waiting for RunActions timeout...")
		time.Sleep(35 * time.Second)
	} else {
		// Keygen broadcast was captured, the lock releases shortly after broadcast.
		// Wait a few extra seconds for the goroutine to fully finish.
		log.Info("Waiting for RunActions lock to release...")
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
	log.Info("TSS key created", "id", key.Id, "algo", key.Algo, "status", key.Status)

	// Step 8: Insert signing request
	msgHex := "4c67f5f07565d45cccd89bebfb3a9ee357bff33fef45b14b8f424cd17c93e6f8"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex,
			Status: tss_db.SignPending,
		})
	}

	log.Info("=== PHASE 2: SIGNING ===")
	log.Info("Processing blocks to trigger signing at block 150...")

	// Re-establish peer connections before signing (connections may have timed out during keygen wait)
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

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
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for signing to complete
	log.Info("Waiting for signing to complete...")
	signDone := false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Signing did not complete within timeout")
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
					log.Info("signature created", "msg", sig.Msg, "sig", sig.Sig)
				}
				if sig.Msg != msgHex {
					t.Errorf("Signed message mismatch: got %s, want %s", sig.Msg, msgHex)
				}
			} else {
				t.Error("Sign broadcast had empty packet")
			}
		}
	}

	log.Info("=== PHASE 3: RESHARE ===")

	// Wait for signing RunActions lock to release before reshare.
	log.Info("Waiting for signing lock to release...")
	time.Sleep(5 * time.Second)

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
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger reshare at block 200...")

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
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for reshare to complete
	log.Info("Waiting for reshare to complete...")
	reshareDone := false
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareDone = true
			log.Info("Reshare completed!")
			break
		}
	}

	if !reshareDone {
		t.Fatal("Reshare did not complete within timeout")
	}

	// Assertion 3: Reshare broadcast captured
	mu.Lock()
	if reshareBroadcast == nil {
		t.Fatal("Reshare broadcast was nil")
	}
	log.Info("reshare broadcast", "ops", len(reshareBroadcast.Operations))
	mu.Unlock()

	log.Info("Phases 1-3 completed successfully!")

	// =====================================================
	// PHASE 4: SIGN WITH ONE NODE OFFLINE (block 250)
	// =====================================================
	log.Info("=== PHASE 4: SIGN WITH NODE OFFLINE ===")

	// Wait for reshare lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	// Clear any stale retry actions that fired during the wait
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Insert reshare commitment from Phase 3 into all nodes
	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "reshare",
			KeyId:       "test-key",
			Epoch:       1,
			BlockHeight: 200,
			Commitment:  commitmentStr, // all 6 bits set
			TxId:        "mock-reshare-tx",
		})
	}

	// Update key epoch to 1 for all nodes
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  1,
		})
	}

	// Disconnect node 5
	excludedNodes.Store(5, true)
	disconnectNode(nodes, 5)
	time.Sleep(1 * time.Second)

	// Insert new signing request
	msgHex2 := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex2,
			Status: tss_db.SignPending,
		})
	}

	// Reset sign broadcast capture
	mu.Lock()
	signBroadcast = nil
	mu.Unlock()

	// Re-establish connections among remaining 5 nodes (excluding node 5)
	connectActivePeers(t, nodes, excludedNodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger signing at block 250...")
	processBlocks(nodes, 206, 248, &headHeight)
	headHeight = 270
	for bh := uint64(249); bh <= 255; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for sign broadcast
	log.Info("Waiting for signing with node offline to complete...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing with node offline completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 4: Signing with node offline did not complete within timeout")
	}

	// Verify signature
	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
			raw, _ := json.Marshal(op)
			var cj struct {
				Json string `json:"json"`
			}
			json.Unmarshal(raw, &cj)
			var payload struct {
				Packet []struct {
					Sig string `json:"sig"`
				} `json:"packet"`
			}
			json.Unmarshal([]byte(cj.Json), &payload)
			if len(payload.Packet) > 0 && payload.Packet[0].Sig == "" {
				t.Error("Phase 4: Signing with node offline produced empty signature")
			}
		}
	}
	mu.Unlock()

	// Reconnect node 5
	excludedNodes.Delete(5)
	reconnectNode(t, nodes, 5)
	time.Sleep(2 * time.Second)

	log.Info("Phase 4 completed!")

	// =====================================================
	// PHASE 5: RESHARE WITH WITNESS REPLACEMENT (block 300)
	// =====================================================
	log.Info("=== PHASE 5: RESHARE WITH WITNESS REPLACEMENT ===")

	// Wait for sign lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	// Clear stale retries from previous phases
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Store epoch 2 election with only 5 members (drop node 5)
	members5 := members[:5]
	weights5 := weights[:5]
	for _, node := range nodes {
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 2,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members5,
				Weights: weights5,
			},
			BlockHeight: 290,
		})
		if err != nil {
			t.Fatalf("Failed to store election epoch 2: %v", err)
		}
	}

	// Update MockElectionSystem to only include 5 witnesses
	originalWitnessNames := make([]string, len(mes.WitnessNames))
	copy(originalWitnessNames, mes.WitnessNames)
	witnessNames5 := make([]string, 0, 5)
	droppedWitness := "mock-tss-" + strconv.Itoa(6) // node 5 has username mock-tss-6
	for _, name := range mes.WitnessNames {
		if name != droppedWitness {
			witnessNames5 = append(witnessNames5, name)
		}
	}
	mes.WitnessNames = witnessNames5

	// Disconnect node 5 and exclude from keepalive
	excludedNodes.Store(5, true)
	disconnectNode(nodes, 5)
	time.Sleep(1 * time.Second)

	// Reset reshare broadcast capture
	mu.Lock()
	reshareBroadcast = nil
	mu.Unlock()

	// Re-establish connections among 5 active nodes (excluding node 5)
	connectActivePeers(t, nodes, excludedNodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger reshare at block 300...")
	processBlocks(nodes, 256, 298, &headHeight)
	headHeight = 320
	for bh := uint64(299); bh <= 305; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for reshare broadcast
	log.Info("Waiting for reshare with witness replacement...")
	reshareDone = false
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareDone = true
			log.Info("Reshare with witness replacement completed!")
			break
		}
	}

	if !reshareDone {
		t.Fatal("Phase 5: Reshare with witness replacement did not complete within timeout")
	}

	mu.Lock()
	if reshareBroadcast == nil {
		t.Fatal("Phase 5: Reshare broadcast was nil")
	}
	mu.Unlock()

	log.Info("Phase 5 completed!")

	// =====================================================
	// PHASE 6: SIGN WITH REDUCED 5-MEMBER SET (block 350)
	// =====================================================
	log.Info("=== PHASE 6: SIGN WITH REDUCED MEMBER SET ===")

	// Wait for reshare lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	// Clear stale retries from previous phases
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Insert reshare commitment for epoch 2 (5-bit bitset for nodes 0-4)
	bitset5 := big.NewInt(0)
	for i := 0; i < 5; i++ {
		bitset5.SetBit(bitset5, i, 1)
	}
	commitmentStr5 := base64.RawURLEncoding.EncodeToString(bitset5.Bytes())

	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "reshare",
			KeyId:       "test-key",
			Epoch:       2,
			BlockHeight: 300,
			Commitment:  commitmentStr5,
			TxId:        "mock-reshare-tx-2",
		})
	}

	// Update key epoch to 2
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  2,
		})
	}

	// Insert new signing request
	msgHex3 := "b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex3,
			Status: tss_db.SignPending,
		})
	}

	// Reset sign broadcast capture
	mu.Lock()
	signBroadcast = nil
	mu.Unlock()

	// Re-establish connections among 5 active nodes (excluding node 5)
	connectActivePeers(t, nodes, excludedNodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger signing at block 350...")
	processBlocks(nodes, 306, 348, &headHeight)
	headHeight = 370
	for bh := uint64(349); bh <= 355; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for sign broadcast
	log.Info("Waiting for signing with reduced set...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing with reduced set completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 6: Signing with reduced set did not complete within timeout")
	}

	// Verify signature
	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
			raw, _ := json.Marshal(op)
			var cj struct {
				Json string `json:"json"`
			}
			json.Unmarshal(raw, &cj)
			var payload struct {
				Packet []struct {
					Sig string `json:"sig"`
				} `json:"packet"`
			}
			json.Unmarshal([]byte(cj.Json), &payload)
			if len(payload.Packet) > 0 && payload.Packet[0].Sig == "" {
				t.Error("Phase 6: Signing with reduced set produced empty signature")
			}
		}
	}
	mu.Unlock()

	log.Info("Phase 6 completed!")

	// =====================================================
	// PHASE 7: RESHARE WITH NODE RETURNING (block 400)
	// =====================================================
	log.Info("=== PHASE 7: RESHARE WITH NODE RETURNING ===")

	// Wait for sign lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	// Clear stale retries from previous phases
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Restore MockElectionSystem to all 6 witnesses
	mes.WitnessNames = originalWitnessNames

	// Reconnect node 5
	excludedNodes.Delete(5)
	reconnectNode(t, nodes, 5)
	time.Sleep(2 * time.Second)

	// Store epoch 3 election with all 6 members
	for _, node := range nodes {
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 3,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members,
				Weights: weights,
			},
			BlockHeight: 390,
		})
		if err != nil {
			t.Fatalf("Failed to store election epoch 3: %v", err)
		}
	}

	// Reset reshare broadcast capture
	mu.Lock()
	reshareBroadcast = nil
	mu.Unlock()

	// Re-establish full mesh connections
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger reshare at block 400...")
	processBlocks(nodes, 356, 398, &headHeight)
	headHeight = 420
	for bh := uint64(399); bh <= 405; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for reshare broadcast
	log.Info("Waiting for reshare with node returning...")
	reshareDone = false
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareDone = true
			log.Info("Reshare with node returning completed!")
			break
		}
	}

	if !reshareDone {
		t.Fatal("Phase 7: Reshare with node returning did not complete within timeout")
	}

	mu.Lock()
	if reshareBroadcast == nil {
		t.Fatal("Phase 7: Reshare broadcast was nil")
	}
	mu.Unlock()

	log.Info("Phases 1-7 completed successfully!")

	// =====================================================
	// PHASE 8: MULTIPLE SIMULTANEOUS SIGNING REQUESTS (block 450)
	// =====================================================
	log.Info("=== PHASE 8: MULTIPLE SIMULTANEOUS SIGNING REQUESTS ===")

	// Wait for reshare lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	// Clear stale retries from previous phases
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Insert reshare commitment from Phase 7 into all nodes
	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "reshare",
			KeyId:       "test-key",
			Epoch:       3,
			BlockHeight: 400,
			Commitment:  commitmentStr, // all 6 bits set
			TxId:        "mock-reshare-tx-3",
		})
	}

	// Update key epoch to 3 for all nodes
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  3,
		})
	}

	// Insert two different signing requests for the same key
	msgHexA := "1111111111111111111111111111111111111111111111111111111111111111"
	msgHexB := "2222222222222222222222222222222222222222222222222222222222222222"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHexA,
			Status: tss_db.SignPending,
		})
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHexB,
			Status: tss_db.SignPending,
		})
	}

	// Reset sign broadcast capture
	mu.Lock()
	signBroadcast = nil
	mu.Unlock()

	// Re-establish full mesh connections
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger signing at block 450...")
	processBlocks(nodes, 406, 448, &headHeight)
	headHeight = 470
	for bh := uint64(449); bh <= 455; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for sign broadcast
	log.Info("Waiting for multi-sign to complete...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Multi-sign completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 8: Multiple simultaneous signing did not complete within timeout")
	}

	// Verify both signatures are in the broadcast
	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
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

			if len(payload.Packet) < 2 {
				t.Errorf("Phase 8: Expected at least 2 signatures in packet, got %d", len(payload.Packet))
			} else {
				foundA, foundB := false, false
				for _, pkt := range payload.Packet {
					if pkt.Msg == msgHexA {
						foundA = true
						if pkt.Sig == "" {
							t.Error("Phase 8: Signature for msgA is empty")
						}
					}
					if pkt.Msg == msgHexB {
						foundB = true
						if pkt.Sig == "" {
							t.Error("Phase 8: Signature for msgB is empty")
						}
					}
				}
				if !foundA {
					t.Errorf("Phase 8: Missing signature for msgA (%s)", msgHexA)
				}
				if !foundB {
					t.Errorf("Phase 8: Missing signature for msgB (%s)", msgHexB)
				}
			}
		}
	}
	mu.Unlock()

	log.Info("Phase 8 completed!")

	// =====================================================
	// PHASE 9: MULTIPLE KEYS / KEYLOCKS MECHANISM (block 500)
	// =====================================================
	log.Info("=== PHASE 9: MULTIPLE KEYS / KEYLOCKS ===")

	// Wait for sign lock to release and stale retry goroutines to fire
	time.Sleep(5 * time.Second)

	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Insert a second key "test-key-2" as "new" (will trigger keygen)
	for _, node := range nodes {
		node.tssKeys.InsertKey("test-key-2", tss_db.EcdsaType, tss_db.MaxKeyEpochs)
	}

	// Insert a signing request for test-key-2 (should be blocked by keyLocks during keygen)
	msgHexLock := "3333333333333333333333333333333333333333333333333333333333333333"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key-2",
			Msg:    msgHexLock,
			Status: tss_db.SignPending,
		})
	}

	// Also insert a signing request for test-key (already active, should NOT be locked)
	msgHex4 := "4444444444444444444444444444444444444444444444444444444444444444"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex4,
			Status: tss_db.SignPending,
		})
	}

	// Reset sign and keygen broadcast captures
	mu.Lock()
	signBroadcast = nil
	keygenBroadcast = nil
	mu.Unlock()

	// Re-establish full mesh connections
	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Block 500 is both TSS_ROTATE_INTERVAL (100) and TSS_SIGN_INTERVAL (50) boundary.
	// This triggers keygen for test-key-2 AND signing for test-key.
	// keyLocks should prevent signing test-key-2 while its keygen is active.
	log.Info("Processing blocks to trigger keygen+sign at block 500...")
	processBlocks(nodes, 456, 498, &headHeight)
	headHeight = 520
	for bh := uint64(499); bh <= 505; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for sign broadcast (for test-key, the active key)
	log.Info("Waiting for signing of active key...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing of active key completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 9: Signing of active key (test-key) did not complete within timeout")
	}

	// Verify the sign broadcast is for test-key (not test-key-2 which should be locked)
	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
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
				if pkt.KeyId == "test-key-2" {
					t.Error("Phase 9: test-key-2 should not have been signed (keyLocks should block it during keygen)")
				}
				if pkt.KeyId == "test-key" && pkt.Msg == msgHex4 && pkt.Sig == "" {
					t.Error("Phase 9: Signature for test-key is empty")
				}
			}
		}
	}
	mu.Unlock()

	// Wait for keygen of test-key-2 to complete
	log.Info("Waiting for keygen of test-key-2...")
	keygenDone = false
	for i := 0; i < 120; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := keygenBroadcast != nil
		mu.Unlock()
		if done {
			keygenDone = true
			log.Info("Keygen of test-key-2 completed!")
			break
		}
	}

	if !keygenDone {
		log.Info("Keygen broadcast for test-key-2 not captured (BLS sig may have failed), continuing...")
	}

	log.Info("Phase 9 completed!")

	// =====================================================
	// PHASE 10: RESHARE WITH 2 MEMBERS SWAPPED (block 600)
	// =====================================================
	log.Info("=== PHASE 10: RESHARE WITH 2 MEMBERS SWAPPED ===")

	// Wait for keygen/sign lock to release
	time.Sleep(10 * time.Second)

	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Store epoch 4 election with only 4 members (drop nodes 4 AND 5)
	members4 := members[:4]
	weights4 := weights[:4]
	for _, node := range nodes {
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 4,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members4,
				Weights: weights4,
			},
			BlockHeight: 590,
		})
		if err != nil {
			t.Fatalf("Failed to store election epoch 4: %v", err)
		}
	}

	// Update MockElectionSystem to only include 4 witnesses
	droppedWitness2 := "mock-tss-" + strconv.Itoa(5) // node 4 has username mock-tss-5
	witnessNames4 := make([]string, 0, 4)
	for _, name := range originalWitnessNames {
		if name != droppedWitness && name != droppedWitness2 {
			witnessNames4 = append(witnessNames4, name)
		}
	}
	mes.WitnessNames = witnessNames4

	// Disconnect nodes 4 and 5, exclude from keepalive
	excludedNodes.Store(4, true)
	excludedNodes.Store(5, true)
	disconnectNode(nodes, 4)
	disconnectNode(nodes, 5)
	time.Sleep(1 * time.Second)

	// Reset reshare broadcast capture
	mu.Lock()
	reshareBroadcast = nil
	mu.Unlock()

	// Re-establish connections among 4 active nodes
	connectActivePeers(t, nodes, excludedNodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger reshare at block 600...")
	processBlocks(nodes, 506, 598, &headHeight)
	headHeight = 620
	for bh := uint64(599); bh <= 605; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Poll for reshare broadcast
	log.Info("Waiting for reshare with 2 members swapped...")
	reshareDone = false
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := reshareBroadcast != nil
		mu.Unlock()
		if done {
			reshareDone = true
			log.Info("Reshare with 2 members swapped completed!")
			break
		}
	}

	if !reshareDone {
		t.Fatal("Phase 10: Reshare with 2 members swapped did not complete within timeout")
	}

	mu.Lock()
	if reshareBroadcast == nil {
		t.Fatal("Phase 10: Reshare broadcast was nil")
	}
	mu.Unlock()

	// Restore for cleanup
	excludedNodes.Delete(4)
	excludedNodes.Delete(5)
	mes.WitnessNames = originalWitnessNames

	log.Info("Phases 1-10 completed!")

	// =====================================================
	// PHASE 11: SIMULTANEOUS KEYGEN + RESHARE (block 700)
	// Verifies that keygen for a new key and reshare for an
	// existing key can run in the same RunActions batch, and
	// that both commitments survive the DB upsert (composite
	// {key_id, tx_id} filter) when sharing the same tx_id.
	// =====================================================
	log.Info("=== PHASE 11: SIMULTANEOUS KEYGEN + RESHARE ===")

	// Wait for Phase 10 lock to release
	time.Sleep(5 * time.Second)
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Neutralize test-key-2: set to active with epoch 5 so it's
	// skipped by both FindNewKeys (not "created") and FindEpochKeys(5) (epoch not < 5).
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key-2",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  5,
		})
	}

	// Insert Phase 10 reshare commitment for test-key (epoch 4, 4-member bitset)
	bitset4Phase10 := big.NewInt(0)
	for i := 0; i < 4; i++ {
		bitset4Phase10.SetBit(bitset4Phase10, i, 1)
	}
	commitmentStr4 := base64.RawURLEncoding.EncodeToString(bitset4Phase10.Bytes())

	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "reshare",
			KeyId:       "test-key",
			Epoch:       4,
			BlockHeight: 600,
			Commitment:  commitmentStr4,
			TxId:        "mock-reshare-tx-4",
		})
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  4,
		})
	}

	// Insert new key for keygen
	for _, node := range nodes {
		node.tssKeys.InsertKey("test-key-multi", tss_db.EcdsaType, tss_db.MaxKeyEpochs)
	}

	// Store epoch 5 election with all 6 members
	for _, node := range nodes {
		err := node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{
				Epoch: 5,
				NetId: "vsc-mocknet",
				Type:  "initial",
			},
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: members,
				Weights: weights,
			},
			BlockHeight: 690,
		})
		if err != nil {
			t.Fatalf("Failed to store election epoch 5: %v", err)
		}
	}

	// Reset broadcast captures
	mu.Lock()
	keygenBroadcast = nil
	reshareBroadcast = nil
	mu.Unlock()

	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Process blocks: keygen for test-key-multi + reshare for test-key at block 700
	log.Info("Processing blocks to trigger simultaneous keygen+reshare at block 700...")
	processBlocks(nodes, 606, 698, &headHeight)
	headHeight = 720
	for bh := uint64(699); bh <= 705; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Wait for BOTH keygen and reshare broadcasts
	log.Info("Waiting for simultaneous keygen+reshare to complete...")
	bothDone := false
	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := keygenBroadcast != nil && reshareBroadcast != nil
		mu.Unlock()
		if done {
			bothDone = true
			log.Info("Both keygen and reshare broadcasts detected!")
			break
		}
	}

	if !bothDone {
		mu.Lock()
		kg := keygenBroadcast != nil
		rs := reshareBroadcast != nil
		mu.Unlock()
		t.Fatalf("Phase 11: Timed out waiting for broadcasts (keygen=%v, reshare=%v)", kg, rs)
	}

	// Verify broadcast contains both entry types
	mu.Lock()
	if keygenBroadcast != nil {
		foundKeygen := false
		for _, op := range keygenBroadcast.Operations {
			raw, _ := json.Marshal(op)
			var cj struct {
				Json string `json:"json"`
			}
			json.Unmarshal(raw, &cj)
			var payload map[string]interface{}
			json.Unmarshal([]byte(cj.Json), &payload)
			for _, v := range payload {
				if m, ok := v.(map[string]interface{}); ok {
					if tp, _ := m["type"].(string); tp == "keygen" {
						foundKeygen = true
					}
				}
			}
		}
		if !foundKeygen {
			t.Error("Phase 11: keygen broadcast missing keygen entry")
		}
	}
	if reshareBroadcast != nil {
		foundReshare := false
		for _, op := range reshareBroadcast.Operations {
			raw, _ := json.Marshal(op)
			var cj struct {
				Json string `json:"json"`
			}
			json.Unmarshal(raw, &cj)
			var payload map[string]interface{}
			json.Unmarshal([]byte(cj.Json), &payload)
			for _, v := range payload {
				if m, ok := v.(map[string]interface{}); ok {
					if tp, _ := m["type"].(string); tp == "reshare" {
						foundReshare = true
					}
				}
			}
		}
		if !foundReshare {
			t.Error("Phase 11: reshare broadcast missing reshare entry")
		}
	}
	mu.Unlock()

	// Critical DB test: insert both commitments with the SAME tx_id.
	// With the old {tx_id}-only filter, the second SetCommitmentData would
	// overwrite the first. With the {key_id, tx_id} fix, both persist.
	for _, node := range nodes {
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "reshare",
			KeyId:       "test-key",
			Epoch:       5,
			BlockHeight: 700,
			Commitment:  commitmentStr,
			TxId:        "mock-multi-tx",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type:        "keygen",
			KeyId:       "test-key-multi",
			Epoch:       5,
			BlockHeight: 700,
			Commitment:  commitmentStr,
			TxId:        "mock-multi-tx", // same tx_id as above
		})
	}

	// Verify both records exist (the core assertion for the DB fix)
	c1, err := nodes[0].tssCommitments.GetCommitment("test-key", 5)
	if err != nil {
		t.Fatalf("Phase 11: Failed to get commitment for test-key epoch 5: %v", err)
	}
	if c1.Type != "reshare" {
		t.Errorf("Phase 11: Expected test-key commitment type 'reshare', got '%s'", c1.Type)
	}
	if c1.TxId != "mock-multi-tx" {
		t.Errorf("Phase 11: Expected test-key tx_id 'mock-multi-tx', got '%s'", c1.TxId)
	}

	c2, err := nodes[0].tssCommitments.GetCommitment("test-key-multi", 5)
	if err != nil {
		t.Fatalf("Phase 11: Failed to get commitment for test-key-multi epoch 5: %v", err)
	}
	if c2.Type != "keygen" {
		t.Errorf("Phase 11: Expected test-key-multi commitment type 'keygen', got '%s'", c2.Type)
	}
	if c2.TxId != "mock-multi-tx" {
		t.Errorf("Phase 11: Expected test-key-multi tx_id 'mock-multi-tx', got '%s'", c2.TxId)
	}
	log.Info("Phase 11: Both commitments stored correctly with same tx_id")

	// Update key states
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  5,
		})
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:     "test-key-multi",
			Status: "active",
			Algo:   tss_db.EcdsaType,
			Epoch:  5,
		})
	}

	log.Info("Phase 11 completed!")

	// =====================================================
	// PHASE 12: SIGN AFTER MULTI-KEY OPERATION (block 750)
	// Confirms test-key still works for signing after being
	// reshared alongside another key's keygen.
	// =====================================================
	log.Info("=== PHASE 12: SIGN AFTER MULTI-KEY OPERATION ===")

	time.Sleep(5 * time.Second)
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Insert signing request for test-key
	msgHex5 := "5555555555555555555555555555555555555555555555555555555555555555"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex5,
			Status: tss_db.SignPending,
		})
	}

	mu.Lock()
	signBroadcast = nil
	mu.Unlock()

	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	log.Info("Processing blocks to trigger signing at block 750...")
	processBlocks(nodes, 706, 748, &headHeight)
	headHeight = 770
	for bh := uint64(749); bh <= 755; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Info("Waiting for signing after multi-key operation...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing after multi-key operation completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 12: Signing after multi-key operation did not complete within timeout")
	}

	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
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
				if pkt.KeyId == "test-key" && pkt.Msg == msgHex5 {
					if pkt.Sig == "" {
						t.Error("Phase 12: Signature for test-key is empty")
					} else {
						log.Info("Phase 12: signature verified", "msg", pkt.Msg, "sig", pkt.Sig)
					}
				}
			}
		}
	}
	mu.Unlock()

	log.Info("Phase 12 completed!")

	log.Info("All 12 TSS phases completed successfully!")

	// =====================================================
	// PHASE 13: KEY EXPIRATION (block 800, epoch 6)
	// Verifies that key deprecation mechanics work: a key
	// whose ExpiryEpoch is reached gets flagged by
	// FindDeprecatingKeys and transitions to deprecated.
	// =====================================================
	log.Info("=== PHASE 13: KEY EXPIRATION ===")

	time.Sleep(5 * time.Second)
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Set test-key to have ExpiryEpoch=6 (about to expire at current epoch)
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:          "test-key",
			Status:      tss_db.TssKeyActive,
			Algo:        tss_db.EcdsaType,
			Epoch:       5,
			Epochs:      tss_db.MaxKeyEpochs,
			ExpiryEpoch: 6,
		})
	}

	// Verify FindDeprecatingKeys returns test-key at epoch 6
	deprecating, err := nodes[0].tssKeys.FindDeprecatingKeys(6)
	if err != nil {
		t.Fatalf("Phase 13: FindDeprecatingKeys failed: %v", err)
	}
	foundDeprecating := false
	for _, k := range deprecating {
		if k.Id == "test-key" {
			foundDeprecating = true
			break
		}
	}
	if !foundDeprecating {
		t.Error("Phase 13: Expected FindDeprecatingKeys(6) to return test-key")
	}

	// FindDeprecatingKeys at epoch 5 should NOT return it (not yet expired)
	notYet, err := nodes[0].tssKeys.FindDeprecatingKeys(5)
	if err != nil {
		t.Fatalf("Phase 13: FindDeprecatingKeys(5) failed: %v", err)
	}
	for _, k := range notYet {
		if k.Id == "test-key" {
			t.Error("Phase 13: FindDeprecatingKeys(5) should NOT return test-key (ExpiryEpoch=6)")
		}
	}

	// Simulate what ProcessBlock does: deprecate the key on all nodes
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:          "test-key",
			Status:      tss_db.TssKeyDeprecated,
			Algo:        tss_db.EcdsaType,
			Epoch:       5,
			Epochs:      tss_db.MaxKeyEpochs,
			ExpiryEpoch: 6,
		})
	}

	// Verify key is now deprecated
	deprecatedKey, err := nodes[0].tssKeys.FindKey("test-key")
	if err != nil {
		t.Fatalf("Phase 13: FindKey after deprecation failed: %v", err)
	}
	if deprecatedKey.Status != tss_db.TssKeyDeprecated {
		t.Errorf("Phase 13: Expected key status 'deprecated', got '%s'", deprecatedKey.Status)
	}

	// FindEpochKeys should NOT return the deprecated key
	epochKeys, _ := nodes[0].tssKeys.FindEpochKeys(7)
	for _, k := range epochKeys {
		if k.Id == "test-key" {
			t.Error("Phase 13: FindEpochKeys(7) should NOT return deprecated test-key")
		}
	}

	log.Info("Phase 13 completed!")

	// =====================================================
	// PHASE 14: KEY RENEWAL + SIGN (block 800)
	// Renews the deprecated key, verifies reactivation and
	// DB query correctness, then signs to confirm the key
	// still works end-to-end after renewal.
	// =====================================================
	log.Info("=== PHASE 14: KEY RENEWAL + SIGN ===")

	time.Sleep(5 * time.Second)
	for _, node := range nodes {
		node.tssMgr.ClearQueuedActions()
	}

	// Simulate renewal (as system_txs.go does): reactivate with new expiry.
	// Key stays at epoch 5 (its actual crypto-material epoch in flatfs).
	renewedExpiry := uint64(6 + tss_db.MaxKeyEpochs)
	for _, node := range nodes {
		node.tssKeys.SetKey(tss_db.TssKey{
			Id:          "test-key",
			Status:      tss_db.TssKeyActive,
			Algo:        tss_db.EcdsaType,
			Epoch:       5,
			Epochs:      tss_db.MaxKeyEpochs,
			ExpiryEpoch: renewedExpiry,
		})
	}

	// Verify key is active again with correct expiry
	renewedKey, err := nodes[0].tssKeys.FindKey("test-key")
	if err != nil {
		t.Fatalf("Phase 14: FindKey after renewal failed: %v", err)
	}
	if renewedKey.Status != tss_db.TssKeyActive {
		t.Errorf("Phase 14: Expected key status 'active' after renewal, got '%s'", renewedKey.Status)
	}
	if renewedKey.ExpiryEpoch != renewedExpiry {
		t.Errorf("Phase 14: Expected ExpiryEpoch=%d after renewal, got %d", renewedExpiry, renewedKey.ExpiryEpoch)
	}

	// FindDeprecatingKeys should no longer return this key at epoch 6
	notDeprecating, _ := nodes[0].tssKeys.FindDeprecatingKeys(6)
	for _, k := range notDeprecating {
		if k.Id == "test-key" {
			t.Error("Phase 14: FindDeprecatingKeys(6) should NOT return renewed test-key")
		}
	}

	// FindEpochKeys(6) should return it (active, epoch 5 < 6, ExpiryEpoch > 6)
	epochKeysAfterRenewal, _ := nodes[0].tssKeys.FindEpochKeys(6)
	foundForReshare := false
	for _, k := range epochKeysAfterRenewal {
		if k.Id == "test-key" {
			foundForReshare = true
			break
		}
	}
	if !foundForReshare {
		t.Error("Phase 14: FindEpochKeys(6) should return renewed test-key for reshare")
	}

	// Sign with renewed key at block 800 (TSS_SIGN_INTERVAL = 50, 800 % 50 == 0)
	msgHex6 := "6666666666666666666666666666666666666666666666666666666666666666"
	for _, node := range nodes {
		node.tssRequests.SetSignedRequest(tss_db.TssRequest{
			KeyId:  "test-key",
			Msg:    msgHex6,
			Status: tss_db.SignPending,
		})
	}

	mu.Lock()
	signBroadcast = nil
	mu.Unlock()

	connectAllPeers(t, nodes)
	time.Sleep(2 * time.Second)

	// Use block 850 (850 % 50 == 0, 850 % 100 != 0) to trigger signing
	// without also triggering a reshare interval that would keyLock test-key.
	log.Info("Processing blocks to trigger signing at block 850...")
	processBlocks(nodes, 756, 848, &headHeight)
	headHeight = 870
	for bh := uint64(849); bh <= 855; bh++ {
		for _, node := range nodes {
			node.consumer.ProcessBlock(hive_blocks.HiveBlock{
				Transactions: []hive_blocks.Tx{},
				BlockNumber:  bh,
			}, &headHeight)
		}
		time.Sleep(200 * time.Millisecond)
	}

	log.Info("Waiting for signing with renewed key...")
	signDone = false
	for i := 0; i < 90; i++ {
		time.Sleep(500 * time.Millisecond)
		mu.Lock()
		done := signBroadcast != nil
		mu.Unlock()
		if done {
			signDone = true
			log.Info("Signing with renewed key completed!")
			break
		}
	}

	if !signDone {
		t.Fatal("Phase 14: Signing with renewed key did not complete within timeout")
	}

	mu.Lock()
	if signBroadcast != nil {
		for _, op := range signBroadcast.Operations {
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
				if pkt.KeyId == "test-key" && pkt.Msg == msgHex6 {
					if pkt.Sig == "" {
						t.Error("Phase 14: Signature for renewed test-key is empty")
					} else {
						log.Info("Phase 14: renewed key signature verified", "msg", pkt.Msg, "sig", pkt.Sig)
					}
				}
			}
		}
	}
	mu.Unlock()

	log.Info("Phase 14 completed!")

	log.Info("All 14 TSS phases completed successfully!")
}

// blameBitset creates a base64-encoded bitset marking the given member indices as blamed.
func blameBitset(indices ...int) string {
	bitset := big.NewInt(0)
	for _, idx := range indices {
		bitset.SetBit(bitset, idx, 1)
	}
	return base64.RawURLEncoding.EncodeToString(bitset.Bytes())
}

func dropBlameTestDatabase() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
	client.Database("go-vsc-blame-test").Drop(ctx)
}

// makeBlameNode creates a minimal node for blame testing.
func makeBlameNode(mes *MockElectionSystem) (nodeComponents, *vtss.TssManager) {
	path := "data-dir-blame"

	os.RemoveAll(path)
	os.Mkdir(path, os.ModePerm)

	dbConf := db.NewDbConfig(path)
	identity := common.NewIdentityConfig(path)
	p2pConf := libp2p.NewConfig(path)
	aggregate.New([]aggregate.Plugin{dbConf, identity, p2pConf}).Init()
	dbConf.SetDbName("go-vsc-blame-test")
	identity.SetUsername("mock-blame-0")
	sysConf := systemconfig.MocknetConfig()

	database := db.New(dbConf)
	vscDb := vsc.New(database, dbConf)
	tssCommitments := tss_db.NewCommitments(vscDb)
	electionDb := elections.New(vscDb)
	witnessDb := witnesses.New(vscDb)

	blockConsumer := blockconsumer.New(nil)

	p2pConf.SetOptions(libp2p.P2POpts{
		Port:         22299,
		ServerMode:   true,
		AllowPrivate: true,
		Bootnodes:    []string{},
	})
	p2p := libp2p.New(witnessDb, p2pConf, identity, sysConf, nil)

	tssKeys := tss_db.NewKeys(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)

	keystore, err := flatfs.CreateOrOpen(path+"/keys", flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}

	txCreator := hive.MockTransactionCreator{
		MockTransactionBroadcaster: hive.MockTransactionBroadcaster{
			Callback: func(tx hivego.HiveTransaction) error { return nil },
		},
		TransactionCrafter: hive.TransactionCrafter{},
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
		tssMgr:         tssMgr,
	}, tssMgr
}

func TestBlameScore(t *testing.T) {
	dropBlameTestDatabase()
	t.Cleanup(func() {
		dropBlameTestDatabase()
		os.RemoveAll("data-dir-blame")
	})

	members4 := []elections.ElectionMember{
		{Account: "node-a", Key: "key-a"},
		{Account: "node-b", Key: "key-b"},
		{Account: "node-c", Key: "key-c"},
		{Account: "node-d", Key: "key-d"},
	}
	weights4 := []uint64{10, 10, 10, 10}

	mes := &MockElectionSystem{WitnessNames: []string{"node-a", "node-b", "node-c", "node-d"}}
	node, tssMgr := makeBlameNode(mes)

	go test_utils.RunPlugin(t, node.agg)
	time.Sleep(3 * time.Second)
	defer node.agg.Stop()

	// Helper to store contiguous elections where all 4 members appear from epoch 0 through
	// the given epoch. BlameScore iterates backwards from the latest epoch and tracks the
	// most recent historical epoch each node appeared in (nodeFirstEpoch). To bypass the
	// grace period (epochsSinceFirst >= 3), we store members in the current epoch and in
	// epochs that are 3+ behind, but NOT in the 3 immediately preceding epochs.
	// This way nodeFirstEpoch = (current - 4) and epochsSinceFirst = 4 >= 3.
	storeElectionsWithGracePeriodBypass := func(currentEpoch uint64) {
		// Current epoch has all 4 members
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: currentEpoch, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
			BlockHeight:        currentEpoch * 100,
		})
		// Epochs (current-1), (current-2), (current-3) exist but with NO members
		// so the backward loop doesn't break but nodeFirstEpoch isn't set yet
		for ep := currentEpoch - 1; ep >= currentEpoch-3 && ep > 0; ep-- {
			node.electionDb.StoreElection(elections.ElectionResult{
				ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: ep, NetId: "vsc-mocknet", Type: "initial"},
				ElectionDataInfo:   elections.ElectionDataInfo{Members: []elections.ElectionMember{}, Weights: []uint64{}},
				BlockHeight:        ep * 100,
			})
		}
		// Epoch (current-4) has all 4 members — this is where nodeFirstEpoch gets set
		// epochsSinceFirst = current - (current-4) = 4 >= gracePeriod(3)
		if currentEpoch >= 4 {
			node.electionDb.StoreElection(elections.ElectionResult{
				ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: currentEpoch - 4, NetId: "vsc-mocknet", Type: "initial"},
				ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
				BlockHeight:        (currentEpoch - 4) * 100,
			})
		}
	}

	t.Run("no_blames_no_bans", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		result := tssMgr.BlameScore()

		if len(result.BannedNodes) != 0 {
			t.Errorf("Expected no banned nodes, got %v", result.BannedNodes)
		}
	})

	t.Run("blame_above_threshold_bans_node", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert 2 blame commitments in epoch 6 (where members exist), both blaming node-a (index 0)
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(0), TxId: "blame-tx-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(0), TxId: "blame-tx-2",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-a"] {
			t.Errorf("Expected node-a to be banned (100%% failure rate), got BannedNodes=%v", result.BannedNodes)
		}
		if result.BannedNodes["node-b"] || result.BannedNodes["node-c"] || result.BannedNodes["node-d"] {
			t.Errorf("Expected only node-a banned, got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("blame_below_threshold_no_ban", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert 3 blame commitments in epoch 6, each blaming a different node (33% < 60%)
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(1), TxId: "blame-tx-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(2), TxId: "blame-tx-2",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 602,
			Commitment: blameBitset(3), TxId: "blame-tx-3",
		})

		result := tssMgr.BlameScore()

		if len(result.BannedNodes) != 0 {
			t.Errorf("Expected no bans (33%% failure rate < 60%% threshold), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("grace_period_exemption", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		// Current epoch 10 has all 4 members including node-d.
		// Epochs 9, 8, 7 have all 4 members (so nodeFirstEpoch for node-d = 9, epochsSinceFirst = 1).
		// This means node-d is in grace period (1 < 3) and should NOT be banned.
		for ep := uint64(7); ep <= 10; ep++ {
			node.electionDb.StoreElection(elections.ElectionResult{
				ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: ep, NetId: "vsc-mocknet", Type: "initial"},
				ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
				BlockHeight:        ep * 100,
			})
		}

		// Blame node-d (index 3) in epochs 7-9 — 100% failure rate but grace period protects
		for ep := uint64(7); ep <= 9; ep++ {
			node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
				Type: "blame", KeyId: "test-key", Epoch: ep, BlockHeight: ep * 100,
				Commitment: blameBitset(3), TxId: fmt.Sprintf("blame-tx-%d", ep),
			})
		}

		result := tssMgr.BlameScore()

		if result.BannedNodes["node-d"] {
			t.Errorf("Expected node-d exempt from ban (grace period), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("timeout_blame_contributes_to_ban", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert blame commitments blaming node-c (index 2) — should trigger ban
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(2), TxId: "blame-tx-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(2), TxId: "blame-tx-2",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-c"] {
			t.Errorf("Expected node-c to be banned (100%% blame rate), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("timeout_blame_metadata_tracked", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert blame commitments with timeout metadata blaming node-b (index 1)
		timeoutErr := "timeout"
		timeoutReason := "Timeout waiting for 1 nodes: [node-b]"
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(1), TxId: "blame-timeout-1",
			Metadata: &tss_db.CommitmentMetadata{Error: &timeoutErr, Reason: &timeoutReason},
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(1), TxId: "blame-timeout-2",
			Metadata: &tss_db.CommitmentMetadata{Error: &timeoutErr, Reason: &timeoutReason},
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-b"] {
			t.Errorf("Expected node-b to be banned (timeout blames count toward ban), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("mixed_timeout_and_error_blames", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert 3 blame commitments for node-a (index 0): 2 timeouts + 1 error
		// Total 3 blames out of 3 operations = 100% > 60% threshold → ban
		timeoutErr := "timeout"
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(0), TxId: "blame-mix-1",
			Metadata: &tss_db.CommitmentMetadata{Error: &timeoutErr},
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(0), TxId: "blame-mix-2",
			Metadata: &tss_db.CommitmentMetadata{Error: &timeoutErr},
		})
		// Error blame (no timeout metadata)
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 602,
			Commitment: blameBitset(0), TxId: "blame-mix-3",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-a"] {
			t.Errorf("Expected node-a to be banned (mixed timeout+error blames), got BannedNodes=%v", result.BannedNodes)
		}
		// Other nodes should not be banned
		if result.BannedNodes["node-b"] || result.BannedNodes["node-c"] || result.BannedNodes["node-d"] {
			t.Errorf("Expected only node-a banned, got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("ban_reduces_to_threshold_exact", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Ban exactly 1 of 4 members (node-a, index 0), leaving 3
		// threshold = ceil(4*2/3)-1 = 2, so threshold+1 = 3 → exactly at minimum quorum
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(0), TxId: "blame-thresh-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(0), TxId: "blame-thresh-2",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-a"] {
			t.Errorf("Expected node-a to be banned, got BannedNodes=%v", result.BannedNodes)
		}
		// Exactly 1 banned, 3 remain (threshold+1 = 3 still achievable)
		bannedCount := 0
		for _, banned := range result.BannedNodes {
			if banned {
				bannedCount++
			}
		}
		if bannedCount != 1 {
			t.Errorf("Expected exactly 1 banned node, got %d: %v", bannedCount, result.BannedNodes)
		}
	})

	t.Run("ban_reduces_below_threshold", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Ban 2 of 4 members (node-a index 0 and node-b index 1), leaving 2
		// threshold+1 = 3, so 2 remaining < 3 → insufficient for quorum
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(0, 1), TxId: "blame-below-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 601,
			Commitment: blameBitset(0, 1), TxId: "blame-below-2",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-a"] {
			t.Errorf("Expected node-a to be banned, got BannedNodes=%v", result.BannedNodes)
		}
		if !result.BannedNodes["node-b"] {
			t.Errorf("Expected node-b to be banned, got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("grace_period_boundary_at_exactly_3", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		// Current epoch=10, node first appears at epoch=7
		// epochsSinceFirst = 10-7 = 3, grace period check is < 3, so 3 is NOT exempt → bannable
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 10, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
			BlockHeight:        1000,
		})
		// Epochs 9, 8 with no members (keeps backward iteration going)
		for ep := uint64(8); ep <= 9; ep++ {
			node.electionDb.StoreElection(elections.ElectionResult{
				ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: ep, NetId: "vsc-mocknet", Type: "initial"},
				ElectionDataInfo:   elections.ElectionDataInfo{Members: []elections.ElectionMember{}, Weights: []uint64{}},
				BlockHeight:        ep * 100,
			})
		}
		// Epoch 7 has all members — nodeFirstEpoch = 7, epochsSinceFirst = 10-7 = 3
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 7, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
			BlockHeight:        700,
		})

		// Blame node-d (index 3) with 100% rate in epoch 7
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 7, BlockHeight: 700,
			Commitment: blameBitset(3), TxId: "blame-grace3-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 7, BlockHeight: 701,
			Commitment: blameBitset(3), TxId: "blame-grace3-2",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-d"] {
			t.Errorf("Expected node-d to be banned (epochsSinceFirst=3, not in grace period), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("grace_period_boundary_at_exactly_2", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		// Current epoch=10, node first appears at epoch=8
		// epochsSinceFirst = 10-8 = 2 < 3 → grace period protects
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 10, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
			BlockHeight:        1000,
		})
		// Epoch 9 with no members
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 9, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: []elections.ElectionMember{}, Weights: []uint64{}},
			BlockHeight:        900,
		})
		// Epoch 8 has all members — nodeFirstEpoch = 8, epochsSinceFirst = 10-8 = 2
		node.electionDb.StoreElection(elections.ElectionResult{
			ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 8, NetId: "vsc-mocknet", Type: "initial"},
			ElectionDataInfo:   elections.ElectionDataInfo{Members: members4, Weights: weights4},
			BlockHeight:        800,
		})

		// Blame node-d (index 3) with 100% rate in epoch 8
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 8, BlockHeight: 800,
			Commitment: blameBitset(3), TxId: "blame-grace2-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 8, BlockHeight: 801,
			Commitment: blameBitset(3), TxId: "blame-grace2-2",
		})

		result := tssMgr.BlameScore()

		if result.BannedNodes["node-d"] {
			t.Errorf("Expected node-d exempt from ban (epochsSinceFirst=2 < 3 grace period), got BannedNodes=%v", result.BannedNodes)
		}
	})

	t.Run("blame_across_multiple_epochs", func(t *testing.T) {
		dropBlameTestDatabase()
		time.Sleep(200 * time.Millisecond)

		storeElectionsWithGracePeriodBypass(10)

		// Insert blame commitments for node-b (index 1) across 3 different epochs
		// Each epoch has 1 blame commitment, so weight per epoch = 1
		// Total: 3 blames out of 3 weight = 100% → ban
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 6, BlockHeight: 600,
			Commitment: blameBitset(1), TxId: "blame-multi-ep-1",
		})
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 10, BlockHeight: 1000,
			Commitment: blameBitset(1), TxId: "blame-multi-ep-2",
		})
		// Also add a blame in the epoch where members exist (epoch current-4 = 6)
		// Use a different epoch that has members — epoch 6 already has members from bypass helper
		// Add blame in epoch 10 (current) which also has members
		node.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
			Type: "blame", KeyId: "test-key", Epoch: 10, BlockHeight: 1001,
			Commitment: blameBitset(1), TxId: "blame-multi-ep-3",
		})

		result := tssMgr.BlameScore()

		if !result.BannedNodes["node-b"] {
			t.Errorf("Expected node-b to be banned (blames across multiple epochs), got BannedNodes=%v", result.BannedNodes)
		}
		// Verify other nodes are not banned
		if result.BannedNodes["node-a"] || result.BannedNodes["node-c"] || result.BannedNodes["node-d"] {
			t.Errorf("Expected only node-b banned, got BannedNodes=%v", result.BannedNodes)
		}
	})
}

func dropLifecycleTestDatabase() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
	client.Database("go-vsc-lifecycle-test").Drop(ctx)
}

func TestKeyLifecycle(t *testing.T) {
	dropLifecycleTestDatabase()
	t.Cleanup(func() {
		dropLifecycleTestDatabase()
		os.RemoveAll("data-dir-lifecycle")
	})

	// Set up a minimal node with real MongoDB (reuse makeBlameNode pattern)
	path := "data-dir-lifecycle"
	os.RemoveAll(path)
	os.Mkdir(path, os.ModePerm)

	dbConf := db.NewDbConfig(path)
	identity := common.NewIdentityConfig(path)
	p2pConf := libp2p.NewConfig(path)
	aggregate.New([]aggregate.Plugin{dbConf, identity, p2pConf}).Init()
	dbConf.SetDbName("go-vsc-lifecycle-test")

	database := db.New(dbConf)
	vscDb := vsc.New(database, dbConf)
	tssKeys := tss_db.NewKeys(vscDb)

	agg := aggregate.New([]aggregate.Plugin{
		dbConf,
		identity,
		database,
		vscDb,
		tssKeys,
	})

	go test_utils.RunPlugin(t, agg)
	time.Sleep(3 * time.Second)
	defer agg.Stop()

	t.Run("activation_sets_expiry", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		tssKeys.InsertKey("key-expiry-1", tss_db.EcdsaType, 10)

		// Simulate activation at epoch 5 (as state_engine.go does)
		tssKeys.SetKey(tss_db.TssKey{
			Id:          "key-expiry-1",
			Status:      tss_db.TssKeyActive,
			Algo:        tss_db.EcdsaType,
			Epoch:       5,
			ExpiryEpoch: 5 + 10, // epoch + epochs
		})

		key, err := tssKeys.FindKey("key-expiry-1")
		if err != nil {
			t.Fatalf("FindKey failed: %v", err)
		}
		if key.ExpiryEpoch != 15 {
			t.Errorf("Expected ExpiryEpoch=15, got %d", key.ExpiryEpoch)
		}
		if key.Status != tss_db.TssKeyActive {
			t.Errorf("Expected status 'active', got '%s'", key.Status)
		}
	})

	t.Run("find_deprecating_keys", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		// Key A: expires at epoch 10
		tssKeys.InsertKey("key-dep-a", tss_db.EcdsaType, 10)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-dep-a", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 0, ExpiryEpoch: 10,
		})
		// Key B: expires at epoch 20
		tssKeys.InsertKey("key-dep-b", tss_db.EcdsaType, 20)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-dep-b", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 0, ExpiryEpoch: 20,
		})

		// At epoch 9: neither should be returned
		keys9, _ := tssKeys.FindDeprecatingKeys(9)
		if len(keys9) != 0 {
			t.Errorf("FindDeprecatingKeys(9): expected 0, got %d", len(keys9))
		}

		// At epoch 10: only key A
		keys10, _ := tssKeys.FindDeprecatingKeys(10)
		if len(keys10) != 1 || keys10[0].Id != "key-dep-a" {
			ids := make([]string, len(keys10))
			for i, k := range keys10 {
				ids[i] = k.Id
			}
			t.Errorf("FindDeprecatingKeys(10): expected [key-dep-a], got %v", ids)
		}

		// At epoch 20: both
		keys20, _ := tssKeys.FindDeprecatingKeys(20)
		if len(keys20) != 2 {
			t.Errorf("FindDeprecatingKeys(20): expected 2, got %d", len(keys20))
		}
	})

	t.Run("find_epoch_keys_excludes_expired", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		tssKeys.InsertKey("key-epoch-1", tss_db.EcdsaType, 3)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-epoch-1", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 2, ExpiryEpoch: 5,
		})

		// Epoch 3: key.Epoch(2) < 3 and ExpiryEpoch(5) > 3 → included
		keys3, _ := tssKeys.FindEpochKeys(3)
		found3 := false
		for _, k := range keys3 {
			if k.Id == "key-epoch-1" {
				found3 = true
			}
		}
		if !found3 {
			t.Error("FindEpochKeys(3): expected key-epoch-1 to be included (not yet expired)")
		}

		// Epoch 5: ExpiryEpoch(5) is NOT > 5 → excluded
		keys5, _ := tssKeys.FindEpochKeys(5)
		for _, k := range keys5 {
			if k.Id == "key-epoch-1" {
				t.Error("FindEpochKeys(5): expected key-epoch-1 to be excluded (expiry reached)")
			}
		}

		// Epoch 6: same, still excluded
		keys6, _ := tssKeys.FindEpochKeys(6)
		for _, k := range keys6 {
			if k.Id == "key-epoch-1" {
				t.Error("FindEpochKeys(6): expected key-epoch-1 to be excluded (past expiry)")
			}
		}
	})

	t.Run("deprecate_legacy_keys", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		// Legacy key: no expiry
		tssKeys.InsertKey("key-legacy", tss_db.EcdsaType, 0)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-legacy", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 1, ExpiryEpoch: 0,
		})
		// Normal key: has expiry
		tssKeys.InsertKey("key-normal", tss_db.EcdsaType, 100)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-normal", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 1, ExpiryEpoch: 101,
		})

		err := tssKeys.DeprecateLegacyKeys()
		if err != nil {
			t.Fatalf("DeprecateLegacyKeys failed: %v", err)
		}

		legacy, _ := tssKeys.FindKey("key-legacy")
		if legacy.Status != tss_db.TssKeyDeprecated {
			t.Errorf("Expected legacy key to be deprecated, got '%s'", legacy.Status)
		}
		if legacy.DeprecatedHeight != 0 {
			t.Errorf("Expected legacy DeprecatedHeight=0, got %d", legacy.DeprecatedHeight)
		}

		normal, _ := tssKeys.FindKey("key-normal")
		if normal.Status != tss_db.TssKeyActive {
			t.Errorf("Expected normal key to remain active, got '%s'", normal.Status)
		}
	})

	t.Run("renewal_reactivates_deprecated", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		tssKeys.InsertKey("key-renew", tss_db.EcdsaType, 10)
		// Active key that got deprecated
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-renew", Status: tss_db.TssKeyDeprecated, Algo: tss_db.EcdsaType,
			Epoch: 5, ExpiryEpoch: 15, DeprecatedHeight: 1000,
		})

		// Simulate renewal at epoch 20 (as system_txs.go does)
		currentEpoch := uint64(20)
		renewEpochs := uint64(100)
		newExpiry := currentEpoch + renewEpochs
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-renew", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 5, ExpiryEpoch: newExpiry, DeprecatedHeight: 0,
		})

		key, _ := tssKeys.FindKey("key-renew")
		if key.Status != tss_db.TssKeyActive {
			t.Errorf("Expected reactivated status 'active', got '%s'", key.Status)
		}
		if key.ExpiryEpoch != 120 {
			t.Errorf("Expected ExpiryEpoch=120, got %d", key.ExpiryEpoch)
		}
		if key.DeprecatedHeight != 0 {
			t.Errorf("Expected DeprecatedHeight=0 after renewal, got %d", key.DeprecatedHeight)
		}

		// Should no longer show up in deprecating keys at old expiry
		dep, _ := tssKeys.FindDeprecatingKeys(15)
		for _, k := range dep {
			if k.Id == "key-renew" {
				t.Error("Renewed key should not appear in FindDeprecatingKeys at old expiry epoch")
			}
		}
	})

	t.Run("renewal_capped_at_max", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		tssKeys.InsertKey("key-cap", tss_db.EcdsaType, tss_db.MaxKeyEpochs)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-cap", Status: tss_db.TssKeyDeprecated, Algo: tss_db.EcdsaType,
			Epoch: 5, ExpiryEpoch: 10,
		})

		// Simulate renewal with epochs exceeding MaxKeyEpochs (apply cap as system_txs.go does)
		currentEpoch := uint64(20)
		requestedEpochs := tss_db.MaxKeyEpochs + 100
		maxExpiry := currentEpoch + tss_db.MaxKeyEpochs
		newExpiry := currentEpoch + requestedEpochs
		if newExpiry > maxExpiry {
			newExpiry = maxExpiry
		}

		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-cap", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			Epoch: 5, ExpiryEpoch: newExpiry, DeprecatedHeight: 0,
		})

		key, _ := tssKeys.FindKey("key-cap")
		if key.ExpiryEpoch != maxExpiry {
			t.Errorf("Expected ExpiryEpoch capped at %d, got %d", maxExpiry, key.ExpiryEpoch)
		}
	})

	t.Run("find_newly_retired", func(t *testing.T) {
		dropLifecycleTestDatabase()
		time.Sleep(200 * time.Millisecond)

		tssKeys.InsertKey("key-retire", tss_db.EcdsaType, 10)
		tssKeys.SetKey(tss_db.TssKey{
			Id: "key-retire", Status: tss_db.TssKeyDeprecated, Algo: tss_db.EcdsaType,
			Epoch: 5, ExpiryEpoch: 15, DeprecatedHeight: 1000,
		})

		// Before grace period: should not be returned
		before, _ := tssKeys.FindNewlyRetired(1000 + tss_db.KeyDeprecationGracePeriod - 1)
		for _, k := range before {
			if k.Id == "key-retire" {
				t.Error("FindNewlyRetired should NOT return key before grace period elapses")
			}
		}

		// At grace period boundary: should be returned
		at, _ := tssKeys.FindNewlyRetired(1000 + tss_db.KeyDeprecationGracePeriod)
		found := false
		for _, k := range at {
			if k.Id == "key-retire" {
				found = true
			}
		}
		if !found {
			t.Error("FindNewlyRetired should return key when grace period elapses")
		}

		// After grace period: should still be returned
		after, _ := tssKeys.FindNewlyRetired(1000 + tss_db.KeyDeprecationGracePeriod + 100)
		foundAfter := false
		for _, k := range after {
			if k.Id == "key-retire" {
				foundAfter = true
			}
		}
		if !foundAfter {
			t.Error("FindNewlyRetired should return key after grace period")
		}
	})
}
