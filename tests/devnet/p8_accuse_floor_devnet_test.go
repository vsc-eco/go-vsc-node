package devnet

import (
	"context"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestP8AccusationYieldsToFloorFourMembers (P8, testnet 2026-10-22): in a 4-member
// committee one landed reshare accusation left the accused out of every later
// reshare of the key. 3 parties lower the threshold below the Item-4 floor, so
// every attempt was refused ("reshare would lower the key threshold") for the
// whole blame window, even with the accused healthy, because only a successful
// reshare clears the accusation.
//
// The accused here is healthy and ready the whole time; one accusation naming it
// is landed after the key's first rotation (synthetic row, as in
// TestPoaAccusedOldMemberAtMinimumExcludesNobody).
//   - floor >= 12 (TssAccuseYieldsToFloorActive): the exclusion yields, the next
//     reshare runs with all four members.
//   - floor < 12: the wedge: no reshare, and the floor refusal naming the accused.
//
// Run with POA_DEVNET_FLOOR=12 on a build whose running version is 0.12.
func TestP8AccusationYieldsToFloorFourMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := poaTssConfig(4)
	cfg.GenesisNode = 4 // genesis = last node (DefaultConfig invariant); must be 1..Nodes
	floor := poaDevnetFloor()
	d, ctx := startDevnet(t, cfg, 75*time.Minute)
	t.Cleanup(func() {
		tssDumpLogs(t, d, cfg.Nodes, "reshare would lower|accusation exclusions|reshare participant selection")
	})
	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("floor %d, keygen at %d", floor, keygen.BlockHeight)

	r1, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(keygen.BlockHeight)}}, 20*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no first reshare after the keygen: %v", err)
	}
	if named := namedIn(t, d, ctx, *r1); len(named) != 4 {
		t.Fatalf("PRECONDITION FAILED: first reshare at %d named %v, want all 4", r1.BlockHeight, named)
	}
	t.Logf("R1 at %d epoch %d with all four", r1.BlockHeight, r1.Epoch)

	accused := d.witnessAccount(4)
	ep, _ := latestElection(t, d, ctx, 1)
	mem, err := d.GetElectionMembers(ctx, 1, ep)
	if err != nil {
		t.Fatalf("members of epoch %d: %v", ep, err)
	}
	bit := -1
	for i, m := range mem {
		if strings.TrimPrefix(m, "hive:") == accused {
			bit = i
		}
	}
	if bit < 0 || len(mem) != 4 {
		t.Fatalf("PRECONDITION FAILED: election %d is %v, want 4 members incl. %s", ep, mem, accused)
	}
	row := bson.M{
		"type": "reshare_accuse", "key_id": "test-key-main", "block_height": r1.BlockHeight + 1, "epoch": ep,
		"commitment": base64.RawURLEncoding.EncodeToString(new(big.Int).SetBit(new(big.Int), bit, 1).Bytes()),
		"tx_id":      "devnet-synthetic-p8",
	}
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := client.Database(d.nodeDbName(n)).Collection("tss_commitments").InsertOne(ctx, row); err != nil {
			t.Fatalf("insert on magi-%d: %v", n, err)
		}
	}
	client.Disconnect(ctx)
	t.Logf("one accusation naming %s (healthy, ready) landed after R1 on every node", accused)

	r2, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": r1.BlockHeight}}, 25*time.Minute)
	refused := false
	for n := 1; n <= cfg.Nodes; n++ {
		if nodeLogsContain(d, context.Background(), n, "reshare would lower the key threshold") {
			refused = true
		}
	}
	if floor >= 12 {
		if err != nil {
			t.Fatalf("0.12: no reshare after the accusation (%v): the exclusion did not yield to the floor (refusal logged: %v)", err, refused)
		}
		named := namedIn(t, d, ctx, *r2)
		if len(named) != 4 || !sliceHas(named, accused) {
			t.Fatalf("0.12: reshare at %d named %v, want all 4 incl. the accused %s", r2.BlockHeight, named, accused)
		}
		t.Logf("0.12: R2 at %d named all four, the accusation yielded (refusal logged before it: %v)", r2.BlockHeight, refused)
		return
	}
	if err == nil {
		named := namedIn(t, d, ctx, *r2)
		t.Fatalf("floor %d: expected the wedge, but a reshare landed at %d naming %v", floor, r2.BlockHeight, named)
	}
	if !refused {
		t.Fatalf("floor %d: no reshare, but no node logged the floor refusal either: wedge not proven", floor)
	}
	t.Logf("floor %d: wedge reproduced: no reshare for 25 min, floor refusal logged with %s excluded", floor, accused)
}
