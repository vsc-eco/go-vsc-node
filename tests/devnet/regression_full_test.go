package devnet

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
)

// regression_full_test.go — TestFullNetworkRegression, the node-wide multi-stage
// regression test. It drives many of every network op, holds multiple elections
// (including a hard witness removal while a key is active), deploys and calls
// contracts (incl. TSS), and injects recoverable faults at every stage, asserting
// the network recovers and stays consistent across all nodes.
//
// This is intentionally long (~50-65 min). It is gated out of `make test` and
// `make test-full`; run it with `make test-regression`. See the Makefile.
//
// Design and per-fault recoverability rationale live in the plan file and in
// .claude/CLAUDE.md (TSS constraints). All faults keep >= 2/3 (>=5 of 7) nodes
// accessible so recovery is guaranteed regardless of the down nodes' health.

const (
	regNodes       = 7
	regGenesis     = 7
	regElectionInt = 100 // ~5 min/epoch: stable committee window per election
	regRotateInt   = 20  // reshare every ~60s

	numTransfers  = 12
	numStakeHbd   = 6
	numConsStake  = 6
	numWithdraws  = 6
	numStateCalls = 24
	numSigns      = 4

	healthDelta = 20 // max block spread tolerated between healthy nodes
)

// regressionConfig returns the 7-node devnet config for the full regression run.
func regressionConfig() *Config {
	cfg := DefaultConfig()
	cfg.Nodes = regNodes
	cfg.GenesisNode = regGenesis
	cfg.SkipFunding = false // need witness funds for deposits + contract fees
	cfg.LogLevel = "info,tss=trace"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: regElectionInt,
		},
		TssParams: &params.TssParams{
			RotateInterval:     regRotateInt,
			SignInterval:       10,
			ReadinessOffset:    5,
			ReshareTimeout:     2 * time.Minute,
			DefaultTimeout:     1 * time.Minute,
			CommitDelay:        2 * time.Second,
			WaitForSigsTimeout: 10 * time.Second,
			ReshareSyncDelay:   2 * time.Second,
			PreParamsTimeout:   10 * time.Minute,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	return cfg
}

// TestRegressionBringup is a fast smoke test for the devnet bring-up + genesis
// path used by TestFullNetworkRegression. It validates that the genesis election
// forms a committee large enough to ratify the first election (epoch 1) and the
// network advances — the part most prone to bootstrap flakiness. Use it to
// iterate on genesis/election issues (~12 min) without the full multi-stage run:
//
//	go test -count=1 -v -timeout 30m -run TestRegressionBringup ./tests/devnet
func TestRegressionBringup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet bring-up smoke test in short mode")
	}
	requireDocker(t)

	cfg := regressionConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Log("starting 7-node devnet...")
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 6*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never ratified (genesis committee too small?): %v", err)
	}
	assertAllNodesHealthy(t, d, ctx, healthDelta)
	t.Logf("bring-up OK: epoch=%d", currentEpoch(t, d, ctx, 1, 2*time.Minute))
}

