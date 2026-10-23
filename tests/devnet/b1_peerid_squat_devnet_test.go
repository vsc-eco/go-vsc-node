package devnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestB1PeerIdSquatDevnet (B1, testnet + mainnet 2026-10-22): any account could
// announce a committee member's libp2p peer id. TSS attributed a round message to
// the FIRST account the peer-id lookup returned (Go map order), so the member's
// messages were credited to the squatter and dropped, and its sessions failed.
//
// Here initminer (not a committee member) announces magi-2's peer id with a valid
// BLS key and PoP but no peer-id proof. Then the next two reshares must complete
// with all four members:
//   - below 0.12 the squat is stored (precondition checked) and the fix's
//     session-party attribution must still credit magi-2 (the old code fails here);
//   - from 0.12 the squat is rejected at ingest. First, magi-2 is restarted so it
//     re-announces with the gate on: its proof must verify (no lock-out of an honest
//     node; startup announcements predate the genesis election, so they ran ungated).
func TestB1PeerIdSquatDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(4)
	cfg.GenesisNode = 4 // genesis = last node (DefaultConfig invariant); must be 1..Nodes
	floor := poaDevnetFloor()
	d, ctx := startDevnet(t, cfg, 100*time.Minute)
	t.Cleanup(func() {
		tssDumpLogs(t, d, cfg.Nodes, "peer_id proof|failed to get witness|reshare participant selection|blame")
	})
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("floor %d, keygen at %d", floor, keygen.BlockHeight)

	victim := d.witnessAccount(2)
	vrow := latestWitnessRow(t, d, ctx, 1, victim)
	victimPeer, _ := vrow["peer_id"].(string)
	netId, _ := vrow["net_id"].(string)
	if victimPeer == "" {
		t.Fatalf("PRECONDITION FAILED: no peer id announced for %s", victim)
	}

	if floor >= 12 {
		// An honest announcement with the gate on must be stored with a verified proof.
		h0, _ := getHeadBlock(d.HiveRPCEndpoint())
		if err := d.StopNode(ctx, 2); err != nil {
			t.Fatalf("stop magi-2: %v", err)
		}
		devnetSleep(5 * time.Second)
		if err := d.StartNode(ctx, 2); err != nil {
			t.Fatalf("start magi-2: %v", err)
		}
		var proven bson.M
		deadline := devnetNow().Add(5 * time.Minute)
		for devnetNow().Before(deadline) {
			if r := latestWitnessRowOrNil(d, ctx, 1, victim); r != nil && heightOf(r) > uint64(h0) {
				if ok, _ := r["peer_id_pop_ok"].(bool); ok {
					proven = r
					break
				}
			}
			devnetSleep(5 * time.Second)
		}
		if proven == nil {
			t.Fatalf("0.12: %s re-announced after the restart but no stored row above %d has a verified peer-id proof (honest node locked out?)", victim, h0)
		}
		if proven["peer_id"] != victimPeer {
			t.Fatalf("0.12: %s re-announced peer id %v, was %s", victim, proven["peer_id"], victimPeer)
		}
		rh := heightOf(proven)
		c, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": rh}}, 20*time.Minute)
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: no reshare after %s's proven re-announcement: %v", victim, err)
		}
		if named := namedIn(t, d, ctx, *c); len(named) != 4 {
			t.Fatalf("PRECONDITION FAILED: reshare at %d after the restart named %v, want all 4", c.BlockHeight, named)
		}
		t.Logf("0.12: %s re-announced at %d with a verified peer-id proof; reshare at %d named all four", victim, rh, c.BlockHeight)
	}

	// The squat: a valid announcement in every respect except the peer-id proof.
	// Witness disabled, as in the live repro (lumen-qpub): no candidacy, and neither
	// peer-id lookup filters on it.
	did, priv := freshBlsKey(t, "b1_peerid_squatter_seed_xxxxxxx")
	pop, err := dids.GenerateBlsPoP(priv, "initminer")
	if err != nil {
		t.Fatalf("bls pop: %v", err)
	}
	meta := map[string]any{
		"services": []string{"vsc.network"},
		"did_keys": []map[string]any{{"t": "consensus", "ct": "DID-BLS", "key": did.String(), "pop": pop}},
		"vsc_node": map[string]any{"net_id": netId, "peer_id": victimPeer, "peer_addrs": []string{},
			"ts": time.Now().Format(time.RFC3339), "witness": map[string]any{"enabled": false}},
	}
	metaJSON, _ := json.Marshal(meta)
	memo := accountMemoKey(t, d, "initminer")
	client := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	client.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
	wif := d.cfg.InitminerWIF
	txId, err := broadcastRetryingStale(client, []hivego.HiveOperation{hivego.AccountUpdateOperation{
		Account: "initminer", MemoKey: memo, JsonMetadata: string(metaJSON),
	}}, &wif, "b1 squat announce")
	if err != nil {
		t.Fatalf("broadcast squat: %v", err)
	}
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("initminer announced %s's peer id %s (tx %s, near block %d)", victim, victimPeer, txId, head)

	// Let every node ingest it.
	deadline := devnetNow().Add(3 * time.Minute)
	stored := false
	for devnetNow().Before(deadline) {
		if r := latestWitnessRowOrNil(d, ctx, 1, "initminer"); r != nil && r["peer_id"] == victimPeer {
			stored = true
			break
		}
		if floor >= 12 && nodeLogsContain(d, context.Background(), 1, "peer_id proof of possession failed (B1)") {
			break
		}
		devnetSleep(5 * time.Second)
	}
	// Every node must reach the same verdict on the announcement.
	storedOn := 0
	for n := 1; n <= cfg.Nodes; n++ {
		if r := latestWitnessRowOrNil(d, ctx, n, "initminer"); r != nil && r["peer_id"] == victimPeer {
			storedOn++
		}
	}
	if storedOn != 0 && storedOn != cfg.Nodes {
		t.Fatalf("the squat is stored on %d of %d nodes: ingest verdict differs between nodes", storedOn, cfg.Nodes)
	}
	if floor >= 12 {
		if stored || storedOn != 0 {
			t.Fatalf("0.12: the squat (no peer-id proof) was stored")
		}
		if !nodeLogsContain(d, context.Background(), 1, "peer_id proof of possession failed (B1)") {
			t.Fatalf("0.12: the squat was not stored, but no rejection was logged either (did the announcement land?)")
		}
		t.Logf("0.12: squat rejected at ingest on every node")
	} else if !stored {
		t.Fatalf("PRECONDITION FAILED (floor %d): the squat was never stored, so attribution is not exercised", floor)
	} else {
		t.Logf("floor %d: squat stored (precondition)", floor)
	}

	// Two reshares after the squat, each with all four members (magi-2's messages
	// must be credited to magi-2).
	after := uint64(head)
	for i := 1; i <= 2; i++ {
		c, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": after}}, 20*time.Minute)
		if err != nil {
			t.Fatalf("reshare %d after the squat never landed (%v): %s's messages were not attributed to it", i, err, victim)
		}
		named := namedIn(t, d, ctx, *c)
		if len(named) != 4 || !sliceHas(named, victim) {
			t.Fatalf("reshare %d at %d named %v, want all 4 incl. %s", i, c.BlockHeight, named, victim)
		}
		t.Logf("reshare %d at %d named all four", i, c.BlockHeight)
		after = c.BlockHeight
	}
}

