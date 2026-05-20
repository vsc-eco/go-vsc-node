package devnet

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPendulumFeedPropagation is M2 of the pendulum E2E workstream
// (docs/pendulum/devnet-test-scope.md). The cheapest pendulum test
// in the series and the foundation M3/M4 build on.
//
// What it proves:
//
//  1. The feed-publisher sidecar (started by Devnet.Start alongside
//     the magi nodes) is actually broadcasting `feed_publish` ops to
//     the Hive L1 chain.
//  2. Every magi node ingests those L1 blocks into its hive_blocks
//     collection identically — i.e. the L1-stream observability
//     under pendulum is consistent across all nodes.
//
// What it does NOT prove:
//
//   - That the oracle FeedTracker on each node converges to the
//     same in-memory state. The tracker is in-memory after commit
//     fe70b3f9 ("simplified price snapshots to just the current
//     data") — there is no per-node oracle collection on this
//     branch to read. The settlement-record check in M3 closes that
//     loop indirectly: identical settlement bytes across nodes can
//     only happen if every node's FeedTracker converged.
//   - That settlement was actually produced. That's M3's job.
//
// Failure modes this catches:
//
//   - feed-publisher misconfigured (no feeds broadcast).
//   - feed-publisher running but some nodes lag the L1 stream
//     (uneven counts across nodes).
//   - Feed payload ops not appearing in hive_blocks under the
//     expected JSON key path (would surface as zero counts despite
//     L1 actually carrying them — the hive_blocks regex sweep would
//     return 0 while the feed-publisher log shows broadcasts).
//
// Run with:
//
//	go test -v -run TestPendulumFeedPropagation -timeout 15m ./tests/devnet/
func TestPendulumFeedPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := pendulumTestConfig()
	d, ctx := startDevnetNoKey(t, cfg, 15*time.Minute)

	// The feed-publisher's default tick interval is 240s (see
	// cmd/feed-publisher/main.go header comment). Wait long enough
	// that we expect to see at least 2-3 publishes if it's healthy.
	// 8 minutes is generous and surfaces "publisher silently stuck"
	// scenarios within the test timeout.
	const waitDuration = 8 * time.Minute
	t.Logf("waiting %s for feed-publisher to broadcast feed_publish ops to L1...", waitDuration)
	select {
	case <-ctx.Done():
		t.Fatalf("ctx cancelled while waiting for feeds: %v", ctx.Err())
	case <-time.After(waitDuration):
	}

	// Pick the L1 block range to count over. We scan from block 10
	// (skip the devnet boot setup blocks) to "current head as seen
	// by magi-1" — bounded so a flaky node lagging the stream
	// surfaces clearly via differing counts.
	headBlock, err := d.getLastProcessedBlock(ctx, 1)
	if err != nil {
		t.Fatalf("reading magi-1 head block: %v", err)
	}
	if headBlock < 20 {
		t.Fatalf("magi-1 head block %d is suspiciously low — devnet didn't progress?", headBlock)
	}
	const fromBlock = uint64(10)
	toBlock := headBlock
	t.Logf("counting feed_publish ops across hive_blocks in [%d, %d]...", fromBlock, toBlock)

	// Read feed_publish count from each node.
	counts := make([]int64, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		c, err := countHiveBlocksWithFeedPublishOps(ctx, d, n, fromBlock, toBlock)
		if err != nil {
			t.Errorf("magi-%d: counting feed_publish hive_blocks: %v", n, err)
			continue
		}
		counts[n-1] = c
		t.Logf("magi-%d: %d hive_blocks carry feed_publish in [%d, %d]", n, c, fromBlock, toBlock)
	}

	// Assertion 1: feed-publisher is producing feeds. If every node
	// reports 0, the publisher is dead or misconfigured.
	if counts[0] == 0 {
		// Dump feed-publisher's logs so the operator can see why.
		logs, _ := d.Logs(ctx, "feed-publisher")
		if logs != "" {
			t.Logf("feed-publisher logs:\n%s", truncateLogs(logs, 30))
		}
		t.Fatalf("magi-1 saw zero feed_publish hive_blocks in [%d, %d] — feed-publisher not broadcasting?", fromBlock, toBlock)
	}

	// Assertion 2: every node sees the SAME number. Divergence here
	// means L1 ingest is inconsistent — a node fell behind, or the
	// hive_blocks regex matches different things on different nodes
	// due to schema drift across versions.
	for n := 2; n <= cfg.Nodes; n++ {
		if counts[n-1] != counts[0] {
			t.Errorf("magi-%d feed_publish count=%d differs from magi-1 count=%d in [%d, %d]",
				n, counts[n-1], counts[0], fromBlock, toBlock)
		}
	}

	// Sanity sweep: no node logged feed-related errors. If the
	// state-engine couldn't decode a feed_publish op, we'd see it in
	// per-node logs even if counts at the hive_blocks layer match.
	t.Run("no_feed_ingest_errors_in_node_logs", func(t *testing.T) {
		patterns := []string{
			"feed_publish: invalid",
			"feed_publish decode failed",
			"oracle: feed rejected",
			"pendulum oracle: parse error",
		}
		for n := 1; n <= cfg.Nodes; n++ {
			logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
			if err != nil {
				t.Logf("warning: could not read magi-%d logs: %v", n, err)
				continue
			}
			for _, pat := range patterns {
				if strings.Contains(logs, pat) {
					t.Errorf("magi-%d logs contain %q (feed ingest error)", n, pat)
				}
			}
		}
	})
}
