package devnet

import (
	"context"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// pendingVersionProposals reads a node's pending consensus-version proposals.
// Used as the POSITIVE CONTROL for the negative test below: it distinguishes
// "the guard refused the rise" from "the proposal op never landed at all".
func (d *Devnet) pendingVersionProposals(ctx context.Context, node int) ([]struct {
	Target          string `json:"target"`
	ActivationEpoch uint64 `json:"activation_epoch"`
	Proposer        string `json:"proposer"`
}, error) {
	const q = `{localNodeInfo{consensus_version_proposals{target activation_epoch proposer}}}`
	var out struct {
		LocalNodeInfo struct {
			Proposals []struct {
				Target          string `json:"target"`
				ActivationEpoch uint64 `json:"activation_epoch"`
				Proposer        string `json:"proposer"`
			} `json:"consensus_version_proposals"`
		} `json:"localNodeInfo"`
	}
	if err := d.gqlQuery(ctx, node, q, nil, &out); err != nil {
		return nil, err
	}
	return out.LocalNodeInfo.Proposals, nil
}

// TestPoaReadinessGuardRefusesPrematureRise is scenario D12-NEGATIVE, and it is
// the most important scenario that was still uncovered.
//
// TestPoaActivationViaProposal proved the floor RISES when the network is ready.
// Nothing proved it REFUSES when the network is not — and that refusal is the
// vault-freeze protection. resolveVersionFloor (election-proposer.go:441-516)
// requires BOTH:
//
//	stakeReady   — >= 4/5 of committee stake announces a version meeting the target
//	prevReadyAt  — the OUTGOING committee still retains quorum on the target
//
// The second exists specifically because advancing past the outgoing committee
// filters its members out, and that committee holds the live TSS key shares: rise
// too early and BTC keysign/reshare loses threshold and the vault FREEZES. A
// config pin (Route A) has no such guard, which is exactly why Route B is the
// safer activation path and why this refusal must be shown to work.
//
// ★★ THIS IS A NEGATIVE TEST, the most dangerous kind to write: "nothing
// happened" is also what a broken devnet, a dropped transaction, or a typo
// produces. Three defences make the absence meaningful:
//
//  1. PREMISE  — assert readiness is genuinely BELOW threshold (2 of 5 witnesses
//     really are announcing a below-target version, verified from their records).
//  2. CONTROL  — assert the proposal was ACCEPTED and is pending on-chain. If the
//     op never landed, a non-rise proves nothing about the guard.
//  3. OUTCOME  — assert POA is still inert after several epochs.
//
// Without (2) especially, this test would pass against a devnet where the
// proposal simply failed to broadcast.
//
// Set POA_OLD_SOURCE to a checkout announcing below 0.7.0 (origin/main = 0.3.0).
func TestPoaReadinessGuardRefusesPrematureRise(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	oldSrc := os.Getenv("POA_OLD_SOURCE")
	if oldSrc == "" {
		t.Skip("POA_OLD_SOURCE not set (path to a checkout announcing < 0.7.0)")
	}

	cfg := tssTestConfig()
	// NO floor override: POA starts inert and only a proposal could raise it.
	// Two of five nodes run the old binary, so at most 3/5 = 60% of stake can
	// announce 0.7.0 — below the 4/5 stakeReady threshold.
	laggards := []int{cfg.Nodes - 1, cfg.Nodes} // magi-4, magi-5
	cfg.OldCodeSourceDir = oldSrc
	cfg.OldCodeNodes = laggards

	d, ctx := startDevnetNoKey(t, cfg, 50*time.Minute)

	upgraded := cfg.Nodes - len(laggards)
	for n := 1; n <= upgraded; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 12*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// ---- DEFENCE 1: readiness really is below threshold ----
	belowCount := 0
	for _, n := range laggards {
		acct := d.witnessAccount(n)
		major, proto, found := d.announcedVersion(ctx, 1, acct)
		if !found {
			t.Fatalf("PREMISE FAILED: no witness record for laggard %s — cannot establish that "+
				"readiness is below threshold", acct)
		}
		if major != 0 || proto >= 7 {
			t.Fatalf("PREMISE FAILED: laggard %s announces %d.%d, which MEETS the 0.7.0 target. "+
				"Readiness would then be 5/5 and a non-rise would be inexplicable rather than "+
				"attributable to the guard.", acct, major, proto)
		}
		belowCount++
		t.Logf("laggard %s announces %d.%d (below target)", acct, major, proto)
	}
	elec1, err := d.GetElectionGQL(ctx, 1, 1)
	if err != nil {
		t.Fatalf("reading election epoch 1: %v", err)
	}
	readyFrac := float64(len(elec1.Members)-belowCount) / float64(len(elec1.Members))
	t.Logf("PREMISE OK: %d of %d committee members announce >= 0.7.0 (%.0f%%), below the 4/5 (80%%) "+
		"stakeReady threshold", len(elec1.Members)-belowCount, len(elec1.Members), readyFrac*100)
	if readyFrac >= 0.8 {
		t.Fatalf("PREMISE FAILED: readiness is %.0f%%, at or above the 80%% threshold — the guard "+
			"SHOULD allow the rise, so refusing to rise would be the bug, not the expected result",
			readyFrac*100)
	}

	// POA must be inert to begin with.
	for _, w := range elec1.Weights {
		if w == params.PoaSeatWeight && len(elec1.Weights) > 1 {
			t.Fatalf("PRECONDITION FAILED: weights already flat %v before any proposal", elec1.Weights)
		}
	}

	// ---- propose 0.7.0 from the UPGRADED members (a node may only propose a
	// version it announces, so the laggards cannot propose it at all) ----
	cur, _ := d.currentEpoch(ctx, 1)
	target := cur + 2
	t.Logf("current epoch %d; proposing 0.7.0 for activation at epoch %d from %d upgraded members",
		cur, target, upgraded)
	for n := 1; n <= upgraded; n++ {
		if _, err := d.proposeConsensusVersion(n, 0, 7, target); err != nil {
			t.Logf("propose from magi-%d failed (continuing): %v", n, err)
		}
	}

	// ---- DEFENCE 2 (THE CONTROL): the proposal must actually be pending ----
	var pending int
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Second)
		props, err := d.pendingVersionProposals(ctx, 1)
		if err != nil {
			continue
		}
		if len(props) > 0 {
			pending = len(props)
			for _, p := range props {
				t.Logf("pending proposal recorded on-chain: target=%s activation_epoch=%d proposer=%s",
					p.Target, p.ActivationEpoch, p.Proposer)
			}
			break
		}
	}
	if pending == 0 {
		t.Fatalf("CONTROL FAILED: no consensus-version proposal is pending after broadcasting from %d "+
			"members. The absence of an activation below would then be explained by the op never "+
			"landing, NOT by the readiness guard. Treat this run as INCONCLUSIVE.", upgraded)
	}

	// ---- DEFENCE 3: and yet POA must NOT activate ----
	for e := target; e <= target+3; e++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		_ = d.waitForElectionEpoch(nctx, 1, e, 8*time.Minute)
		cancel()
		el, err := d.GetElectionGQL(ctx, 1, e)
		if err != nil || el == nil {
			continue
		}
		flat := len(el.Weights) > 0
		for _, w := range el.Weights {
			if w != params.PoaSeatWeight {
				flat = false
				break
			}
		}
		t.Logf("epoch %d weights=%v flat=%v members=%d", e, el.Weights, flat, len(el.Members))
		if flat {
			t.Fatalf("READINESS GUARD FAILED: POA ACTIVATED at epoch %d with only %.0f%% of stake "+
				"announcing the target. The guard exists to stop exactly this: advancing the floor "+
				"past the outgoing committee filters out the members holding the live TSS shares, "+
				"which freezes the BTC vault. weights=%v", e, readyFrac*100, el.Weights)
		}
	}

	seats, _ := d.poaSeats(ctx, 1)
	if len(seats) != 0 {
		t.Errorf("READINESS GUARD FAILED: the seat registry was seeded (%d rows) despite the floor "+
			"never rising: %s", len(seats), seatFingerprint(seats))
	}

	t.Logf("CONFIRMED: with %.0f%% readiness the proposal was ACCEPTED (%d pending) but the floor did "+
		"NOT rise and POA stayed inert. The vault-freeze protection works — this is the guard a "+
		"config-pin activation (Route A) does not have.", readyFrac*100, pending)
}
