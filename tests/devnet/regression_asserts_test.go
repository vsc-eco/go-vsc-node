package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// regression_asserts_test.go — cross-node consistency assertions for the
// node-wide regression test. The load-bearing checks assert that independent
// nodes AGREE (same balances, contract state, election members, block height)
// rather than asserting exact predicted values, which are fragile against RC/fee
// accounting. Faults are recoverable, so these run against the set of nodes that
// are intended to be up at the time of the check.

// nodeBlock returns a node's last processed L1 block (via MongoDB, the same
// source the other devnet tests poll). Fatals on error; use only for nodes that
// are known to be up (e.g. the never-faulted node 1).
func nodeBlock(t *testing.T, d *Devnet, ctx context.Context, node int) uint64 {
	t.Helper()
	bh, err := d.getLastProcessedBlock(ctx, node)
	if err != nil {
		t.Fatalf("magi-%d last processed block: %v", node, err)
	}
	return bh
}

// nodeBlockOpt reads a node's last processed block without failing. ok=false if
// the node hasn't written its hive_blocks metadata yet (e.g. mid bring-up or
// while restarting).
func nodeBlockOpt(d *Devnet, ctx context.Context, node int) (uint64, bool) {
	bh, err := d.getLastProcessedBlock(ctx, node)
	if err != nil {
		return 0, false
	}
	return bh, true
}

// maxBlock returns the highest readable last-processed-block among nodes.
func maxBlock(d *Devnet, ctx context.Context, nodes []int) uint64 {
	var max uint64
	for _, n := range nodes {
		if bh, ok := nodeBlockOpt(d, ctx, n); ok && bh > max {
			max = bh
		}
	}
	return max
}

// allNodes returns [1..cfg.Nodes].
func (d *Devnet) allNodes() []int {
	ns := make([]int, d.cfg.Nodes)
	for i := range ns {
		ns[i] = i + 1
	}
	return ns
}

