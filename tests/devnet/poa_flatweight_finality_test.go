package devnet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// maxSlotHeight returns the highest slot_height in a node's block_headers.
//
// ★ THIS IS THE CORRECT INSTRUMENT, and the obvious alternative is wrong.
// getLastProcessedBlock reads hive_blocks metadata, i.e. HIVE L1 ingestion, which
// keeps advancing whether or not VSC can reach quorum — a halt test built on it
// would never observe the halt. A block_headers row only exists for a VSC block
// that actually gathered its BLS quorum, so this is the thing flat weight moves.
func (d *Devnet) maxSlotHeight(ctx context.Context, node int) (int, error) {
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

// grewWithin reports whether a node's VSC block height advanced during the window.
func (d *Devnet) grewWithin(ctx context.Context, t *testing.T, node int, window time.Duration) (bool, int, int) {
	start, err := d.maxSlotHeight(ctx, node)
	if err != nil {
		t.Fatalf("baseline slot height on magi-%d: %v", node, err)
	}
	deadline := time.Now().Add(window)
	last := start
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		h, err := d.maxSlotHeight(ctx, node)
		if err != nil {
			continue
		}
		last = h
		if h > start {
			return true, start, h
		}
	}
	return false, start, last
}

// TestPoaFlatWeightFinalityThreshold is scenario D10.
//
// POA replaces stake-proportional election weights with a flat weight of 1 per
// seat (params.PoaSeatWeight, written in election-proposer.go). BLS finality
// requires signedWeight >= weightTotal - weightTotal/3 (bls_quorum.go). For 5
// flat seats that threshold is 4, so activation silently converts a network that
// finalised with 3-of-5 (by stake) into one that needs 4-of-5 (by count).
//
// This is an operational property, not a code defect, and it is the single most
// likely way a POA activation surprises an operator: their liveness margin
// shrinks at the activation epoch with no other visible change. Nothing in the
// existing D1-D9 plan covers block finality under flat weight — D5 covers
// gateway multisig KEY weights, a different consumer of the same numbers.
//
// ★ WHAT THIS TEST DOES AND DOES NOT PROVE. Read this before quoting a result.
//
// cmd/devnet-setup stakes every witness the SAME args.stakeAmt, so on a devnet
// all weights are already equal (2,000,000 == CONSENSUS_MINIMUM) BEFORE POA. The
// pre-POA quorum is therefore 10,000,000 - 10,000,000/3 = 6,666,667, which 3
// seats (6,000,000) already FAIL and 4 seats (8,000,000) meet. In other words
// this devnet needs 4-of-5 both before AND after activation, so this test cannot
// observe the threshold CHANGING and must not be cited as evidence that it does.
//
// What it does prove, which is still worth proving: flat weight is genuinely in
// force and its quorum is ENFORCED end-to-end — the chain really does stop when
// the flat threshold is unreachable and really does recover when it is restored.
//
// The CHANGE is a property of unequal stake and is demonstrated arithmetically
// against the live testnet committee (50M/20M/5M/20M/20.000002M): total
// 115,000,002, quorum 76,666,668, top three = 90,000,002, so 3-of-5 finalises
// today and 4-of-5 is required after activation. Proving that on a devnet would
// need per-witness stake amounts, which devnet-setup does not currently support.
//
// The test asserts the threshold in both directions: the chain advances with all
// 5 up, STOPS when too many are stopped, and RESUMES when one returns.
func TestPoaFlatWeightFinalityThreshold(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// ★ FloorEpoch MUST be non-zero (PinnedVersionFloor treats 0 as "no floor").
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 35*time.Minute)

	// ★ Wait for epoch 2, not 1. POA activation takes TWO epochs: the registry
	// seeds at the activation election, but flat weight is gated on
	// poaActive(prevVersion), so the activation election itself still carries
	// stake weights. This test originally polled epoch 1 and failed its own
	// precondition (weight=2000000) against a perfectly healthy chain.
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 12*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2 (first flat-weight election): %v", n, err)
		}
	}

	// ---- PRECONDITION: POA active and weights genuinely flat ----
	elec, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	for i, w := range elec.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d — POA is INERT, so a halt "+
				"observed below would prove nothing about flat weight", i, w, params.PoaSeatWeight)
		}
	}
	members := len(elec.Members)
	if members < 3 {
		t.Fatalf("PRECONDITION FAILED: committee of %d is too small to test a quorum threshold", members)
	}
	// ceil(2N/3) expressed exactly as bls_quorum.go computes it.
	total := uint64(members) * params.PoaSeatWeight
	quorum := total - total/3
	tolerated := uint64(members) - quorum // how many seats may be offline
	t.Logf("POA ACTIVE: %d flat seats, total weight %d, quorum %d => at most %d node(s) may be offline",
		members, total, quorum, tolerated)

	// Record the pre-POA threshold for this devnet so nobody reads the halt below
	// as evidence that activation CHANGED the margin. On a devnet it did not.
	preTotal := uint64(members) * uint64(params.CONSENSUS_MINIMUM)
	preQuorum := preTotal - preTotal/3
	preTolerated := 0
	for off := 0; off < members; off++ {
		if uint64(members-off)*uint64(params.CONSENSUS_MINIMUM) >= preQuorum {
			preTolerated = off
		}
	}
	t.Logf("NOTE: devnet stakes every witness equally (%d), so the PRE-POA quorum was %d of %d "+
		"and tolerated %d offline node(s). Same margin as flat weight here, so this run proves "+
		"ENFORCEMENT of the flat quorum, NOT that activation changed the margin.",
		params.CONSENSUS_MINIMUM, preQuorum, preTotal, preTolerated)
	if tolerated != 1 {
		t.Logf("NOTE: this devnet tolerates %d offline nodes, not 1; adjusting the stop count", tolerated)
	}

	// ---- baseline: the chain IS advancing with everyone up ----
	grew, from, to := d.grewWithin(ctx, t, 1, 3*time.Minute)
	if !grew {
		t.Fatalf("PRECONDITION FAILED: VSC block height did not advance (%d -> %d) with all %d nodes "+
			"up; the halt assertion below would be meaningless", from, to, cfg.Nodes)
	}
	t.Logf("baseline: VSC slot height advanced %d -> %d with all %d nodes up", from, to, cfg.Nodes)

	// ---- stop one MORE node than the quorum tolerates ----
	stopCount := int(tolerated) + 1
	stopped := []int{}
	for i := 0; i < stopCount; i++ {
		node := cfg.Nodes - i // stop from the top: magi-5, magi-4, ...
		if err := d.StopNode(ctx, node); err != nil {
			t.Fatalf("stopping magi-%d: %v", node, err)
		}
		stopped = append(stopped, node)
		t.Logf("stopped magi-%d", node)
	}
	defer func() {
		for _, n := range stopped {
			_ = d.StartNode(context.Background(), n)
		}
	}()

	// ---- assert the chain HALTS ----
	// Give the network a slot or two to settle, then require NO growth.
	time.Sleep(30 * time.Second)
	grew, from, to = d.grewWithin(ctx, t, 1, 3*time.Minute)
	if grew {
		t.Errorf("EXPECTED HALT BUT CHAIN ADVANCED %d -> %d with %d of %d nodes stopped. "+
			"Either flat weight is not reaching bls_quorum, or the quorum arithmetic here is wrong "+
			"(total=%d quorum=%d).", from, to, stopCount, cfg.Nodes, total, quorum)
	} else {
		t.Logf("CONFIRMED: VSC block height frozen at %d with %d of %d nodes stopped "+
			"(quorum %d of %d unreachable)", to, stopCount, cfg.Nodes, quorum, total)
	}

	// ---- assert it RESUMES when quorum is restored ----
	revive := stopped[len(stopped)-1]
	if err := d.StartNode(ctx, revive); err != nil {
		t.Fatalf("restarting magi-%d: %v", revive, err)
	}
	t.Logf("restarted magi-%d; expecting finality to resume", revive)
	grew, from, to = d.grewWithin(ctx, t, 1, 5*time.Minute)
	if !grew {
		t.Errorf("chain did NOT resume after restoring quorum (height stuck %d -> %d). A halt that "+
			"does not recover when the node returns is a much worse finding than the halt itself.",
			from, to)
	} else {
		t.Logf("CONFIRMED: finality resumed %d -> %d after magi-%d returned", from, to, revive)
	}
}