func TestFullNetworkRegression(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping node-wide regression test in short mode")
	}
	requireDocker(t)

	cfg := regressionConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
	t.Cleanup(cancel)

	// Build the call-tss contract up front so the TSS stage doesn't pay the
	// Docker TinyGo build under load.
	tssWasm, err := BuildCallTssContract(ctx)
	if err != nil {
		t.Fatalf("building call-tss contract: %v", err)
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Log("starting 7-node devnet (this takes ~12 min)...")
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// Shared state across stages.
	var (
		stateContractId string
		fullKeyId       string
		baselineEpoch   uint64
	)

	// ── S0: baseline ────────────────────────────────────────────────
	ok := t.Run("S0_baseline", func(t *testing.T) {
		if err := d.WaitForBlockProcessing(ctx, 1, 30, 6*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("network never reached block 30: %v", err)
		}
		// Wait for the first on-chain election to be indexed. Until then the
		// localNodeInfo / oracle resolvers look up the active election and return
		// "no documents". With ElectionInterval=100 this lands around block 100.
		t.Log("S0: waiting for first election (epoch >= 1)...")
		if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("first election never indexed: %v", err)
		}
		assertAllNodesHealthy(t, d, ctx, healthDelta)
		baselineEpoch = currentEpoch(t, d, ctx, 1, 2*time.Minute)
		t.Logf("baseline: epoch=%d", baselineEpoch)
	})
	if !ok {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("bring-up failed; aborting regression")
	}

	// ── S1: ledger op storm + single-node restart ───────────────────
	t.Run("S1_ledger_storm_restart", func(t *testing.T) {
		var txids []string
		// Deposits: every witness deposits HIVE and HBD into the VSC ledger.
		for n := 1; n <= cfg.Nodes; n++ {
			if tx, err := d.Deposit(ctx, n, "100.000", "hive"); err != nil {
				t.Errorf("deposit hive w%d: %v", n, err)
			} else {
				txids = append(txids, tx)
			}
			if tx, err := d.Deposit(ctx, n, "20.000", "hbd"); err != nil {
				t.Errorf("deposit hbd w%d: %v", n, err)
			} else {
				txids = append(txids, tx)
			}
		}
		if !waitBalancePositive(t, d, ctx, 1, "hive:"+d.witnessAccount(1), "hive", 3*time.Minute) {
			dumpDiagnostics(t, d, ctx)
			t.Fatal("deposits never credited the VSC ledger")
		}

		fireTransfers := func(start, end int) {
			for i := start; i < end; i++ {
				from := (i % cfg.Nodes) + 1
				to := ((i + 1) % cfg.Nodes) + 1
				if tx, err := d.Transfer(from, to, "1.000", "hive", fmt.Sprintf("t%d", i)); err != nil {
					t.Errorf("transfer %d: %v", i, err)
				} else {
					txids = append(txids, tx)
				}
			}
		}
		fireTransfers(0, numTransfers/2)

		// ── recoverable fault: restart node 3 mid-storm (6/7 keep producing)
		t.Log("S1 fault: stopping magi-3")
		if err := d.StopNode(ctx, 3); err != nil {
			t.Fatalf("stop node 3: %v", err)
		}

		fireTransfers(numTransfers/2, numTransfers)
		for i := 0; i < numStakeHbd; i++ {
			w := (i % cfg.Nodes) + 1
			if _, err := d.StakeHBD(w, w, "1.000"); err != nil {
				t.Errorf("stake_hbd %d: %v", i, err)
			}
		}
		for i := 0; i < numConsStake; i++ {
			w := (i % cfg.Nodes) + 1
			if _, err := d.ConsensusStake(w, w, "1.000"); err != nil {
				t.Errorf("consensus_stake %d: %v", i, err)
			}
		}
		for i := 0; i < numWithdraws; i++ {
			w := (i % cfg.Nodes) + 1
			if _, err := d.Withdraw(w, d.witnessAccount(w), "1.000", "hive", "w"); err != nil {
				t.Errorf("withdraw %d: %v", i, err)
			}
		}

		// ── recover
		t.Log("S1 recover: restarting magi-3")
		if err := d.StartNode(ctx, 3); err != nil {
			t.Fatalf("start node 3: %v", err)
		}
		if err := d.WaitForBlockProcessing(ctx, 3, nodeBlock(t, d, ctx, 1), 5*time.Minute); err != nil {
			t.Fatalf("magi-3 never caught up: %v", err)
		}

		assertAllNodesHealthy(t, d, ctx, healthDelta)
		for _, tx := range sampleStrs(txids, 5) {
			assertTxProcessed(t, d, ctx, 2, tx)
		}
		// node 1 and the rejoined node 3 must agree on balances.
		assertBalancesAgree(t, d, ctx, []int{1, 3}, "hive:"+d.witnessAccount(1), "hive")
		assertBalancesAgree(t, d, ctx, []int{1, 3}, "hive:"+d.witnessAccount(1), "hbd_savings")
	})

	// ── S2: contract deploy + call storm + partition ────────────────
	t.Run("S2_contracts_partition", func(t *testing.T) {
		// Use the freshly-built call-tss wasm (it carries setString/getString)
		// rather than the committed contract_test.wasm artifact, which is stale
		// (built months before its source) and validated by no running test.
		cid, err := d.DeployContract(ctx, ContractDeployOpts{
			WasmPath:     tssWasm,
			Name:         "regtest-state",
			DeployerNode: 1,
			GQLNode:      2,
		})
		if err != nil {
			t.Fatalf("deploy state contract: %v", err)
		}
		stateContractId = cid
		t.Logf("deployed state contract %s", cid)

		if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
			t.Fatalf("nodes not synced post-deploy: %v", err)
		}

		// Validate a round-trip (catches a stale committed wasm artifact).
		if _, err := d.CallContract(ctx, 2, cid, "setString", "rk0,rv0"); err != nil {
			t.Fatalf("setString round-trip call: %v", err)
		}
		if !waitStateKey(t, d, ctx, 2, cid, "rk0", "rv0", 2*time.Minute) {
			dumpDiagnostics(t, d, ctx)
			t.Fatal("contract_test.wasm round-trip failed (artifact stale?)")
		}

		for i := 1; i <= numStateCalls/2; i++ {
			node := (i % 5) + 1 // callers among nodes 1..5
			if _, err := d.CallContract(ctx, node, cid, "setString", fmt.Sprintf("k%d,v%d", i, i)); err != nil {
				t.Errorf("setString %d: %v", i, err)
			}
		}

		// ── recoverable fault: partition {6,7} from {1..5} (majority keeps quorum)
		majority := []int{1, 2, 3, 4, 5}
		minority := []int{6, 7}
		t.Log("S2 fault: partition {6,7} | {1..5}")
		partitionGroups(t, d, ctx, majority, minority)

		for i := numStateCalls/2 + 1; i <= numStateCalls; i++ {
			node := (i % 5) + 1
			if _, err := d.CallContract(ctx, node, cid, "setString", fmt.Sprintf("k%d,v%d", i, i)); err != nil {
				t.Errorf("setString %d: %v", i, err)
			}
		}

		// ── recover
		t.Log("S2 recover: healing partition")
		healGroups(t, d, ctx, majority, minority)
		head := nodeBlock(t, d, ctx, 1)
		for _, n := range minority {
			if err := d.WaitForBlockProcessing(ctx, n, head, 5*time.Minute); err != nil {
				t.Fatalf("magi-%d never re-synced: %v", n, err)
			}
		}

		assertAllNodesHealthy(t, d, ctx, healthDelta)
		// Contract state lives in the DA layer (not fully replicated to every
		// node), so verify correctness on the query node rather than requiring
		// cross-node agreement. Keys written before AND during the partition must
		// be present after heal.
		want := map[string]string{"rk0": "rv0", "k1": "v1", "k2": "v2"}
		want[fmt.Sprintf("k%d", numStateCalls)] = fmt.Sprintf("v%d", numStateCalls)
		assertContractState(t, d, ctx, 2, cid, want)
	})

	// ── S3: TSS lifecycle via contract + disconnect -> blame -> recovery
	t.Run("S3_tss_lifecycle_blame", func(t *testing.T) {
		cid, err := d.DeployContract(ctx, ContractDeployOpts{
			WasmPath:     tssWasm,
			Name:         "call-tss",
			DeployerNode: 1,
			GQLNode:      2,
		})
		if err != nil {
			t.Fatalf("deploy call-tss: %v", err)
		}
		if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
			t.Fatalf("not synced post-deploy: %v", err)
		}

		if _, err := d.CallContract(ctx, 2, cid, "tssCreate", `{"key_name":"regKey","epochs":20}`); err != nil {
			t.Fatalf("tssCreate: %v", err)
		}
		fullKeyId = cid + "-regKey"
		if _, err := d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId}, 5*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("key never created: %v", err)
		}
		keygen, err := d.WaitForCommitment(ctx, 2, bson.M{"key_id": fullKeyId, "type": "keygen"}, 10*time.Minute)
		if err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("keygen never landed: %v", err)
		}
		t.Logf("keygen at block %d epoch %d", keygen.BlockHeight, keygen.Epoch)
		if _, err := d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId, "status": "active"}, 3*time.Minute); err != nil {
			t.Fatalf("key never became active: %v", err)
		}
		// NOTE: reshares only fire when the key's epoch is behind the current
		// election (tss.go:1140 "skipping reshare, commitment epoch meets or
		// exceeds current"). With ElectionInterval=100 (~5 min/epoch), a reshare
		// happens roughly once per epoch, so the recovery reshare below proves
		// resharing works without a separate (slow) steady-state reshare wait.

		// signing storm
		for i := 0; i < numSigns; i++ {
			if _, err := d.CallContract(ctx, 2, cid, "tssSign",
				fmt.Sprintf(`{"key_name":"regKey","msg_hex":"%s"}`, msg32hex(i))); err != nil {
				t.Errorf("tssSign %d: %v", i, err)
			}
		}
		if !waitTssRequestComplete(t, d, ctx, 2, fullKeyId, 5*time.Minute) {
			t.Errorf("no TSS signing request completed")
		}

		// renew
		if _, err := d.CallContract(ctx, 2, cid, "tssRenew", `{"key_name":"regKey","epochs":20}`); err != nil {
			t.Errorf("tssRenew: %v", err)
		}

		// ── recoverable fault: disconnect node 5 -> reshare excludes it -> reconnect
		// A node disconnected BEFORE the readiness window misses its readiness
		// attestation, so the next reshare cleanly EXCLUDES it (no blame — blame
		// is for FALSE readiness, where a node attests then fails to participate).
		// The key reshares to the surviving committee and stays alive.
		before := nodeBlock(t, d, ctx, 2)
		t.Log("S3 fault: disconnecting magi-5")
		if err := d.Disconnect(ctx, 5); err != nil {
			t.Fatalf("disconnect 5: %v", err)
		}

		reshareOut, err := d.WaitForCommitment(ctx, 2, bson.M{
			"key_id": fullKeyId, "type": "reshare",
			"block_height": bson.M{"$gt": before},
		}, 14*time.Minute)
		if err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("reshare excluding offline node never landed: %v", err)
		}
		assertNotInCommittee(t, d, ctx, reshareOut, 5)
		if _, err := d.WaitForTssKey(ctx, 2, bson.M{"id": fullKeyId, "status": "active"}, 2*time.Minute); err != nil {
			t.Errorf("key not active after excluding node 5: %v", err)
		}

		// ── recover: reconnect node 5; it re-syncs and the network keeps reshaping
		t.Log("S3 recover: reconnecting magi-5")
		if err := d.Reconnect(ctx, 5); err != nil {
			t.Fatalf("reconnect 5: %v", err)
		}
		if err := d.WaitForBlockProcessing(ctx, 5, nodeBlock(t, d, ctx, 2), 6*time.Minute); err != nil {
			t.Errorf("magi-5 never re-synced after reconnect: %v", err)
		}
		after := nodeBlock(t, d, ctx, 2)
		if _, err := d.WaitForCommitment(ctx, 2, bson.M{
			"key_id": fullKeyId, "type": "reshare",
			"block_height": bson.M{"$gt": after},
		}, 14*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("post-recovery reshare never landed: %v", err)
		}
		assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)
		assertAllNodesHealthy(t, d, ctx, healthDelta)
	})

	// ── S4: elections across epochs + hard witness removal under latency
	t.Run("S4_elections_churn", func(t *testing.T) {
		if fullKeyId == "" {
			t.Skip("S3 did not establish a TSS key")
		}
		ep0 := currentEpoch(t, d, ctx, 2, 2*time.Minute)

		// Additive weight shift — exercises consensus_stake without changing
		// membership (always safe for TSS party lists).
		if tx, err := d.ConsensusStake(1, 1, "50.000"); err != nil {
			t.Errorf("consensus_stake shift: %v", err)
		} else {
			assertTxProcessed(t, d, ctx, 2, tx)
		}

		// Hard removal: drop node 4's consensus stake below MinStake AND take it
		// fully offline while it still holds an active key share. Recovery is via
		// on-chain blame -> exclusion from both committees -> reshare to the
		// surviving 6 (>= threshold+1 = 5), independent of the slow de-election.
		survivors := []int{1, 2, 3, 5, 6, 7}
		t.Log("S4: unstaking + stopping magi-4 while it holds an active key")
		if tx, err := d.ConsensusUnstake(4, "1500.000"); err != nil {
			t.Errorf("consensus_unstake node4: %v", err)
		} else {
			assertTxProcessed(t, d, ctx, 2, tx)
		}
		before := nodeBlock(t, d, ctx, 2)
		if err := d.StopNode(ctx, 4); err != nil {
			t.Fatalf("stop node 4: %v", err)
		}

		// Stress the survivors' gossip timing during the transition.
		if err := d.AddOutboundLatency(ctx, 1, 2, 1500, 250); err != nil {
			t.Logf("add latency: %v", err)
		}

		// The key must reshare to a committee that EXCLUDES the down node 4.
		// Any successful reshare after node 4 went down inherently excludes it
		// (else it would time out and produce blame instead).
		// Excluding the down node 4 takes TWO epoch boundaries: the first
		// reshare times out (node 4 silent) and blames it; the next reshare
		// reads that blame and succeeds without it. At ~5 min/epoch allow ~15m.
		reshare, err := d.WaitForCommitment(ctx, 2, bson.M{
			"key_id": fullKeyId, "type": "reshare",
			"block_height": bson.M{"$gt": before},
		}, 16*time.Minute)
		if err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("reshare to surviving committee never landed: %v", err)
		}
		assertNotInCommittee(t, d, ctx, reshare, 4)

		// Drive at least 2 epoch transitions (multiple elections).
		target := ep0 + 2
		if err := d.waitForElectionEpoch(ctx, 2, target, 20*time.Minute); err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("epoch %d never reached: %v", target, err)
		}

		// ── recover
		if err := d.RemoveLatency(ctx, 1); err != nil {
			t.Logf("remove latency: %v", err)
		}
		assertElectionEpochAdvanced(t, d, ctx, 2, ep0)
		assertElectionAgrees(t, d, ctx, survivors, reshare.Epoch)
		assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)
		assertNodesHealthy(t, d, ctx, survivors, healthDelta)

		// Fold node 4 back in and wait for catch-up so S5 starts with 7 nodes.
		t.Log("S4: restarting magi-4")
		if err := d.StartNode(ctx, 4); err != nil {
			t.Logf("start node 4: %v", err)
		}
		if err := d.WaitForBlockProcessing(ctx, 4, nodeBlock(t, d, ctx, 1), 5*time.Minute); err != nil {
			t.Errorf("magi-4 never re-synced: %v", err)
		}
	})

	// ── S5: chaos — concurrent op mix + sequenced recoverable faults ─
	t.Run("S5_chaos", func(t *testing.T) {
		if stateContractId == "" {
			t.Skip("no state contract from S2")
		}
		chaosStart := nodeBlock(t, d, ctx, 1)
		fs := newFaultSet()
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var opErrs int64

		wg.Add(1)
		go func() {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				i++
				node := (i % 5) + 1 // keep callers on the always-up majority side
				if _, err := d.CallContract(ctx, node, stateContractId, "setString",
					fmt.Sprintf("c%d,cv%d", i, i)); err != nil {
					atomic.AddInt64(&opErrs, 1)
				}
				if _, err := d.Transfer((i%5)+1, ((i+2)%5)+1, "0.500", "hive", "chaos"); err != nil {
					atomic.AddInt64(&opErrs, 1)
				}
				time.Sleep(2 * time.Second)
			}
		}()

		// fault 1: transient latency
		_ = d.AddLatency(ctx, 1, 2, 800, 200)
		time.Sleep(30 * time.Second)
		_ = d.RemoveLatency(ctx, 1)
		_ = d.RemoveLatency(ctx, 2)

		// fault 2: stop node 2 for ~1 reshare cycle
		fs.requireReachable(t, cfg.Nodes, 5)
		fs.markDown(2)
		_ = d.StopNode(ctx, 2)
		time.Sleep(75 * time.Second)
		_ = d.StartNode(ctx, 2)
		fs.markUp(2)
		if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 5*time.Minute); err != nil {
			t.Errorf("node 2 didn't catch up: %v", err)
		}

		// fault 3: isolate node 7
		fs.requireReachable(t, cfg.Nodes, 5)
		fs.markDown(7)
		_ = d.Disconnect(ctx, 7)
		time.Sleep(75 * time.Second)
		_ = d.Reconnect(ctx, 7)
		fs.markUp(7)
		if err := d.WaitForBlockProcessing(ctx, 7, nodeBlock(t, d, ctx, 1), 5*time.Minute); err != nil {
			t.Errorf("node 7 didn't catch up: %v", err)
		}

		close(stop)
		wg.Wait()
		t.Logf("chaos op errors during faults (some expected): %d", opErrs)

		assertAllNodesHealthy(t, d, ctx, healthDelta)
		if fullKeyId != "" {
			if _, err := d.WaitForCommitment(ctx, 1, bson.M{
				"key_id": fullKeyId, "type": "reshare",
				"block_height": bson.M{"$gt": chaosStart},
			}, 14*time.Minute); err != nil {
				t.Errorf("no reshare landed after chaos: %v", err)
			}
		}
		assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)
	})

	// ── S6: final full-network consistency ──────────────────────────
	t.Run("S6_final_consistency", func(t *testing.T) {
		target := nodeBlock(t, d, ctx, 1)
		for _, n := range d.allNodes() {
			if err := d.WaitForBlockProcessing(ctx, n, target, 5*time.Minute); err != nil {
				t.Errorf("magi-%d didn't reach block %d: %v", n, target, err)
			}
		}
		assertAllNodesHealthy(t, d, ctx, healthDelta)

		finalEpoch := currentEpoch(t, d, ctx, 1, 2*time.Minute)
		if finalEpoch <= baselineEpoch {
			t.Errorf("epoch did not advance over the run: %d <= %d", finalEpoch, baselineEpoch)
		}

		// cross-node consistency over ALL nodes
		assertElectionAgrees(t, d, ctx, d.allNodes(), baselineEpoch)
		if stateContractId != "" {
			assertContractState(t, d, ctx, 2, stateContractId,
				map[string]string{"rk0": "rv0", "k1": "v1", "k2": "v2"})
		}
		assertBalancesAgree(t, d, ctx, d.allNodes(), "hive:"+d.witnessAccount(1), "hive")
		assertBalancesAgree(t, d, ctx, d.allNodes(), "hive:"+d.witnessAccount(2), "hive")
		assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

		t.Logf("regression complete: epoch %d -> %d", baselineEpoch, finalEpoch)
	})
}

