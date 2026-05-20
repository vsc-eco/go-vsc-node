package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestElectionConsensusDeterminism asserts that every honest node in a
// running devnet records byte-identical election bytes (the BLS-attested
// `data` field plus the canonical weight/member arrays) for the same
// election epoch.
//
// This is a baseline consensus-determinism check for the election
// proposer path. It does not exercise any single fix: it catches the
// *class* of bug where two honest nodes compute divergent election
// results from the same witness set. Known instances of that class:
//
//   - PR #181 commit 6b0c01a4 (review4 HIGH #6): float64 arithmetic in
//     `distWeight = math.Ceil((1 + opt/2) / n)` could round differently
//     across CPU architectures / FP units. The fix promotes the math to
//     integer. Note: the bug is *dormant* in the current tree because
//     REQUIRED_ELECTION_MEMBERS is empty (see modules/election-proposer/
//     election-proposer.go:222), so this test cannot prove the fix
//     directly — but it WILL catch any reintroduction of float math (or
//     any other non-determinism) once required members are populated.
//
//   - PR #181 commit 6b0c01a4 (review4 HIGH #40): map iteration order in
//     `scoreMap()` building the `bannedNodes` slice. The method has no
//     callers in the current tree, so the same dormancy caveat applies.
//
//   - Future variants where any non-deterministic operation (map iter,
//     time.Now, rand, FP, BSON-vs-canonical-bytes drift) leaks into the
//     election proposal path.
//
// What the test does:
//
//  1. Spins up a 5-node devnet with a fast election interval.
//  2. Waits until every node has ingested at least one post-genesis
//     election (epoch >= 1, produced by the running election-proposer
//     rather than the one-shot genesis-elector binary).
//  3. Reads each node's `elections` collection for the chosen epoch and
//     compares the canonical fields byte-by-byte: `data` (the BLS-signed
//     bytes), `weights`, `total_weight`, ordered `members`.
//  4. Asserts no node logged an election-verification rejection or
//     "election_result rejected" / "election cid mismatch" warning
//     during the run.
//
// If this test ever fails, election consensus is broken — any divergence
// in the `data` field means BLS signatures aggregated by different nodes
// over different bytes, which would prevent the election from ever
// landing in a finalised block.
//
// Run with:
//
//	go test -v -run TestElectionConsensusDeterminism -timeout 20m ./tests/devnet/
func TestElectionConsensusDeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnetNoKey(t, cfg, 15*time.Minute)

	// Wait for every node to ingest at least one post-genesis election.
	// Epoch 0 is the genesis election produced by the genesis-elector
	// binary at devnet boot, which doesn't exercise the running
	// election-proposer path — we want epoch >= 1.
	const minEpoch = uint64(1)
	t.Logf("waiting for every node to ingest at least one post-genesis election (epoch >= %d)...", minEpoch)
	for n := 1; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if err := d.waitForElectionEpoch(nodeCtx, n, minEpoch, 8*time.Minute); err != nil {
			cancel()
			t.Fatalf("magi-%d never ingested epoch >= %d: %v", n, minEpoch, err)
		}
		cancel()
	}

	// Pick the lowest epoch present on every node. Higher epochs may not
	// yet have propagated to all nodes evenly when this code runs; the
	// lowest post-genesis epoch is the simplest cross-node comparison
	// point that is guaranteed present everywhere.
	epoch, err := lowestCommonElectionEpoch(ctx, d, cfg.Nodes, minEpoch)
	if err != nil {
		t.Fatalf("finding lowest common election epoch: %v", err)
	}
	t.Logf("comparing election bytes for epoch %d across %d nodes", epoch, cfg.Nodes)

	// Fetch the canonical fields from every node for this epoch.
	records := make([]canonicalElectionRecord, cfg.Nodes)
	for n := 1; n <= cfg.Nodes; n++ {
		rec, err := readCanonicalElection(ctx, d, n, epoch)
		if err != nil {
			t.Fatalf("reading election epoch %d from magi-%d: %v", epoch, n, err)
		}
		records[n-1] = rec
	}

	// Compare every node's record against magi-1. Any divergence is a
	// consensus break — fail loudly and dump enough context to triage.
	base := records[0]
	for n := 2; n <= cfg.Nodes; n++ {
		other := records[n-1]
		if base.Data != other.Data {
			t.Errorf("magi-%d election Data differs from magi-1 at epoch %d:\n  magi-1: %s\n  magi-%d: %s",
				n, epoch, truncate(base.Data, 120), n, truncate(other.Data, 120))
		}
		if base.TotalWeight != other.TotalWeight {
			t.Errorf("magi-%d total_weight=%d differs from magi-1 total_weight=%d at epoch %d",
				n, other.TotalWeight, base.TotalWeight, epoch)
		}
		if !equalUint64s(base.Weights, other.Weights) {
			t.Errorf("magi-%d weights differ from magi-1 at epoch %d:\n  magi-1: %v\n  magi-%d: %v",
				n, epoch, base.Weights, n, other.Weights)
		}
		if !equalStrings(base.MemberAccounts, other.MemberAccounts) {
			t.Errorf("magi-%d members differ from magi-1 at epoch %d:\n  magi-1: %v\n  magi-%d: %v",
				n, epoch, base.MemberAccounts, n, other.MemberAccounts)
		}
	}

	// Sanity: no node logged an election-verification rejection. If any
	// node had rejected the proposal, the BLS aggregate would have come
	// up short and the election would not have landed at all — so a
	// successful read above already implies this. But surface log
	// evidence too, in case future code paths log warnings without
	// breaking the aggregate.
	t.Run("no_election_rejection_warnings", func(t *testing.T) {
		patterns := []string{
			"election_result rejected",
			"election cid mismatch",
			"election verification failed",
			"invalid election proposal",
		}
		for n := 1; n <= cfg.Nodes; n++ {
			logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
			if err != nil {
				t.Logf("warning: could not read magi-%d logs: %v", n, err)
				continue
			}
			for _, pat := range patterns {
				if strings.Contains(logs, pat) {
					t.Errorf("magi-%d logs contain %q (election non-determinism leak?)", n, pat)
				}
			}
		}
	})
}