func heightOf(row bson.M) uint64 {
	switch h := row["height"].(type) {
	case int64:
		return uint64(h)
	case int32:
		return uint64(h)
	case float64:
		return uint64(h)
	}
	return 0
}

func latestWitnessRowOrNil(d *Devnet, ctx context.Context, node int, account string) bson.M {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil
	}
	defer client.Disconnect(ctx)
	var row bson.M
	err = client.Database(d.nodeDbName(node)).Collection("witnesses").FindOne(ctx, bson.M{"account": account},
		options.FindOne().SetSort(bson.D{{Key: "height", Value: -1}})).Decode(&row)
	if err != nil {
		return nil
	}
	return row
}

func latestWitnessRow(t *testing.T, d *Devnet, ctx context.Context, node int, account string) bson.M {
	t.Helper()
	row := latestWitnessRowOrNil(d, ctx, node, account)
	if row == nil {
		t.Fatalf("no witness row for %s on magi-%d", account, node)
	}
	return row
}

// accountMemoKey reads an account's memo key (account_update must carry it).
func accountMemoKey(t *testing.T, d *Devnet, account string) string {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"condenser_api.get_accounts","params":[["%s"]],"id":1}`, account)
	resp, err := http.Post(d.HiveRPCEndpoint(), "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("get_accounts: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Result []struct {
			MemoKey string `json:"memo_key"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Result) == 0 {
		t.Fatalf("get_accounts %s: %v", account, err)
	}
	return out.Result[0].MemoKey
}