// waitNodesConverged polls until every node is readable AND within maxDelta
// blocks of the others. This tolerates bring-up / post-fault catch-up where
// nodes legitimately sit at very different heights for a while. Fails on timeout.
func waitNodesConverged(t *testing.T, d *Devnet, ctx context.Context, nodes []int, maxDelta uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[int]uint64
	for {
		per := map[int]uint64{}
		allReadable := true
		min, max := ^uint64(0), uint64(0)
		for _, n := range nodes {
			bh, ok := nodeBlockOpt(d, ctx, n)
			if !ok {
				allReadable = false
				break
			}
			per[n] = bh
			if bh < min {
				min = bh
			}
			if bh > max {
				max = bh
			}
		}
		if allReadable {
			last = per
			if max-min <= maxDelta {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("nodes did not converge within %v (maxDelta=%d): %v", timeout, maxDelta, last)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled waiting for node convergence: %v", ctx.Err())
		case <-time.After(4 * time.Second):
		}
	}
}

// assertNodesHealthy waits for the given nodes to converge within maxDelta blocks
// and asserts the network is still advancing.
func assertNodesHealthy(t *testing.T, d *Devnet, ctx context.Context, nodes []int, maxDelta uint64) {
	t.Helper()
	waitNodesConverged(t, d, ctx, nodes, maxDelta, 4*time.Minute)
	assertAdvancing(t, d, ctx, nodes, 2*time.Minute)
}

// assertAdvancing polls until the max processed block among nodes increases.
// last_processed_block is a synchronized checkpoint that advances in jumps (it
// lags the streamer), so a single short sample is unreliable — poll over a window.
func assertAdvancing(t *testing.T, d *Devnet, ctx context.Context, nodes []int, timeout time.Duration) {
	t.Helper()
	start := maxBlock(d, ctx, nodes)
	deadline := time.Now().Add(timeout)
	for {
		if maxBlock(d, ctx, nodes) > start {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("network not advancing: head stuck at %d for %v", start, timeout)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context cancelled waiting for advance: %v", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// currentEpoch polls localNodeInfo until it succeeds, returning the node's epoch.
// localNodeInfo errors ("no documents") until the first election is indexed, so
// callers that need the epoch must tolerate that startup window.
func currentEpoch(t *testing.T, d *Devnet, ctx context.Context, node int, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		_, ep, err := d.LocalNodeInfo(ctx, node)
		if err == nil {
			return ep
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("magi-%d localNodeInfo never succeeded within %v: %v", node, timeout, lastErr)
		}
		time.Sleep(4 * time.Second)
	}
}

// assertAllNodesHealthy is assertNodesHealthy over every node.
func assertAllNodesHealthy(t *testing.T, d *Devnet, ctx context.Context, maxDelta uint64) {
	t.Helper()
	assertNodesHealthy(t, d, ctx, d.allNodes(), maxDelta)
}

// assertTxProcessed polls a tx until it reaches a terminal status. PROCESSED or
// CONFIRMED passes; FAILED fails; not-yet-indexed keeps polling until timeout.
func assertTxProcessed(t *testing.T, d *Devnet, ctx context.Context, node int, txId string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, err := d.FindTransactionStatus(ctx, node, txId)
		if err == nil {
			switch status {
			case "PROCESSED", "CONFIRMED":
				return
			case "FAILED":
				t.Errorf("tx %s FAILED on magi-%d", txId, node)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Errorf("tx %s never reached terminal status on magi-%d (last=%q, err=%v)", txId, node, status, err)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func balanceField(b *BalanceRecord, asset string) int64 {
	switch asset {
	case "hbd":
		return b.Hbd
	case "hbd_savings":
		return b.HbdSavings
	case "hive_consensus":
		return b.HiveConsensus
	case "consensus_unstaking":
		return b.ConsensusUnstaking
	default:
		return b.Hive
	}
}

// assertBalancesAgree asserts every node reports the same balance for the given
// account+asset. Returns the agreed value (0 if it errors out).
func assertBalancesAgree(t *testing.T, d *Devnet, ctx context.Context, nodes []int, account, asset string) int64 {
	t.Helper()
	var want int64
	var wantNode int
	for i, n := range nodes {
		bal, err := d.GetAccountBalance(ctx, n, account)
		if err != nil {
			t.Errorf("balance %s/%s on magi-%d: %v", account, asset, n, err)
			continue
		}
		got := balanceField(bal, asset)
		if i == 0 {
			want, wantNode = got, n
			continue
		}
		if got != want {
			t.Errorf("balance disagreement for %s/%s: magi-%d=%d vs magi-%d=%d",
				account, asset, n, got, wantNode, want)
		}
	}
	return want
}

// assertContractState asserts the contract returns the expected values for the
// given keys on a SINGLE node. Contract state lives in the DA layer and is
// fetched on demand, so it is NOT reliably present on every node — in
// particular, nodes that restarted return null for keys whose state data they
// never fetched. So we verify correctness on a node known to hold it (the
// deploy/query node) rather than asserting cross-node agreement.
func assertContractState(t *testing.T, d *Devnet, ctx context.Context, node int, contractId string, want map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, err := d.GetStateByKeys(ctx, node, contractId, keys)
		ok := err == nil
		if ok {
			for k, v := range want {
				if got, present := st[k]; !present || fmt.Sprintf("%v", got) != v {
					ok = false
					break
				}
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			st, _ := d.GetStateByKeys(ctx, node, contractId, keys)
			t.Errorf("contract %s state on magi-%d = %v, want %v", contractId, node, st, want)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

// assertElectionAgrees asserts every node reports identical members and weights
// for the given epoch.
func assertElectionAgrees(t *testing.T, d *Devnet, ctx context.Context, nodes []int, epoch uint64) {
	t.Helper()
	var want string
	var wantNode int
	for i, n := range nodes {
		e, err := d.GetElectionGQL(ctx, n, epoch)
		if err != nil {
			t.Errorf("election %d on magi-%d: %v", epoch, n, err)
			continue
		}
		if e == nil {
			t.Errorf("election %d missing on magi-%d", epoch, n)
			continue
		}
		b, _ := json.Marshal(struct {
			M []string
			W []uint64
		}{e.Members, e.Weights})
		got := string(b)
		if i == 0 {
			want, wantNode = got, n
			continue
		}
		if got != want {
			t.Errorf("election %d disagreement: magi-%d=%s vs magi-%d=%s",
				epoch, n, got, wantNode, want)
		}
	}
}

// assertElectionEpochAdvanced asserts a node's current epoch is strictly greater
// than fromEpoch.
func assertElectionEpochAdvanced(t *testing.T, d *Devnet, ctx context.Context, node int, fromEpoch uint64) uint64 {
	t.Helper()
	epoch := currentEpoch(t, d, ctx, node, 2*time.Minute)
	if epoch <= fromEpoch {
		t.Errorf("epoch did not advance on magi-%d: %d <= %d", node, epoch, fromEpoch)
	}
	return epoch
}

// faultSet tracks which nodes are intentionally down/isolated so the chaos stage
// can enforce the quorum floor (>=2/3 accessible) before escalating.
type faultSet struct {
	down map[int]bool
}

func newFaultSet() *faultSet { return &faultSet{down: map[int]bool{}} }

func (f *faultSet) markDown(n int) { f.down[n] = true }
func (f *faultSet) markUp(n int)   { delete(f.down, n) }

// reachable returns how many of total nodes are not currently faulted.
func (f *faultSet) reachable(total int) int { return total - len(f.down) }

// requireReachable fails the test if applying a fault would drop reachable nodes
// below floor. Call BEFORE applying a new fault.
func (f *faultSet) requireReachable(t *testing.T, total, floor int) {
	t.Helper()
	if r := f.reachable(total); r < floor {
		t.Fatalf("quorum floor violated: %d reachable < %d required (down=%v)", r, floor, f.down)
	}
}

// fmtDown is a small helper for log lines.
func (f *faultSet) String() string { return fmt.Sprintf("%v", f.down) }
