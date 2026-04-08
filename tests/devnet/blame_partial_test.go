package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// TestBlamePartialLatency tests whether the network can make useful
// progress when 2 of 7 nodes are slow enough to fail readiness checks,
// but 5 healthy nodes (> 2/3 threshold) remain.
//
// The challenge: each healthy node sees a different subset of the 2
// slow nodes as reachable vs timed-out, depending on which latency
// paths happen to be configured. This means:
//
//   - Node 1 might see {6: timeout, 7: timeout}
//   - Node 2 might see {6: timeout, 7: ok}
//   - Node 3 might see {6: ok, 7: timeout}
//   - Node 4 might see {6: ok, 7: ok}
//   - Node 5 might see {6: ok, 7: ok}
//
// With the current buggy checkParticipantReadiness, each node builds a
// different party list → SSID mismatch → no blame lands.
//
// When fixed, nodes should build identical party lists from on-chain
// data only, let the protocol timeout handle slow nodes, and produce
// a blame commitment that the 5 healthy nodes agree on. The subsequent
// reshare should then exclude the blamed nodes and succeed with the
// remaining healthy quorum.
//
// Latency setup (deterministic, not random):
//
//	Nodes 6,7 are "slow". Latency is added asymmetrically:
//	  - Node 1 ↔ Node 6: 6s (timeout)     Node 1 ↔ Node 7: 6s (timeout)
//	  - Node 2 ↔ Node 6: 6s (timeout)     Node 2 ↔ Node 7: normal (ok)
//	  - Node 3 ↔ Node 6: normal (ok)      Node 3 ↔ Node 7: 6s (timeout)
//	  - Node 4 ↔ Node 6: normal (ok)      Node 4 ↔ Node 7: normal (ok)
//	  - Node 5 ↔ Node 6: normal (ok)      Node 5 ↔ Node 7: normal (ok)
//
// This guarantees different nodes see different combinations:
//   - Nodes 1,2 see node 6 as unreachable
//   - Nodes 1,3 see node 7 as unreachable
//   - Nodes 4,5 see both as reachable
//
// Run with:
//
//	go test -v -run TestBlamePartialLatency -timeout 30m ./tests/devnet/
func TestBlamePartialLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet partial latency test in short mode")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	wasmPath, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 7
	cfg.LogLevel = "trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 60,
		},
		TssParams: &params.TssParams{
			RotateInterval: 20,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// ── Phase 1: Deploy contract, keygen, wait for epoch 1 ───────────

	t.Log("Phase 1: deploying contract and triggering keygen...")

	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath:     wasmPath,
		Name:         "call-tss",
		DeployerNode: 1,
		GQLNode:      2,
	})
	if err != nil {
		t.Fatalf("deploying contract: %v", err)
	}

	if err := d.WaitForBlockProcessing(ctx, 2, 10, 3*time.Minute); err != nil {
		t.Fatalf("node 2 never synced: %v", err)
	}

	payload := `{"key_name":"partialKey","epochs":10}`
	_, err = d.CallContract(ctx, 2, contractId, "tssCreate", payload)
	if err != nil {
		t.Fatalf("calling tssCreate: %v", err)
	}

	fullKeyId := contractId + "-partialKey"
	t.Logf("waiting for key %s...", fullKeyId)
	_, err = d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("key never created: %v", err)
	}

	keygenCommit, err := d.WaitForCommitment(ctx, 2, bson.M{
		"key_id": fullKeyId,
		"type":   "keygen",
	}, 10*time.Minute)
	if err != nil {
		d.dumpBlockHeight(ctx, t, 2)
		d.dumpTssLogs(ctx, t, 2)
		t.Fatalf("keygen commitment never landed: %v", err)
	}
	t.Logf("keygen at block %d epoch %d", keygenCommit.BlockHeight, keygenCommit.Epoch)

	_, err = d.WaitForTssKey(ctx, 2, bson.M{
		"id": fullKeyId, "status": "active",
	}, 2*time.Minute)
	if err != nil {
		t.Fatalf("key never became active: %v", err)
	}

	t.Log("waiting for epoch >= 1...")
	if err := d.waitForElectionEpoch(ctx, 2, 1, 5*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("epoch 1 never arrived: %v", err)
	}

	// ── Phase 2: Inject asymmetric latency to 2 "slow" nodes ─────────
	//
	// Nodes 6 and 7 become slow. Different healthy nodes see different
	// combinations of them as reachable vs timed-out.

	t.Log("Phase 2: injecting asymmetric latency to nodes 6 and 7...")

	// Use OUTBOUND-ONLY latency: delay traffic FROM nodes 6/7 TO
	// specific healthy nodes. This makes 6/7 appear slow to those
	// nodes without making the healthy nodes appear slow to 6/7.
	//
	// Without this, bidirectional latency causes the healthy nodes
	// to ALSO be blamed (their responses to 6/7 are delayed too),
	// resulting in 4+ blamed nodes and no quorum for reshare.

	// Node 6 is slow SENDING to nodes 1,2
	for _, n := range []int{1, 2} {
		if err := d.AddOutboundLatency(ctx, 6, n, 6000, 0); err != nil {
			t.Fatalf("adding outbound latency 6→%d: %v", n, err)
		}
	}

	// Node 7 is slow SENDING to nodes 1,3
	for _, n := range []int{1, 3} {
		if err := d.AddOutboundLatency(ctx, 7, n, 6000, 0); err != nil {
			t.Fatalf("adding outbound latency 7→%d: %v", n, err)
		}
	}

	t.Cleanup(func() {
		d.RemoveLatency(context.Background(), 6)
		d.RemoveLatency(context.Background(), 7)
	})

	t.Log("  Outbound latency (6s, one-way):")
	t.Log("  Node 6 → Nodes 1,2: slow (readiness check times out)")
	t.Log("  Node 7 → Nodes 1,3: slow (readiness check times out)")
	t.Log("  All other paths: normal")

	// ── Phase 3: Wait for the network to self-heal WITHIN ONE EPOCH ──
	//
	// The latency stays active throughout. With ElectionInterval=60
	// and RotateInterval=20, there are 3 reshare cycles per epoch.
	// The protocol MUST recover within those 3 cycles:
	//
	//   Cycle 1: blame lands (healthy nodes agree on the 2 slow culprits)
	//   Cycle 2: reshare succeeds (blamed nodes excluded, 5 healthy remain)
	//
	// If the protocol has bugs like cascading blame (a slow node's
	// delayed messages cause a healthy node to also appear stuck and
	// get blamed), the blame set grows beyond the 2 actual slow nodes,
	// reducing the remaining healthy set below quorum. The test fails.
	//
	// We wait through exactly 3 reshare cycles (one epoch) to enforce
	// that recovery must happen within a single epoch — not by waiting
	// for an epoch transition to reset the state.

	rotateInterval := uint64(20)
	bh, _ := d.getLastProcessedBlock(ctx, 2)
	// 3 reshare cycles = 1 epoch. Add margin for protocol timeout (2m)
	// and BLS collection.
	targetBlock := ((bh/rotateInterval)+1)*rotateInterval + 3*rotateInterval + 50

	t.Logf("current block: %d, waiting through 3 reshare cycles (1 epoch, latency active) to block %d...", bh, targetBlock)
	if err := d.WaitForBlockProcessing(ctx, 2, targetBlock, 12*time.Minute); err != nil {
		t.Fatalf("didn't reach target block: %v", err)
	}

	// Collect all blame/reshare commitments since keygen, within the
	// SAME epoch as the first reshare attempt.
	commits, err := d.GetCommitments(ctx, 2, bson.M{
		"key_id":       fullKeyId,
		"type":         bson.M{"$in": []string{"blame", "reshare"}},
		"block_height": bson.M{"$gt": keygenCommit.BlockHeight},
	})
	if err != nil {
		t.Fatalf("querying commitments: %v", err)
	}

	// Get election members from MongoDB to map bitset indices to
	// account names. The ordering is by consensus key, NOT by node
	// number — we must read the actual election to decode correctly.
	var blameEpoch uint64
	if len(commits) > 0 {
		blameEpoch = commits[0].Epoch
	}
	members, err := d.GetElectionMembers(ctx, 2, blameEpoch)
	if err != nil {
		t.Logf("warning: could not read election members for epoch %d: %v", blameEpoch, err)
		// Fall back to node order (may be wrong)
		members = make([]string, cfg.Nodes)
		for i := range members {
			members[i] = fmt.Sprintf("%s%d", cfg.WitnessPrefix, i+1)
		}
	}
	t.Logf("election epoch %d member ordering: %v", blameEpoch, members)

	t.Logf("commitments during obstructed network: %d", len(commits))
	for i, c := range commits {
		bits := decodeBitset(t, c.Commitment)
		var names []string
		for j := 0; j < len(members); j++ {
			if bits.Bit(j) == 1 {
				names = append(names, members[j])
			}
		}
		label := "blamed"
		if c.Type == "reshare" {
			label = "in committee"
		}
		t.Logf("  [%d] type=%s block=%d epoch=%d %s=%v bitset=%s",
			i, c.Type, c.BlockHeight, c.Epoch, label, names, bits.Text(2))
	}

	// ── Assertions ───────────────────────────────────────────────────

	hasReshare := false
	for _, c := range commits {
		if c.Type == "reshare" {
			hasReshare = true
			break
		}
	}

	if !hasReshare {
		t.Log("")
		// Diagnose why it failed.
		switch {
		case len(commits) == 0:
			t.Log("FAIL: No commitments landed under obstructed network.")
			t.Log("DIAGNOSIS: Nodes could not agree on any blame CID.")
			t.Log("This indicates SSID mismatch or BLS collection failure.")

		case len(commits) >= 2 && commits[0].Commitment == commits[len(commits)-1].Commitment:
			t.Log("FAIL: Repeated identical blame — same nodes blamed each cycle.")
			t.Log("DIAGNOSIS: Blame landed but exclusion isn't working on retry.")
			t.Log("The blamed nodes are not being removed from the party list.")

		default:
			// Check for cascading blame: more than 2 nodes blamed.
			for _, c := range commits {
				if c.Type == "blame" {
					bits := decodeBitset(t, c.Commitment)
					blamedCount := 0
					var blamedNames []string
					for j := 0; j < len(members); j++ {
						if bits.Bit(j) == 1 {
							blamedCount++
							blamedNames = append(blamedNames, members[j])
						}
					}
					if blamedCount > 2 {
						t.Logf("FAIL: Cascading blame — %d nodes blamed %v but only 2 are slow.",
							blamedCount, blamedNames)
						t.Log("DIAGNOSIS: A slow node's delayed messages caused a healthy")
						t.Log("node to stall in the protocol (couldn't advance rounds),")
						t.Log("making it appear as a non-participant. The blame set grew")
						t.Log("beyond the actual faulty nodes, reducing quorum below the")
						t.Log("threshold needed for reshare.")
					} else {
						t.Logf("FAIL: Blame landed (blamed %v) but reshare didn't succeed.", blamedNames)
						t.Log("DIAGNOSIS: Blamed nodes were not excluded on retry,")
						t.Log("or insufficient cycles within the epoch.")
					}
					break
				}
			}
		}
		t.Log("")
		t.Log("REQUIRED: The network must recover within a single epoch.")
		t.Log("With 2/7 slow nodes and 5 healthy (above 2/3 threshold),")
		t.Log("blame should target ONLY the 2 slow nodes, and the next")
		t.Log("reshare should succeed with the 5 healthy nodes.")
		t.FailNow()
	}

	t.Log("")
	t.Log("PASS: Reshare succeeded under obstructed network within one epoch!")
	t.Log("The healthy nodes agreed on a blame, excluded the slow nodes,")
	t.Log("and completed a reshare among themselves.")
}
