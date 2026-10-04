package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// breakGatewayPoPQuiet is breakGatewayPoPEverywhere without t.Fatalf, for use from
// a background goroutine that is stopped by cancelling ctx.
func breakGatewayPoPQuiet(ctx context.Context, d *Devnet, account string, nodes int) int {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0
	}
	defer client.Disconnect(context.Background())
	total := 0
	for n := 1; n <= nodes; n++ {
		res, err := client.Database(d.nodeDbName(n)).Collection("witnesses").UpdateMany(ctx,
			bson.M{"account": account},
			bson.M{"$set": bson.M{"gateway_key_pop": "3045022100deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0220cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"}})
		if err != nil {
			return total
		}
		total += int(res.MatchedCount)
	}
	return total
}

// TestPoaBootstrapProvesKeys: a member of the committee before the switch holds a
// gateway key with an INVALID proof of possession when the floor rises. At 0.9.0
// bootstrap checks both proofs before writing a seat (PoaBootstrapProvesKeysActive),
// so the member gets no seat, and the first POA-gated election drops it. Below
// 0.9.0 (POA_DEVNET_FLOOR=7, the line testnet bootstrapped at) bootstrap seeds it
// exactly as the 0.7.0 build did.
//
// The proof is broken on every node (witness rows are consensus input) and broken
// again every few seconds until the switch, so a re-announce cannot quietly repair
// it before bootstrap reads it.
func TestPoaBootstrapProvesKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	const floorEpoch = 4
	floor := poaDevnetFloor()
	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 1
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = floor
	cp.ConsensusVersionFloorEpoch = floorEpoch
	d, ctx := startDevnetNoKey(t, cfg, 60*time.Minute)

	victim := d.witnessAccount(cfg.Nodes)
	bare := func(ms []string) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = strings.TrimPrefix(m, "hive:")
		}
		return out
	}

	if err := d.waitForElectionEpoch(ctx, 1, floorEpoch-1, 25*time.Minute); err != nil {
		t.Fatalf("never reached epoch %d: %v", floorEpoch-1, err)
	}
	prev, err := d.GetElectionMembers(ctx, 1, floorEpoch-1)
	if err != nil {
		t.Fatalf("members of epoch %d: %v", floorEpoch-1, err)
	}
	if !contains(bare(prev), victim) {
		t.Fatalf("PRECONDITION FAILED: %s is not in the pre-POA committee of epoch %d: %v", victim, floorEpoch-1, bare(prev))
	}
	if seats := pfMustSeats(t, d, ctx, 1); len(seats) != 0 {
		t.Fatalf("PRECONDITION FAILED: seat registry not empty before the switch: %s", pfSeatFingerprint(seats))
	}
	t.Logf("pre-POA committee epoch %d: %v; floor 0.%d at epoch %d", floorEpoch-1, bare(prev), floor, floorEpoch)

	breakCtx, stopBreaking := context.WithCancel(ctx)
	broken := make(chan int, 1)
	go func() {
		first := 0
		for {
			if n := breakGatewayPoPQuiet(breakCtx, d, victim, cfg.Nodes); first == 0 && n > 0 {
				first = n
				broken <- n
			}
			select {
			case <-breakCtx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()
	select {
	case n := <-broken:
		t.Logf("broke the gateway proof of %s on %d witness row(s), re-breaking every 3 s", victim, n)
	case <-time.After(2 * time.Minute):
		stopBreaking()
		t.Fatalf("PRECONDITION FAILED: no witness rows for %s to break", victim)
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, floorEpoch+1, 20*time.Minute); err != nil {
			stopBreaking()
			t.Fatalf("magi-%d never reached epoch %d: %v", n, floorEpoch+1, err)
		}
	}
	stopBreaking()

	memT, _ := d.GetElectionMembers(ctx, 1, floorEpoch)
	memT1, _ := d.GetElectionMembers(ctx, 1, floorEpoch+1)
	t.Logf("transition committee epoch %d: %v", floorEpoch, bare(memT))
	t.Logf("first POA-gated committee epoch %d: %v", floorEpoch+1, bare(memT1))

	seats := pfMustSeats(t, d, ctx, 1)
	fp := pfSeatFingerprint(seats)
	t.Logf("seat registry (magi-1): %s", fp)
	for n := 2; n <= cfg.Nodes; n++ {
		if other := pfSeatFingerprint(pfMustSeats(t, d, ctx, n)); other != fp {
			t.Errorf("seat registry differs on magi-%d: %s", n, other)
		}
	}
	if len(seats) == 0 {
		t.Fatalf("PRECONDITION FAILED: bootstrap never seeded the seat registry")
	}
	seated := map[string]bool{}
	for _, s := range seats {
		seated[s.Account] = true
	}

	if floor >= 9 {
		if seated[victim] {
			t.Errorf("%s holds a seat although its gateway key has no valid proof of possession (registry %s)", victim, fp)
		}
		for _, m := range bare(prev) {
			if m != victim && !seated[m] {
				t.Errorf("%s was in the pre-POA committee with valid proofs but got no seat (registry %s)", m, fp)
			}
		}
		if contains(bare(memT1), victim) {
			t.Errorf("%s has no seat but is in the first POA-gated committee: %v", victim, bare(memT1))
		}
		if !t.Failed() {
			t.Logf("RESULT 0.%d: %s not seated and out of epoch %d; the other %d incumbents seated", floor, victim, floorEpoch+1, len(seats))
		}
		return
	}
	if !seated[victim] {
		t.Errorf("below 0.9.0 bootstrap must seed %s as the 0.7.0 build does (registry %s)", victim, fp)
	} else {
		t.Logf("RESULT 0.%d: %s seated as before (no key check below 0.9.0): %s", floor, victim, fmt.Sprint(len(seats), " seats"))
	}
}
