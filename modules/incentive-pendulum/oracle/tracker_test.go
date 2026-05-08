package oracle

import (
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

func TestParseHbdPerHivePair(t *testing.T) {
	q, ok := hiveHBDPerHiveFromFeed("0.250 HBD", "1.000 HIVE")
	if !ok {
		t.Fatalf("ok=%v", ok)
	}
	if q.HbdRaw != 250 || q.HiveRaw != 1000 {
		t.Fatalf("q=%+v want {HbdRaw:250 HiveRaw:1000}", q)
	}
}

func TestHbdInterestFromProps(t *testing.T) {
	props := []interface{}{
		[]interface{}{"account_creation_fee", "1.000 TESTS"},
		[]interface{}{"hbd_interest_rate", "2000"},
	}
	v, ok := hbdInterestFromProps(props)
	if !ok || v != 2000 {
		t.Fatalf("v=%d ok=%v", v, ok)
	}
}

func TestFeedTrackerTick(t *testing.T) {
	tr := NewFeedTracker()
	// Fill 4 blocks with same witness so FeedTrust passes at minSig=4.
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("alice")
		tr.IngestTransactionOps(h, hive_blocks.Tx{
			Operations: []hivego.Operation{{
				Type: "feed_publish",
				Value: map[string]interface{}{
					"publisher": "alice",
					"exchange_rate": map[string]interface{}{
						"base":  "0.25 HBD",
						"quote": "1 HIVE",
					},
				},
			}},
		})
	}
	tr.IngestTransactionOps(4, hive_blocks.Tx{
		Operations: []hivego.Operation{{
			Type: "witness_set_properties",
			Value: map[string]interface{}{
				"owner": "alice",
				"props": []interface{}{
					[]interface{}{"hbd_interest_rate", "1500"},
				},
			},
		}},
	})
	tr.TickIfDue(100)
	snap := tr.LastTick()
	if !snap.TrustedHiveOK {
		t.Fatal("expected trusted hive")
	}
	// 0.25 HBD per 1 HIVE = 2500 bps. Allow ±1 bps for integer-floor noise.
	if snap.TrustedHivePriceBps < 2_499 || snap.TrustedHivePriceBps > 2_501 {
		t.Fatalf("priceBps=%d", snap.TrustedHivePriceBps)
	}
	if !snap.HBDInterestRateOK || snap.HBDInterestRateBps != 1500 {
		t.Fatalf("apr=%d ok=%v", snap.HBDInterestRateBps, snap.HBDInterestRateOK)
	}
	if len(snap.TrustedWitnessGroup) != 1 || snap.TrustedWitnessGroup[0] != "alice" {
		t.Fatalf("group=%v", snap.TrustedWitnessGroup)
	}
}

// stubWarmupSource is a minimal in-memory WarmupSource for tracker tests.
type stubWarmupSource struct {
	head   uint64
	blocks []hive_blocks.HiveBlock
	err    error
}

func (s *stubWarmupSource) GetLastProcessedBlock() (uint64, error) {
	return s.head, s.err
}

func (s *stubWarmupSource) FetchStoredBlocks(start, end uint64) ([]hive_blocks.HiveBlock, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]hive_blocks.HiveBlock, 0, len(s.blocks))
	for _, b := range s.blocks {
		if b.BlockNumber >= start && b.BlockNumber <= end {
			out = append(out, b)
		}
	}
	return out, nil
}

func makePublishBlock(bh uint64, witness string) hive_blocks.HiveBlock {
	return hive_blocks.HiveBlock{
		BlockNumber: bh,
		Witness:     witness,
		Transactions: []hive_blocks.Tx{{
			Operations: []hivego.Operation{{
				Type: "feed_publish",
				Value: map[string]interface{}{
					"publisher": witness,
					"exchange_rate": map[string]interface{}{
						"base":  "0.25 HBD",
						"quote": "1.000 HIVE",
					},
				},
			}},
		}},
	}
}

// TestFeedTrackerWarmedColdStart confirms a freshly-constructed tracker is
// not warm — the swap applier and env gate on this to refuse divergent
// reads during catch-up.
func TestFeedTrackerWarmedColdStart(t *testing.T) {
	tr := NewFeedTracker()
	if tr.Warmed() {
		t.Fatal("fresh tracker reported warmed")
	}
}