// ── stage helpers ───────────────────────────────────────────────────

// waitBalancePositive polls until an account's asset balance is > 0.
func waitBalancePositive(t *testing.T, d *Devnet, ctx context.Context, node int, account, asset string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		bal, err := d.GetAccountBalance(ctx, node, account)
		if err == nil && balanceField(bal, asset) > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

// waitStateKey polls contract state until key == want.
func waitStateKey(t *testing.T, d *Devnet, ctx context.Context, node int, cid, key, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, err := d.GetStateByKeys(ctx, node, cid, []string{key})
		if err == nil {
			if v, ok := st[key]; ok && fmt.Sprintf("%v", v) == want {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(3 * time.Second)
	}
}

// waitTssRequestComplete polls until at least one signing request for keyId is
// complete with a non-empty signature.
func waitTssRequestComplete(t *testing.T, d *Devnet, ctx context.Context, node int, keyId string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		reqs, err := d.GetTssRequests(ctx, node, keyId)
		if err == nil {
			for _, r := range reqs {
				if r.Status == "complete" && r.Sig != "" {
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// partitionGroups blocks traffic between every cross-group node pair.
func partitionGroups(t *testing.T, d *Devnet, ctx context.Context, a, b []int) {
	t.Helper()
	for _, x := range a {
		for _, y := range b {
			if err := d.Partition(ctx, x, y); err != nil {
				t.Fatalf("partition %d<->%d: %v", x, y, err)
			}
		}
	}
}

// healGroups restores traffic between every cross-group node pair.
func healGroups(t *testing.T, d *Devnet, ctx context.Context, a, b []int) {
	t.Helper()
	for _, x := range a {
		for _, y := range b {
			if err := d.Heal(ctx, x, y); err != nil {
				t.Logf("heal %d<->%d: %v", x, y, err)
			}
		}
	}
}

// assertNotInCommittee asserts a reshare commitment's committee bitset EXCLUDES
// the witness at node (its bit is 0).
func assertNotInCommittee(t *testing.T, d *Devnet, ctx context.Context, commit *TssCommitmentDoc, node int) {
	t.Helper()
	members, err := d.GetElectionMembers(ctx, 2, commit.Epoch)
	if err != nil {
		t.Fatalf("election members for epoch %d: %v", commit.Epoch, err)
	}
	bits := decodeBitset(t, commit.Commitment)
	target := d.witnessAccount(node)
	for j, m := range members {
		if m == target && bits.Bit(j) == 1 {
			t.Errorf("reshare committee still includes removed node %s (bits=%s)", target, bits.Text(2))
			return
		}
	}
}

// sampleStrs returns up to n roughly-evenly-spaced elements of s.
func sampleStrs(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	step := len(s) / n
	out := make([]string, 0, n)
	for i := 0; i < len(s) && len(out) < n; i += step {
		out = append(out, s[i])
	}
	return out
}

// msg32hex returns a deterministic 32-byte message as hex for TSS signing.
func msg32hex(seed int) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(seed + i)
	}
	return hex.EncodeToString(b)
}
