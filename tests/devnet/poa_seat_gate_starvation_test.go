package devnet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// trimRegistryEverywhere deletes every seat EXCEPT the named accounts, on EVERY
// node's poa_seats collection.
//
// Written to all nodes because the registry is consensus input to the seat gate:
// trimming one node only would test registry DIVERGENCE (that is D16/D17), not
// the starvation guard. Every node must reach the identical verdict.
func trimRegistryEverywhere(t *testing.T, d *Devnet, ctx context.Context, keep []string, nodes int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	total := 0
	for n := 1; n <= nodes; n++ {
		res, err := client.Database(d.nodeDbName(n)).Collection("poa_seats").
			DeleteMany(ctx, bson.M{"account": bson.M{"$nin": keep}})
		if err != nil {
			t.Fatalf("trimming poa_seats on magi-%d: %v", n, err)
		}
		total += int(res.DeletedCount)
	}
	return total
}

// bareAccounts strips the "hive:" prefix election members carry, so they can be
// compared against seat-registry accounts, which are stored bare.
func bareAccounts(members []string) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, strings.TrimPrefix(m, "hive:"))
	}
	sort.Strings(out)
	return out
}

func allFlat(weights []uint64) bool {
	for _, w := range weights {
		if w != params.PoaSeatWeight {
			return false
		}
	}
	return len(weights) > 0
}

// waitElections blocks until node 1 has ingested `n` further election epochs,
// and returns the election it landed on.
func waitElections(t *testing.T, d *Devnet, ctx context.Context, from uint64, n uint64, timeout time.Duration) *ElectionInfo {
	t.Helper()
	target := from + n
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := d.waitForElectionEpoch(wctx, 1, target, timeout); err != nil {
		t.Fatalf("magi-1 never reached election epoch %d: %v", target, err)
	}
	elec, err := d.GetElectionGQL(ctx, 1, target)
	if err != nil {
		t.Fatalf("reading election epoch %d: %v", target, err)
	}
	return elec
}

