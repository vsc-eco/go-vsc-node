package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// proposeConsensusVersion broadcasts vsc.propose_consensus_version from a witness.
func (d *Devnet) proposeConsensusVersion(witness int, major, consensus, activationEpoch uint64) (string, error) {
	acct := d.witnessAccount(witness)
	payload := map[string]interface{}{
		"net_id":           d.netId(),
		"major":            major,
		"consensus":        consensus,
		"non_consensus":    0,
		"activation_epoch": activationEpoch,
	}
	return d.BroadcastCustomJSON("vsc.propose_consensus_version", []string{acct}, payload, d.cfg.InitminerWIF)
}

// currentEpoch reads the highest election epoch a node has ingested.
func (d *Devnet) currentEpoch(ctx context.Context, node int) (uint64, error) {
	for e := uint64(60); e > 0; e-- {
		if _, err := d.GetElectionGQL(ctx, node, e); err == nil {
			return e, nil
		}
	}
	return 0, nil
}

// TestPoaActivationViaProposal is the Route B scenario, and it is the one that
// matters most operationally: it is the EXACT procedure we would run on testnet.
//
// The July runbook recommended Route A (edit the ConsensusVersionFloor* fields in
// system-config.go and redeploy). But the live testnet reached 0.5.0 with its
// config pin still reading epoch 765 / consensus 3, which means 0.4.0 and 0.5.0
// were rolled out with the ON-CHAIN op, not a config edit. Route B is therefore
// the established practice here, and unlike a config pin it carries the readiness
// guard (resolveVersionFloor, election-proposer.go:441-516): the floor only rises
// once >= 4/5 of committee stake announces the target AND the outgoing committee
// still holds quorum on it, which is what stops an activation from filtering the
// TSS share-holders below threshold and freezing the BTC vault.
//
// Nothing has ever tested that path. This test deliberately does NOT pin the
// floor: it leaves the devnet default, confirms POA is inert, then activates it
// the way a real operator would — by posting the op from a committee member — and
// asserts the floor actually rises and POA comes up.
func TestPoaActivationViaProposal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	// ★ NO floor override. POA must start INERT so the proposal is what activates it.
	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 12*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// ---- PRECONDITION (inverted): POA must be INERT before we activate it ----
	elec1, err := d.GetElectionGQL(ctx, 1, 1)
	if err != nil {
		t.Fatalf("reading election epoch 1: %v", err)
	}
	allFlat := true
	for _, w := range elec1.Weights {
		if w != params.PoaSeatWeight {
			allFlat = false
			break
		}
	}
	if allFlat {
		t.Fatalf("PRECONDITION FAILED: weights are already flat %v at epoch 1 — POA is active before "+
			"the proposal, so this test would prove nothing about the proposal path", elec1.Weights)
	}
	seats0, _ := d.poaSeats(ctx, 1)
	if len(seats0) != 0 {
		t.Fatalf("PRECONDITION FAILED: registry already has %d seats before activation", len(seats0))
	}
	t.Logf("POA correctly INERT: epoch 1 weights=%v, registry empty", elec1.Weights)

	// ---- activate the way an operator would ----
	cur, err := d.currentEpoch(ctx, 1)
	if err != nil || cur == 0 {
		t.Fatalf("cannot determine current epoch: %v", err)
	}
	target := cur + 2 // a couple of epochs of margin, as on testnet
	t.Logf("current epoch %d; proposing 0.7.0 for activation at epoch %d", cur, target)

	// Every committee member proposes, so stake-readiness is unambiguous. On
	// testnet ONE member posting is enough; the guard is about ANNOUNCED versions,
	// not about how many proposed.
	for n := 1; n <= cfg.Nodes; n++ {
		if _, err := d.proposeConsensusVersion(n, 0, 7, target); err != nil {
			t.Logf("propose from magi-%d failed (continuing): %v", n, err)
		}
	}

	// ---- the floor must rise and POA must come up ----
	// Flat weight lands one epoch after the floor is active (see the two-epoch
	// note in poa_bootstrap_test.go), so allow through target+2.
	deadline := time.Now().Add(20 * time.Minute)
	activated := false
	var seenEpoch uint64
	var seenWeights []uint64
	for time.Now().Before(deadline) && !activated {
		time.Sleep(20 * time.Second)
		for e := target; e <= target+3; e++ {
			el, err := d.GetElectionGQL(ctx, 1, e)
			if err != nil {
				continue
			}
			flat := len(el.Weights) > 0
			for _, w := range el.Weights {
				if w != params.PoaSeatWeight {
					flat = false
					break
				}
			}
			seenEpoch, seenWeights = e, el.Weights
			if flat {
				activated = true
				break
			}
		}
	}
	if !activated {
		t.Fatalf("POA did NOT activate via vsc.propose_consensus_version. Last seen epoch %d "+
			"weights=%v. This is the exact procedure intended for testnet, so a failure here is a "+
			"blocker for the testnet rollout, not a test problem.", seenEpoch, seenWeights)
	}
	t.Logf("ACTIVATED VIA PROPOSAL: epoch %d carries flat weights %v", seenEpoch, seenWeights)

	// ---- and the registry must be seeded, identically, everywhere ----
	want := ""
	for n := 1; n <= cfg.Nodes; n++ {
		seats, err := d.poaSeats(ctx, n)
		if err != nil {
			t.Errorf("magi-%d poa_seats: %v", n, err)
			continue
		}
		if len(seats) == 0 {
			t.Errorf("magi-%d: registry EMPTY after activation via proposal", n)
			continue
		}
		fp := seatFingerprint(seats)
		t.Logf("magi-%d registry (%d seats): %s", n, len(seats), fp)
		if n == 1 {
			want = fp
		} else if fp != want {
			t.Errorf("REGISTRY DIVERGENCE after proposal-driven activation, magi-1 vs magi-%d\n  %s\n  %s",
				n, want, fp)
		}
	}
}