// TestFeedTrackerWarmupReplay drives Warmup against a stub source covering
// 300 blocks with publishes at every tick boundary and asserts the tracker
// flips to warmed and exposes a populated tick snapshot — the long-running-
// peer steady state.
func TestFeedTrackerWarmupReplay(t *testing.T) {
	src := &stubWarmupSource{head: 300}
	for bh := uint64(1); bh <= 300; bh++ {
		blk := hive_blocks.HiveBlock{BlockNumber: bh, Witness: "alice"}
		// Publish at every tick boundary so each tick's trust check passes.
		if bh == 1 || bh%100 == 0 {
			blk = makePublishBlock(bh, "alice")
		}
		src.blocks = append(src.blocks, blk)
	}

	tr := NewFeedTracker()
	if err := tr.Warmup(src); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if !tr.Warmed() {
		t.Fatal("tracker not warmed after Warmup")
	}
	snap := tr.LastTick()
	if snap.TickBlockHeight != 300 {
		t.Fatalf("tick=%d want 300", snap.TickBlockHeight)
	}
	if !snap.TrustedHiveOK || !snap.HiveMovingAvgOK {
		t.Fatalf("ok flags: trusted=%v ma=%v", snap.TrustedHiveOK, snap.HiveMovingAvgOK)
	}
}

// TestFeedTrackerWarmupIdempotent verifies that calling Warmup on an already-
// warmed tracker is a no-op (early return without re-replaying), so a
// stale-on-restart state engine doesn't double-push the signature window.
func TestFeedTrackerWarmupIdempotent(t *testing.T) {
	src := &stubWarmupSource{head: 300}
	for bh := uint64(1); bh <= 300; bh++ {
		blk := hive_blocks.HiveBlock{BlockNumber: bh, Witness: "alice"}
		if bh == 1 || bh%100 == 0 {
			blk = makePublishBlock(bh, "alice")
		}
		src.blocks = append(src.blocks, blk)
	}

	tr := NewFeedTracker()
	if err := tr.Warmup(src); err != nil {
		t.Fatalf("first Warmup: %v", err)
	}
	signaturesAfterFirst := tr.win.SignatureCount("alice")

	if err := tr.Warmup(src); err != nil {
		t.Fatalf("second Warmup: %v", err)
	}
	if got := tr.win.SignatureCount("alice"); got != signaturesAfterFirst {
		t.Fatalf("signature count drifted: first=%d second=%d (Warmup not idempotent)",
			signaturesAfterFirst, got)
	}
}

// TestFeedTrackerWarmupGenesis covers the fresh-chain path: the source
// reports head=0, Warmup marks the tracker explicitly warmed without
// replaying anything, and consumers proceed past the gate.
func TestFeedTrackerWarmupGenesis(t *testing.T) {
	tr := NewFeedTracker()
	if err := tr.Warmup(&stubWarmupSource{head: 0}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if !tr.Warmed() {
		t.Fatal("expected explicit-warmed flag on genesis-chain Warmup")
	}
}

// TestFeedTrackerWarmedNaturalFill covers the recovery path when explicit
// Warmup wasn't called or failed — natural ProcessBlock ingest fills both
// the signature window and MA ring after enough live blocks.
func TestFeedTrackerWarmedNaturalFill(t *testing.T) {
	tr := NewFeedTracker()
	for bh := uint64(1); bh <= 300; bh++ {
		tr.RecordWitnessBlock("alice")
		if bh == 1 || bh%100 == 0 {
			tr.IngestTransactionOps(bh, hive_blocks.Tx{
				Operations: []hivego.Operation{{
					Type: "feed_publish",
					Value: map[string]interface{}{
						"publisher": "alice",
						"exchange_rate": map[string]interface{}{
							"base":  "0.25 HBD",
							"quote": "1.000 HIVE",
						},
					},
				}},
			})
		}
		tr.TickIfDue(bh)
	}
	if !tr.Warmed() {
		t.Fatal("tracker should be warmed after 300 blocks of organic ingest with periodic publishes")
	}
}

func TestFeedTrackerLastTickReturnsDefensiveCopies(t *testing.T) {
	tr := NewFeedTracker()
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("alice")
	}
	tr.IngestTransactionOps(4, hive_blocks.Tx{
		Operations: []hivego.Operation{{
			Type: "feed_publish",
			Value: map[string]interface{}{
				"publisher": "alice",
				"exchange_rate": map[string]interface{}{
					"base":  "0.25 HBD",
					"quote": "1 HIVE",
				},
			},
		}},
	})
	tr.TickIfDue(100)

	s1 := tr.LastTick()
	s1.TrustedWitnessGroup[0] = "mutated"

	s2 := tr.LastTick()
	if len(s2.TrustedWitnessGroup) != 1 || s2.TrustedWitnessGroup[0] != "alice" {
		t.Fatalf("unexpected group copy behavior: %v", s2.TrustedWitnessGroup)
	}
}