// TestPoaSeatGateStarvation is scenario D4: the seat gate on the REAL election
// path must not starve the committee.
//
// Two phases, and the second is the one that matters most.
//
// PHASE A — the gate APPLIES. With more stake-eligible witnesses than seated
// accounts, only seated accounts may be elected. This is the restriction POA
// exists to impose.
//
// PHASE B — the gate REFUSES. When applying the filter would drop the committee
// below MinMembers, the gate declines entirely and the election proceeds
// UNGATED (election-proposer.go, "STARVATION GUARD"). The chain must keep
// producing rather than stalling, which is the failure that halted mainnet at
// epoch 1699 in the structurally identical H-6 gate.
//
// ★ PHASE B IS ALSO THE LIVE PROOF OF FIX C3. On the starvation-refusal path the
// seat filter is NOT applied, so the POA benefit must not be applied either.
// Before C3, `poaActive` re-derived its verdict from `poaSeats != nil` and so
// still said yes here, producing the strictly-worst regime: candidacy back to
// "anyone with MinStake" while every candidate carries a full seat's weight, so
// committee weight is purchasable at MinStake per seat. After C3 the POA rules
// bind to what the gate actually DID, so an ungated election must carry
// STAKE-derived weights. Asserting "weights are not flat" here is therefore a
// direct, on-chain observation of the fix.
func TestPoaSeatGateStarvation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	floor := poaDevnetFloor()
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = floor
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 40*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}

	// ---- PRECONDITION: POA is genuinely ACTIVE, not inert ----
	base, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	if !allFlat(base.Weights) {
		t.Fatalf("PRECONDITION FAILED: epoch 2 weights=%v are not flat=%d. POA is INERT, so the "+
			"seat gate is not running at all and nothing below would be meaningful",
			base.Weights, params.PoaSeatWeight)
	}
	members := bareAccounts(base.Members)
	if len(members) < 4 {
		t.Fatalf("PRECONDITION FAILED: need >=4 committee members to trim below MinMembers "+
			"meaningfully, got %d (%v)", len(members), members)
	}
	seats, err := d.pfPoaSeats(ctx, 1)
	if err != nil {
		t.Fatalf("poa_seats: %v", err)
	}
	if len(seats) != len(members) {
		t.Fatalf("PRECONDITION FAILED: registry has %d seats but committee has %d members; "+
			"the starting state is not the healthy fully-seeded one this scenario assumes",
			len(seats), len(members))
	}
	minMembers := 3 // devnet ConsensusParams().MinMembers
	t.Logf("PRECONDITION OK: POA active at epoch %d, %d members, %d seats, flat weight %d",
		base.Epoch, len(members), len(seats), params.PoaSeatWeight)

	// ================= PHASE A: the gate APPLIES =================
	//
	// Keep exactly MinMembers seats. All 5 witnesses remain stake-eligible and
	// enabled, so the candidate list is still 5 while the registry names 3.
	// len(gated)=3 >= floor=3, so the gate applies and the 2 unseated candidates
	// must be excluded.
	keep := members[:minMembers]
	dropped := trimRegistryEverywhere(t, d, ctx, keep, cfg.Nodes)
	t.Logf("PHASE A: trimmed registry to %d seats (%v), deleted %d rows across %d nodes",
		len(keep), keep, dropped, cfg.Nodes)

	for n := 1; n <= cfg.Nodes; n++ {
		s, err := d.pfPoaSeats(ctx, n)
		if err != nil {
			t.Fatalf("magi-%d poa_seats: %v", n, err)
		}
		if len(s) != len(keep) {
			t.Fatalf("magi-%d has %d seats after the trim, want %d — the nodes no longer agree "+
				"on the registry, which would make this a divergence test rather than a "+
				"starvation test", n, len(s), len(keep))
		}
	}

	elecA := waitElections(t, d, ctx, base.Epoch, 2, 12*time.Minute)
	gotA := bareAccounts(elecA.Members)
	t.Logf("PHASE A: epoch %d members=%v weights=%v", elecA.Epoch, gotA, elecA.Weights)

	if fmt.Sprint(gotA) != fmt.Sprint(keep) {
		t.Errorf("SEAT GATE DID NOT APPLY: epoch %d elected %v, want exactly the seated set %v. "+
			"Unseated but stake-eligible witnesses were admitted, which is the permissionless "+
			"entry POA exists to close", elecA.Epoch, gotA, keep)
	}
	if !allFlat(elecA.Weights) {
		t.Errorf("epoch %d: gate applied but weights=%v are not flat=%d — the restriction and the "+
			"benefit have come apart, which is exactly the C3 defect in the other direction",
			elecA.Epoch, elecA.Weights, params.PoaSeatWeight)
	}

	// ================= PHASE B: the gate REFUSES =================
	//
	// Trim to a single seat. Applying the filter would leave 1 < MinMembers=3, so
	// the starvation guard must decline to apply it at all.
	keep1 := members[:1]
	dropped = trimRegistryEverywhere(t, d, ctx, keep1, cfg.Nodes)
	t.Logf("PHASE B: trimmed registry to 1 seat (%v), deleted %d further rows", keep1, dropped)

	beforeHeights := make([]int, cfg.Nodes+1)
	for n := 1; n <= cfg.Nodes; n++ {
		h, err := d.pfMaxSlotHeight(ctx, n)
		if err != nil {
			t.Fatalf("magi-%d block_headers read before the refusal: %v", n, err)
		}
		beforeHeights[n] = h
	}

	elecB := waitElections(t, d, ctx, elecA.Epoch, 2, 12*time.Minute)
	gotB := bareAccounts(elecB.Members)
	t.Logf("PHASE B: epoch %d members=%v weights=%v total=%d",
		elecB.Epoch, gotB, elecB.Weights, elecB.TotalWeight)

	// 1. The chain must still be producing. A stalled chain is the failure mode.
	stalled := 0
	for n := 1; n <= cfg.Nodes; n++ {
		after, err := d.pfMaxSlotHeight(ctx, n)
		if err != nil {
			t.Errorf("magi-%d block_headers read after the refusal: %v", n, err)
			continue
		}
		if after <= beforeHeights[n] {
			stalled++
			t.Errorf("magi-%d: block_headers did not advance across the starvation refusal "+
				"(%d -> %d). The seat gate became the thing that stops block production, which "+
				"is precisely what the starvation guard exists to prevent",
				n, beforeHeights[n], after)
		}
	}
	if stalled == 0 {
		t.Logf("chain advanced on all %d nodes across the refusal", cfg.Nodes)
	}

	// 2. At 0.9.0 (POA-2): the seat stays and the committee is topped up to
	// exactly MinMembers with unseated candidates, all at flat seat weight.
	if floor >= 9 {
		hasSeat := false
		for _, a := range gotB {
			if a == keep1[0] {
				hasSeat = true
			}
		}
		if len(gotB) != minMembers || !hasSeat {
			t.Errorf("POA-2: epoch %d elected %v, want the seat %v plus a top-up to exactly MinMembers=%d",
				elecB.Epoch, gotB, keep1, minMembers)
		} else {
			t.Logf("POA-2 FIXED on a live network: epoch %d elected %v (seat %v + %d top-up), not the whole candidate list",
				elecB.Epoch, gotB, keep1, minMembers-1)
		}
		if !allFlat(elecB.Weights) {
			t.Errorf("POA-2: top-up election epoch %d weights=%v, want flat %d", elecB.Epoch, elecB.Weights, params.PoaSeatWeight)
		}
		return
	}

	// 2. The election proceeded UNGATED: unseated candidates are back.
	if len(gotB) <= len(keep1) {
		t.Errorf("epoch %d elected only %v. The gate appears to have been APPLIED against a "+
			"1-seat registry, starving the committee, instead of refusing", elecB.Epoch, gotB)
	}

	// 3. ★ C3: ungated election must NOT carry flat weight.
	if allFlat(elecB.Weights) {
		t.Errorf("★ C3 REGRESSION: epoch %d proceeded UNGATED (elected %d of %d seated) yet "+
			"carries FLAT weights %v. The seat filter was not applied but the POA benefit was, "+
			"so committee weight is purchasable at MinStake per seat — cheaper than the stake "+
			"weighting POA replaced and free of the vetting POA adds",
			elecB.Epoch, len(gotB), len(keep1), elecB.Weights)
	} else {
		t.Logf("★ C3 CONFIRMED on a live network: ungated election epoch %d carries "+
			"stake-derived weights %v, not flat %d",
			elecB.Epoch, elecB.Weights, params.PoaSeatWeight)
	}
}
