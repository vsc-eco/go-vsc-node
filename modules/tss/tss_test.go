// TSS tests: run in isolation (no full VSC node) with:
//
//	go test -short ./modules/tss/helpers/...   # unit tests only (GetThreshold, MsgToHashInt)
//	go test -short ./modules/tss/...           # same + tss package unit tests (e.g. makeEpochIdx); skips TestVtss
//	go test ./modules/tss/... -run TestVtss    # full 3-node integration (no -short); requires full build
package tss_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/db"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	vtss "vsc-node/modules/tss"
	tss_helpers "vsc-node/modules/tss/helpers"

	flatfs "github.com/ipfs/go-ds-flatfs"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	// "vsc-node/modules/tss"
)

type MockElectionSystem struct {
	ActiveWitnesses map[string]stateEngine.Witness
}

// waitForPeersConnected blocks until each P2P server has at least minPeers connected, or timeout.
func waitForPeersConnected(t *testing.T, pts []*libp2p.P2PServer, minPeers int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allGood := true
		for _, p := range pts {
			if len(p.Network().Peers()) < minPeers {
				allGood = false
				break
			}
		}
		if allGood {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Timeout: report current peer counts
	for i, p := range pts {
		t.Logf("node %d peers: %d (need %d)", i, len(p.Network().Peers()), minPeers)
	}
	t.Fatalf("peers not connected within %v (need %d per node)", timeout, minPeers)
}

func (mes *MockElectionSystem) GetSchedule(blockHeight uint64) []stateEngine.WitnessSlot {
	witnesses := make([]stateEngine.Witness, 0)
	for key := range mes.ActiveWitnesses {
		witnesses = append(witnesses, stateEngine.Witness{
			Account: key,
		})
	}

	list := make([]stateEngine.WitnessSlot, 0)
	for x := 0; x < 5; x++ {
		modl := (int(blockHeight) + x) % len(witnesses)
		dl := (blockHeight % 10)
		dx := blockHeight - dl
		list = append(list, stateEngine.WitnessSlot{
			Account:    witnesses[modl].Account,
			SlotHeight: uint64(int(dx) + x*10),
		})
	}
	return list
}

type testNode struct {
	agg      *aggregate.Aggregate
	vstr     *blockconsumer.HiveConsumer
	witness  witnesses.Witnesses
	p2p      *libp2p.P2PServer
	election elections.Elections
	identity common.IdentityConfig
	tss      *vtss.TssManager
}

func MakeNode(index int, mes *MockElectionSystem) testNode {
	path := "data-dir-" + strconv.Itoa(index)

	os.Mkdir(path, os.ModePerm)
	identity := common.NewIdentityConfig(path)
	identity.Init()
	identity.SetUsername("e2e-" + strconv.Itoa(index))
	dbConf := db.NewDbConfig()
	sconf := systemconfig.MocknetConfig()

	db := db.New(dbConf)
	vscDb := vsc.New(db, "vsc-tss-test-"+strconv.Itoa(index))
	tssKeys := tss_db.NewKeys(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	electionDb := elections.New(vscDb)
	witnesses := witnesses.New(vscDb)
	hiveConsumer := blockconsumer.New(nil)

	p2p := libp2p.New(witnesses, identity, sconf, nil, 22222+index)

	keystore, err := flatfs.CreateOrOpen(path+"/keys", flatfs.Prefix(1), false)
	if err != nil {
		panic(err)
	}
	tssMgr := vtss.New(p2p, tssKeys, tssRequests, tssCommitments, witnesses, electionDb, hiveConsumer, mes, identity, keystore, nil)

	agg := aggregate.New([]aggregate.Plugin{
		identity,
		dbConf,
		db,
		vscDb,
		tssKeys,
		tssRequests,
		tssCommitments,
		electionDb,
		witnesses,

		p2p,

		tssMgr,
	})

	return testNode{agg: agg, vstr: hiveConsumer, witness: witnesses, p2p: p2p, election: electionDb, identity: identity, tss: tssMgr}
}

func TestVtss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full TSS integration test in short mode (use: go test ./modules/tss/... without -short)")
	}
	// Full integration test requires MongoDB (aggregate uses db plugin)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://127.0.0.1:27017"))
	if err != nil {
		cancel()
		t.Skipf("TestVtss requires MongoDB on 127.0.0.1:27017: %v", err)
	}
	if err := mongoClient.Ping(ctx, nil); err != nil {
		_ = mongoClient.Disconnect(context.Background())
		cancel()
		t.Skipf("TestVtss requires MongoDB on 127.0.0.1:27017: %v", err)
	}
	_ = mongoClient.Disconnect(context.Background())
	cancel()

	mes := &MockElectionSystem{
		ActiveWitnesses: map[string]stateEngine.Witness{
			"e2e-0": {},
			"e2e-1": {},
			"e2e-2": {},
		},
	}

	// Create 3 test nodes
	nodes := make([]testNode, 3)
	for x := 0; x < 3; x++ {
		nodes[x] = MakeNode(x, mes)
		go test_utils.RunPlugin(t, nodes[x].agg)
	}

	// Wait for all P2P servers to be started
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	for _, n := range nodes {
		if _, err := n.p2p.Started().Await(waitCtx); err != nil {
			waitCancel()
			t.Fatalf("p2p started: %v", err)
		}
	}
	waitCancel()
	time.Sleep(2 * time.Second)

	// Get each node's BLS DID for election member keys
	blsDIDs := make([]string, 3)
	for i, n := range nodes {
		did, err := n.identity.BlsDID()
		if err != nil {
			t.Fatalf("get BLS DID for node %d: %v", i, err)
		}
		blsDIDs[i] = string(did)
		t.Logf("node %d BLS DID: %s", i, blsDIDs[i])
	}

	// Seed witness records (account → peer_id) so TSS P2P can resolve peers.
	for _, n := range nodes {
		for i, other := range nodes {
			err := n.witness.SetWitnessUpdate(witnesses.SetWitnessUpdateType{
				Account: "e2e-" + strconv.Itoa(i),
				Height:  1,
				Metadata: witnesses.PostingJsonMetadata{
					DidKeys: []witnesses.PostingJsonKeys{{Type: "consensus", Key: blsDIDs[i]}},
					VscNode: witnesses.PostingJsonMetadataVscNode{
						PeerId: other.p2p.ID().String(),
					},
				},
			})
			if err != nil {
				t.Fatalf("seed witness e2e-%d: %v", i, err)
			}
		}
	}

	// Connect nodes using explicit 127.0.0.1 addrs (hosts listen on 0.0.0.0)
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 30*time.Second)
	const basePort = 22222
	for i, n := range nodes {
		for ix, other := range nodes {
			if ix == i {
				continue
			}
			port := basePort + ix
			tcpAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port))
			quicAddr, _ := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/udp/%d/quic-v1", port))
			addrInfo := peer.AddrInfo{ID: other.p2p.ID(), Addrs: []multiaddr.Multiaddr{tcpAddr, quicAddr}}
			if err := n.p2p.Connect(connectCtx, addrInfo); err != nil {
				connectCancel()
				t.Fatalf("connect node %d -> %d: %v", i, ix, err)
			}
		}
	}
	connectCancel()

	pts := make([]*libp2p.P2PServer, len(nodes))
	for i, n := range nodes {
		pts[i] = n.p2p
	}
	waitForPeersConnected(t, pts, 2, 45*time.Second)

	// Store election with BLS keys and weights so waitForSigs can collect signatures
	electionMembers := make([]elections.ElectionMember, 3)
	electionWeights := make([]uint64, 3)
	for i := range nodes {
		electionMembers[i] = elections.ElectionMember{
			Account: "e2e-" + strconv.Itoa(i),
			Key:     blsDIDs[i],
		}
		electionWeights[i] = 1
	}

	for _, n := range nodes {
		n.election.StoreElection(elections.ElectionResult{
			BlockHeight: 1,
			ElectionDataInfo: elections.ElectionDataInfo{
				Members: electionMembers,
				Weights: electionWeights,
			},
		})
	}

	// Queue keygen on each node
	for _, n := range nodes {
		n.tss.KeyGen("test-key", tss_helpers.SigningAlgoEcdsa)
	}

	// Feed blocks starting at TSS_ACTIVATE_HEIGHT so BlockTick runs
	done := make(chan struct{})
	go func() {
		bh := uint64(vtss.TSS_ACTIVATE_HEIGHT)
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, n := range nodes {
				n.vstr.ProcessBlock(hive_blocks.HiveBlock{
					Transactions: []hive_blocks.Tx{},
					BlockNumber:  bh,
				}, &bh)
			}
			time.Sleep(3 * time.Second)
			bh++
		}
	}()

	// Wait for keygen to complete: poll for "Hex public key" in session results
	// The keygen produces a public key within ~20s if P2P works; give it 2 minutes.
	t.Log("Waiting for TSS keygen to complete...")
	keygenDone := false
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		// Check if any node's TSS produced a keygen result by checking the key in the DB
		for _, n := range nodes {
			key, err := n.tss.GetKeygenResult("test-key")
			if err == nil && key != "" {
				t.Logf("TSS keygen succeeded! Public key: %s", key)
				keygenDone = true
				break
			}
		}
		if keygenDone {
			break
		}
	}
	close(done)

	if !keygenDone {
		t.Fatal("TSS keygen did not complete within 2 minutes")
	}
}

