package devnet

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// poaDevnetFloor is the consensus floor the POA fix scenarios run at: 9, the
// line that carries the fixes. POA_DEVNET_FLOOR overrides it; running a tree
// that predates the fixes at POA_DEVNET_FLOOR=7 shows each finding (a binary
// below the floor is dropped from every committee, so each tree runs at the
// floor it implements).
func poaDevnetFloor() uint64 {
	if v, err := strconv.ParseUint(os.Getenv("POA_DEVNET_FLOOR"), 10, 64); err == nil && v > 0 {
		return v
	}
	return 9
}

// poaDisableWitnessAllNodes sets enabled=false on an account's witness rows in
// every node's database, which is what drops it from the candidate set the
// election proposer reads (GetWitnessesAtBlockHeight with EnabledOnly). Written
// to every node because witness records are consensus input and all nodes must
// derive the identical committee. Returns the number of rows matched.
func poaDisableWitnessAllNodes(t *testing.T, d *Devnet, ctx context.Context, account string, nodes int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	total := 0
	for n := 1; n <= nodes; n++ {
		res, err := client.Database(d.nodeDbName(n)).Collection("witnesses").UpdateMany(ctx,
			bson.M{"account": account}, bson.M{"$set": bson.M{"enabled": false}})
		if err != nil {
			t.Fatalf("disabling %s on magi-%d: %v", account, n, err)
		}
		total += int(res.MatchedCount)
	}
	return total
}

// The seat-registry helpers below mirror the ones in the wider POA devnet suite
// under their own names, so both files can live in the package.

// pfSeatDoc is a poa_seats row as stored by the state engine.
type pfSeatDoc struct {
	Account          string `bson:"account"`
	UboId            string `bson:"ubo_id,omitempty"`
	AdmittedHeight   uint64 `bson:"admitted_height"`
	Bootstrap        bool   `bson:"bootstrap,omitempty"`
	LastSeatedHeight uint64 `bson:"last_seated_height"`
	ExitHeight       uint64 `bson:"exit_height"`
}

// pfPoaSeats reads a node's seat registry, sorted by account.
func (d *Devnet) pfPoaSeats(ctx context.Context, node int) ([]pfSeatDoc, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(ctx)

	cur, err := client.Database(d.nodeDbName(node)).Collection("poa_seats").Find(ctx, bson.M{})
	if err != nil {
		return nil, fmt.Errorf("poa_seats find on magi-%d: %w", node, err)
	}
	defer cur.Close(ctx)

	var out []pfSeatDoc
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("poa_seats decode on magi-%d: %w", node, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

func pfMustSeats(t *testing.T, d *Devnet, ctx context.Context, node int) []pfSeatDoc {
	t.Helper()
	s, err := d.pfPoaSeats(ctx, node)
	if err != nil {
		t.Fatalf("poa_seats on magi-%d: %v", node, err)
	}
	return s
}

// pfSeatFingerprint is a compact, comparable form of a seat registry.
func pfSeatFingerprint(seats []pfSeatDoc) string {
	s := ""
	for _, x := range seats {
		s += fmt.Sprintf("%s@%d(bootstrap=%v);", x.Account, x.AdmittedHeight, x.Bootstrap)
	}
	return s
}

// pfAdmitVote broadcasts a vsc.admit_vote from witness voterWitness.
func (d *Devnet) pfAdmitVote(voterWitness int, candidate, uboId string) (string, error) {
	voter := d.witnessAccount(voterWitness)
	payload := map[string]interface{}{
		"candidate": candidate,
		"ubo_id":    uboId,
		"net_id":    d.netId(),
	}
	return d.BroadcastCustomJSON("vsc.admit_vote", []string{voter}, payload, d.cfg.InitminerWIF)
}

// pfMaxSlotHeight is the highest L2 slot a node has stored a block header for.
func (d *Devnet) pfMaxSlotHeight(ctx context.Context, node int) (int, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	var doc struct {
		SlotHeight int `bson:"slot_height"`
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "slot_height", Value: -1}})
	err = client.Database(d.nodeDbName(node)).Collection("block_headers").FindOne(ctx, bson.M{}, opts).Decode(&doc)
	if err != nil {
		return 0, fmt.Errorf("block_headers on magi-%d: %w", node, err)
	}
	return doc.SlotHeight, nil
}
