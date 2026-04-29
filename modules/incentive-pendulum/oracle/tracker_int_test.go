package oracle

import (
	"math/rand"
	"sort"
	"strconv"
	"testing"

	"vsc-node/lib/intmath"
	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

// TestLastTickInt_DeterministicAcrossTrackers verifies the load-bearing W2
// guarantee: two FeedTracker instances fed the same on-chain inputs in the
// same order produce byte-identical integer snapshots at the same tick.
//
// The float aggregation in TrustedHivePrice iterates a Go map and is therefore
// non-deterministic across runs (and across nodes). LastTickInt re-derives
// the trusted mean from sorted witness names + SQ64 arithmetic, so the
// resulting snapshot is the same regardless of map iteration order.
func TestLastTickInt_DeterministicAcrossTrackers(t *testing.T) {
	const tickHeight = uint64(100)

	// Five witnesses with distinct quotes. Ingest order varies between the two
	// trackers to maximize the chance of float-side divergence — but the
	// integer snapshot must still match.
	witnesses := []string{"alice", "bob", "carol", "dave", "erin"}
	quotes := map[string]string{
		"alice": "0.31 HBD",
		"bob":   "0.27 HBD",
		"carol": "0.33 HBD",
		"dave":  "0.29 HBD",
		"erin":  "0.30 HBD",
	}

	build := func(order []string) FeedTickSnapshotInt {
		tr := NewFeedTracker()
		// Establish enough signatures + a feed publish for each witness so
		// FeedTrust passes (minSig=4) at the tick.
		for h := uint64(1); h <= 4; h++ {
			for _, w := range order {
				tr.RecordWitnessBlock(w)
				tr.IngestTransactionOps(h, hive_blocks.Tx{
					Operations: []hivego.Operation{{
						Type: "feed_publish",
						Value: map[string]interface{}{
							"publisher": w,
							"exchange_rate": map[string]interface{}{
								"base":  quotes[w],
								"quote": "1 HIVE",
							},
						},
					}},
				})
			}
		}
		tr.TickIfDue(tickHeight)
		return tr.LastTickInt()
	}

	orderA := append([]string(nil), witnesses...)
	orderB := append([]string(nil), witnesses...)
	rand.Shuffle(len(orderB), func(i, j int) { orderB[i], orderB[j] = orderB[j], orderB[i] })

	snapA := build(orderA)
	snapB := build(orderB)

	if snapA.TickBlockHeight != tickHeight || snapB.TickBlockHeight != tickHeight {
		t.Fatalf("tick heights wrong: A=%d B=%d want %d", snapA.TickBlockHeight, snapB.TickBlockHeight, tickHeight)
	}
	if snapA.TrustedHiveMean != snapB.TrustedHiveMean {
		t.Fatalf("integer trusted mean diverges across trackers: A=%d B=%d (orderA=%v orderB=%v)",
			snapA.TrustedHiveMean, snapB.TrustedHiveMean, orderA, orderB)
	}
	if snapA.TrustedHiveOK != snapB.TrustedHiveOK {
		t.Fatalf("trusted ok diverges: A=%v B=%v", snapA.TrustedHiveOK, snapB.TrustedHiveOK)
	}
	if !sliceEqualString(snapA.TrustedWitnessGroup, snapB.TrustedWitnessGroup) {
		t.Fatalf("trusted witness group diverges: A=%v B=%v", snapA.TrustedWitnessGroup, snapB.TrustedWitnessGroup)
	}
}

// TestLastTickInt_MeanMatchesIntegerAverageOfSortedQuotes pins the SQ64 mean
// computation: it must be the integer average of SQ64FromFloat(quote) values
// in sorted witness order.
func TestLastTickInt_MeanMatchesIntegerAverageOfSortedQuotes(t *testing.T) {
	tr := NewFeedTracker()
	witnesses := []string{"alice", "bob", "carol", "dave"}
	rawQuotes := map[string]float64{
		"alice": 0.31,
		"bob":   0.27,
		"carol": 0.33,
		"dave":  0.29,
	}
	for h := uint64(1); h <= 4; h++ {
		for _, w := range witnesses {
			tr.RecordWitnessBlock(w)
			tr.IngestTransactionOps(h, hive_blocks.Tx{
				Operations: []hivego.Operation{{
					Type: "feed_publish",
					Value: map[string]interface{}{
						"publisher": w,
						"exchange_rate": map[string]interface{}{
							"base":  formatHbd(rawQuotes[w]),
							"quote": "1 HIVE",
						},
					},
				}},
			})
		}
	}
	tr.TickIfDue(100)
	snap := tr.LastTickInt()

	// Hand-compute the expected mean: floor(sum(SQ64) / count).
	names := append([]string(nil), witnesses...)
	sort.Strings(names)
	var sum int64
	for _, w := range names {
		sum += int64(intmath.SQ64FromFloat(rawQuotes[w]))
	}
	want := intmath.SQ64(sum / int64(len(names)))
	if snap.TrustedHiveMean != want {
		t.Fatalf("mean mismatch: got %d want %d", snap.TrustedHiveMean, want)
	}
}

// TestLastTickInt_SlashEntriesSortedByWitness verifies the slash list is
// sorted, which is required for byte-determinism of the persisted bson form
// (Go map iteration order is otherwise random).
func TestLastTickInt_SlashEntriesSortedByWitness(t *testing.T) {
	in := map[string]int{
		"zach":  100,
		"alice": 50,
		"mary":  25,
	}
	got := sortedSlashEntries(in)
	if len(got) != 3 {
		t.Fatalf("len got=%d want 3", len(got))
	}
	if got[0].Witness != "alice" || got[1].Witness != "mary" || got[2].Witness != "zach" {
		t.Fatalf("not sorted: %v", got)
	}
	if got[0].Bps != 50 || got[1].Bps != 25 || got[2].Bps != 100 {
		t.Fatalf("bps not preserved: %v", got)
	}
}

func TestLastTickInt_NilTracker(t *testing.T) {
	var tr *FeedTracker
	snap := tr.LastTickInt()
	if snap.TickBlockHeight != 0 || snap.TrustedHiveOK {
		t.Fatalf("nil tracker should return zero snapshot; got %+v", snap)
	}
}

// helpers

func formatHbd(v float64) string {
	// Hive feed_publish exchange-rate strings parse via parseAssetAmount(); a
	// fixed-precision value with the "HBD" suffix round-trips through the parser.
	return strconv.FormatFloat(v, 'f', 6, 64) + " HBD"
}

func sliceEqualString(a, b []string) bool {
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