// TestReshareSingleNodeFailure tests reshare behavior when one node fails mid-process
// Steps:
// 1. Start 5-node cluster
// 2. Trigger reshare
// 3. Kill 1 node mid-reshare
// 4. Observe behavior: timeout, blame, retry
// Expected: Reshare completes with remaining 4 nodes, failed node blamed
func TestReshareSingleNodeFailure(t *testing.T) {
	t.Skip("Integration test - requires local testnet setup")
	// TODO: Implement with local testnet
	// - Set up 5-node cluster
	// - Trigger reshare via KeyReshare
	// - Kill one node process
	// - Verify reshare completes
	// - Check blame commitments for failed node
	// - Verify automatic retry scheduled
}

// TestReshareNetworkPartition tests reshare behavior during network partition
// Steps:
// 1. Start 5-node cluster
// 2. Partition network: 2 nodes isolated from 3 nodes
// 3. Trigger reshare
// 4. Observe both partitions
// Expected: Larger partition completes, smaller partition times out and blames
func TestReshareNetworkPartition(t *testing.T) {
	t.Skip("Integration test - requires local testnet setup")
	// TODO: Implement with network simulation
	// - Use iptables or network namespace to partition nodes
	// - Trigger reshare
	// - Verify behavior in both partitions
}

// TestReshareStaggeredNodeStartup tests message buffering for late-arriving nodes
// Steps:
// 1. Start 3 nodes
// 2. Trigger reshare
// 3. Start 2 more nodes during reshare
// 4. Observe message handling
// Expected: Late nodes receive buffered messages or reshare completes without them
func TestReshareStaggeredNodeStartup(t *testing.T) {
	t.Skip("Integration test - requires local testnet setup")
	// TODO: Implement staggered startup
	// - Start subset of nodes
	// - Trigger reshare
	// - Start remaining nodes
	// - Verify message buffering/replay works
}