// canonicalElectionRecord is the subset of fields from a node's
// `elections` collection that must be byte-identical across all honest
// nodes for the same epoch.
type canonicalElectionRecord struct {
	Epoch          uint64
	Data           string
	TotalWeight    uint64
	Weights        []uint64
	MemberAccounts []string
}

// readCanonicalElection fetches the election record for `epoch` from the
// named node and returns the canonical comparison fields. `members` is
// stored as an array of {account, key} objects; we extract just the
// `account` strings in their stored order (which IS the canonical order
// the election proposer wrote them in).
func readCanonicalElection(ctx context.Context, d *Devnet, node int, epoch uint64) (canonicalElectionRecord, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return canonicalElectionRecord{}, err
	}
	defer client.Disconnect(ctx)

	coll := client.Database(d.nodeDbName(node)).Collection("elections")
	var raw struct {
		Epoch       uint64   `bson:"epoch"`
		Data        string   `bson:"data"`
		TotalWeight uint64   `bson:"total_weight"`
		Weights     []uint64 `bson:"weights"`
		Members     []struct {
			Account string `bson:"account"`
		} `bson:"members"`
	}
	if err := coll.FindOne(ctx, bson.M{"epoch": epoch}).Decode(&raw); err != nil {
		return canonicalElectionRecord{}, fmt.Errorf("finding election epoch %d on magi-%d: %w", epoch, node, err)
	}
	accounts := make([]string, len(raw.Members))
	for i, m := range raw.Members {
		accounts[i] = m.Account
	}
	return canonicalElectionRecord{
		Epoch:          raw.Epoch,
		Data:           raw.Data,
		TotalWeight:    raw.TotalWeight,
		Weights:        raw.Weights,
		MemberAccounts: accounts,
	}, nil
}

// lowestCommonElectionEpoch returns the lowest epoch >= minEpoch that
// exists on every node. Used so we compare an election that is
// guaranteed to be present everywhere rather than a high-water mark that
// may not yet have propagated.
func lowestCommonElectionEpoch(ctx context.Context, d *Devnet, nodes int, minEpoch uint64) (uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Disconnect(ctx)

	for epoch := minEpoch; epoch < minEpoch+50; epoch++ {
		present := 0
		for n := 1; n <= nodes; n++ {
			coll := client.Database(d.nodeDbName(n)).Collection("elections")
			count, err := coll.CountDocuments(ctx, bson.M{"epoch": epoch})
			if err != nil {
				return 0, fmt.Errorf("counting epoch %d on magi-%d: %w", epoch, n, err)
			}
			if count > 0 {
				present++
			}
		}
		if present == nodes {
			return epoch, nil
		}
	}
	return 0, fmt.Errorf("no epoch in [%d, %d) present on all %d nodes", minEpoch, minEpoch+50, nodes)
}

func equalUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
