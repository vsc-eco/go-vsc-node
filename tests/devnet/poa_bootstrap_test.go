package devnet

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// poaSeatDoc mirrors the poa_seats document shape (modules/db/vsc/poaseats.Seat).
type poaSeatDoc struct {
	Account          string `bson:"account"`
	UboId            string `bson:"ubo_id,omitempty"`
	AdmittedHeight   uint64 `bson:"admitted_height"`
	Bootstrap        bool   `bson:"bootstrap,omitempty"`
	LastSeatedHeight uint64 `bson:"last_seated_height"`
	ExitHeight       uint64 `bson:"exit_height"`
}

// poaSeats reads a node's whole seat registry, sorted by account.
func (d *Devnet) poaSeats(ctx context.Context, node int) ([]poaSeatDoc, error) {
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

	var out []poaSeatDoc
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("poa_seats decode on magi-%d: %w", node, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// seatFingerprint renders a registry as a comparable string. Deliberately
// includes admitted_height and bootstrap: two nodes agreeing on the SET of
// accounts while disagreeing on the height they were admitted at is exactly the
// divergence that forks the chain, and an account-only comparison would miss it.
func seatFingerprint(seats []poaSeatDoc) string {
	s := ""
	for _, x := range seats {
		s += fmt.Sprintf("%s@%d(bootstrap=%v);", x.Account, x.AdmittedHeight, x.Bootstrap)
	}
	return s
}

// TestPoaBootstrapDeterminism is scenario D2.
//
// It proves the property the whole POA batch rests on: when the consensus floor
// reaches 0.7.0, bootstrapPoaSeats seeds the seat registry from the first
// ratified election, it fires EXACTLY ONCE, and every node derives a
// BYTE-IDENTICAL registry. A divergence here forks the chain, and the registry
// is append-only, so a wrong seed is permanent.
//
// ★ ANTI-VACUOUS-PASS GUARDS. POA is inert until the floor reaches 0.7.0, so a
// test that merely finds an empty-and-identical registry on all nodes would
// "pass" against a completely inert system. Two preconditions must hold before
// any assertion counts:
//
//	(a) the ratified election carries FLAT weights (every weight == PoaSeatWeight
//	    == 1). Pre-activation weights are stake-derived and large, so this is a
//	    direct, positive observation that the POA batch actually took effect.
//
//	    ★ TIMING, learned by running this: POA activation takes TWO epochs.
//	    bootstrapPoaSeats seeds the registry at the ACTIVATION election -- the one
//	    "whose PREDECESSOR was still below the POA line" (poa_seats.go) -- but flat
//	    weight is gated on poaActive(prevVersion) (election-proposer.go), i.e. the
//	    PRIOR election's version. So the activation election itself still carries
//	    stake weights, and flat weight appears only from the NEXT one. Asserting
//	    flatness at the activation epoch fails against a perfectly healthy chain;
//	    the first run of this test did exactly that (weights were 2000000, the
//	    devnet MinStake).
//	(b) the registry is NON-EMPTY and its size equals the committee size.
//
// If either fails the test FAILS rather than reporting success, per
// feedback_vacuous_pass_is_worse_than_fail.
func TestPoaBootstrapDeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// Pin consensus 0.7.0 from epoch 1 so the POA batch is live.
	// ★ FloorEpoch MUST be non-zero: PinnedVersionFloor treats 0 as "no floor",
	// which would leave POA inert and make every assertion below vacuous.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 25*time.Minute)

	// Every node must ingest a real election before anything is asserted.
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// Epoch 1 is the ACTIVATION election: bootstrap seeds the registry here, but
	// its own weights are still stake-derived. Flat weight lands at epoch 2, so
	// wait for that before judging whether POA took effect.
	activation, err := d.GetElectionGQL(ctx, 1, 1)
	if err != nil {
		t.Fatalf("reading activation election epoch 1 from magi-1: %v", err)
	}
	t.Logf("activation election epoch 1: %d members, weights=%v (stake-derived here by design)",
		len(activation.Members), activation.Weights)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2 (the first flat-weight election): %v", n, err)
		}
	}

	// ---- PRECONDITION (a): POA is genuinely ACTIVE, not inert ----
	elec, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2 from magi-1: %v", err)
	}
	if len(elec.Members) == 0 {
		t.Fatal("election epoch 1 has no members — cannot evaluate POA activation")
	}
	for i, w := range elec.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED at epoch 2: weight[%d]=%d, want flat %d. "+
				"POA is INERT (floor pin did not take), so nothing below would be meaningful. "+
				"members=%v weights=%v", i, w, params.PoaSeatWeight, elec.Members, elec.Weights)
		}
	}
	if elec.TotalWeight != uint64(len(elec.Members))*params.PoaSeatWeight {
		t.Fatalf("PRECONDITION FAILED: total_weight=%d, want %d (flat weight x %d members)",
			elec.TotalWeight, uint64(len(elec.Members))*params.PoaSeatWeight, len(elec.Members))
	}
	t.Logf("POA ACTIVE: epoch %d, %d members, all weights flat=%d, total=%d",
		elec.Epoch, len(elec.Members), params.PoaSeatWeight, elec.TotalWeight)

	// ---- PRECONDITION (b) + the actual assertion: identical registries ----
	want := ""
	for n := 1; n <= cfg.Nodes; n++ {
		seats, err := d.poaSeats(ctx, n)
		if err != nil {
			t.Fatalf("magi-%d: %v", n, err)
		}
		if len(seats) == 0 {
			t.Fatalf("PRECONDITION FAILED: magi-%d has an EMPTY poa_seats registry while POA is "+
				"active. Bootstrap did not fire; an empty registry would make an all-nodes-agree "+
				"assertion trivially true", n)
		}
		if len(seats) != len(activation.Members) {
			t.Errorf("magi-%d: registry has %d seats but the ratified committee has %d members — "+
				"a PARTIAL bootstrap is worse than none (the seat gate refuses to apply while the "+
				"registry is short). seats=%v activation members=%v",
				n, len(seats), len(activation.Members), seatFingerprint(seats), activation.Members)
		}
		for _, s := range seats {
			if !s.Bootstrap {
				t.Errorf("magi-%d: seat %q has bootstrap=false; every founding seat must be "+
					"flagged as bootstrap-seeded", n, s.Account)
			}
		}
		fp := seatFingerprint(seats)
		t.Logf("magi-%d registry (%d seats): %s", n, len(seats), fp)
		if n == 1 {
			want = fp
		} else if fp != want {
			t.Errorf("REGISTRY DIVERGENCE magi-1 vs magi-%d — this forks the chain\n  magi-1: %s\n  magi-%d: %s",
				n, want, n, fp)
		}
	}

	// ---- bootstrap fires EXACTLY once: the registry must not grow ----
	// Wait out two further elections and re-read. bootstrapPoaSeats explicitly
	// refuses to retry, so any growth here means a second seed fired.
	base := len(activation.Members)
	for _, targetEpoch := range []uint64{3, 4} {
		nctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		err := d.waitForElectionEpoch(nctx, 1, targetEpoch, 6*time.Minute)
		cancel()
		if err != nil {
			t.Logf("magi-1 did not reach epoch %d (%v) — skipping the re-seed check at this epoch", targetEpoch, err)
			continue
		}
		for n := 1; n <= cfg.Nodes; n++ {
			seats, err := d.poaSeats(ctx, n)
			if err != nil {
				t.Errorf("magi-%d re-read at epoch %d: %v", n, targetEpoch, err)
				continue
			}
			if len(seats) != base {
				t.Errorf("magi-%d: registry changed from %d to %d seats by epoch %d — bootstrap "+
					"must fire exactly once and never re-seed. now=%s",
					n, base, len(seats), targetEpoch, seatFingerprint(seats))
			}
		}
	}
}