// TestReshareHighLatencyNetwork tests reshare with network delays
// Steps:
// 1. Configure network delay (100-500ms) between nodes
// 2. Trigger reshare
// 3. Observe timeout behavior
// Expected: Reshare completes despite delays, or timeout increases appropriately
func TestReshareHighLatencyNetwork(t *testing.T) {
	t.Skip("Integration test - requires network delay simulation")
	// TODO: Implement with tc (traffic control) or similar
	// - Add network delay between nodes
	// - Trigger reshare
	// - Verify completion or appropriate timeout
}

// TestReshareRapidNodeChurn tests stability during node restarts
// Steps:
// 1. Start 5-node cluster
// 2. Rapidly restart nodes (1 at a time, every 10 seconds)
// 3. Trigger reshare during churn
// 4. Observe stability
// Expected: System handles churn gracefully, reshares succeed eventually
func TestReshareRapidNodeChurn(t *testing.T) {
	t.Skip("Integration test - requires local testnet setup")
	// TODO: Implement node restart simulation
	// - Restart nodes in sequence
	// - Trigger reshare during churn
	// - Verify eventual success
}

// TestReshareMessageRetry tests retry mechanism for failed messages
func TestReshareMessageRetry(t *testing.T) {
	// Unit test for retry logic
	// This can be tested without full testnet
	t.Skip("TODO: Implement unit test for retry logic")
}

// TestReshareMessageBuffering tests message buffering for early messages
func TestReshareMessageBuffering(t *testing.T) {
	// Unit test for message buffering
	t.Skip("TODO: Implement unit test for message buffering")
}

// TestReshareParticipantReadiness tests readiness check logic
func TestReshareParticipantReadiness(t *testing.T) {
	// Unit test for readiness checks
	t.Skip("TODO: Implement unit test for participant readiness")
}

// TestBanProtocolGracePeriod tests grace period for new nodes
func TestBanProtocolGracePeriod(t *testing.T) {
	// Unit test for ban protocol
	t.Skip("TODO: Implement unit test for ban grace period")
}

// func TestP2p(t *testing.T) {
// 	fmt.Println("P2P Test started")
// 	dbConfig := db.NewDbConfig()
// 	db := db.New(dbConfig)
// 	vscDb := vsc.New(db, "vsc-tss")
// 	witnessDb := witnesses.New(vscDb)
// 	identityConfig := vcommon.NewIdentityConfig("data-tss" + "/config")

// 	identityConfig.Init()

// 	systemConfig := vcommon.SystemConfig{}

// 	p2pServer := libp2p.New(witnessDb, identityConfig, systemConfig, 10722)
// 	err := p2pServer.Init()
// 	if err != nil {
// 		t.Fatalf("Failed to initialize P2P server: %v", err)
// 	}

// 	fmt.Println("P2P Test completed")
// 	tssMgr := vtss.New(p2pServer)

// 	agg := []aggregate.Plugin{
// 		dbConfig,
// 		db,
// 		vscDb,
// 		witnessDb,
// 		identityConfig,
// 		p2pServer,
// 		tssMgr,
// 	}

// 	test_utils.RunPlugin(t, aggregate.New(agg))
// }
