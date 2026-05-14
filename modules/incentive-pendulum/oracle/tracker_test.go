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
	tr := NewFeedTracker(false)
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
	tr := NewFeedTracker(false)
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

	tr := NewFeedTracker(false)
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

	tr := NewFeedTracker(false)
	if err := tr.Warmup(src); err != nil {
		t.Fatalf("first Warmup: %v", err)
	}
	signaturesAfterFirst := tr.win.BlocksProducedBy("alice")

	if err := tr.Warmup(src); err != nil {
		t.Fatalf("second Warmup: %v", err)
	}
	if got := tr.win.BlocksProducedBy("alice"); got != signaturesAfterFirst {
		t.Fatalf("signature count drifted: first=%d second=%d (Warmup not idempotent)",
			signaturesAfterFirst, got)
	}
}

// TestFeedTrackerWarmupGenesis covers the fresh-chain path: the source
// reports head=0, Warmup marks the tracker explicitly warmed without
// replaying anything, and consumers proceed past the gate.
func TestFeedTrackerWarmupGenesis(t *testing.T) {
	tr := NewFeedTracker(false)
	if err := tr.Warmup(&stubWarmupSource{head: 0}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	if !tr.Warmed() {
		t.Fatal("expected explicit-warmed flag on genesis-chain Warmup")
	}
}

// feedPublishOp is a small constructor for the on-the-wire feed_publish
// op shape that ingest expects (publisher + exchange_rate map).
func feedPublishOp(publisher string) hive_blocks.Tx {
	return hive_blocks.Tx{
		Operations: []hivego.Operation{{
			Type: "feed_publish",
			Value: map[string]interface{}{
				"publisher": publisher,
				"exchange_rate": map[string]interface{}{
					"base":  "0.25 HBD",
					"quote": "1.000 HIVE",
				},
			},
		}},
	}
}

func witnessSetPropsOp(owner string, rate string) hive_blocks.Tx {
	return hive_blocks.Tx{
		Operations: []hivego.Operation{{
			Type: "witness_set_properties",
			Value: map[string]interface{}{
				"owner": owner,
				"props": []interface{}{
					[]interface{}{"hbd_interest_rate", rate},
				},
			},
		}},
	}
}

// TestFeedTracker_IngestGate_RejectsNonProducer covers the spam-prevention
// path: a feed_publish from an account that has not produced any L1 blocks
// in the rolling window is silently dropped instead of growing the maps.
func TestFeedTracker_IngestGate_RejectsNonProducer(t *testing.T) {
	tr := NewFeedTracker(false)
	// "spammer" never appears in RecordWitnessBlock — so BlocksProducedBy=0.
	tr.IngestTransactionOps(50, feedPublishOp("spammer"))
	tr.IngestTransactionOps(50, witnessSetPropsOp("spammer", "1500"))

	if _, ok := tr.quotes["spammer"]; ok {
		t.Fatalf("spammer's feed_publish was accepted; quotes=%v", tr.quotes)
	}
	if _, ok := tr.lastFeedBlk["spammer"]; ok {
		t.Fatalf("spammer's lastFeedBlk persisted")
	}
	if _, ok := tr.witnessProps["spammer"]; ok {
		t.Fatalf("spammer's props persisted")
	}
}

// TestFeedTracker_IngestGate_AcceptsProducer covers the happy path: an
// account that has produced at least one block in the window can publish a
// feed and have it stored.
func TestFeedTracker_IngestGate_AcceptsProducer(t *testing.T) {
	tr := NewFeedTracker(false)
	tr.RecordWitnessBlock("alice")
	tr.IngestTransactionOps(50, feedPublishOp("alice"))

	q, ok := tr.quotes["alice"]
	if !ok {
		t.Fatalf("alice's feed_publish was rejected despite producing")
	}
	if q.HbdRaw != 250 || q.HiveRaw != 1000 {
		t.Fatalf("quote not stored correctly: %+v", q)
	}
}

// TestFeedTracker_IngestGate_PublishAlongsideOwnProduction confirms a
// witness publishing in the same Hive block they produced is accepted —
// state_engine.ProcessBlock calls RecordWitnessBlock before
// IngestTransactionOps, so by gate time the producer has count=1.
func TestFeedTracker_IngestGate_PublishAlongsideOwnProduction(t *testing.T) {
	tr := NewFeedTracker(false)
	// Mirror state_engine ordering: RecordWitnessBlock first, then op ingest.
	tr.RecordWitnessBlock("alice")
	tr.IngestTransactionOps(1, feedPublishOp("alice"))
	if _, ok := tr.quotes["alice"]; !ok {
		t.Fatal("first-block self-publish should be accepted")
	}
}

// TestFeedTracker_TickEvictsAgedOutFeeds verifies that a witness whose
// feed has aged out of the trust window is removed from the in-memory
// maps at the next tick. Caps map size at the actively-publishing set.
func TestFeedTracker_TickEvictsAgedOutFeeds(t *testing.T) {
	tr := NewFeedTracker(false)

	// Build up 100 blocks of "alice" producing, and have her publish at
	// block 1. At tick 100, alice's lastFeedBlk=1 satisfies 1+100>100 → trusted.
	for h := uint64(1); h <= 100; h++ {
		tr.RecordWitnessBlock("alice")
	}
	tr.IngestTransactionOps(1, feedPublishOp("alice"))
	tr.IngestTransactionOps(1, witnessSetPropsOp("alice", "1500"))

	tr.TickIfDue(100)
	// At tick 100: 1+100=101>100 is the trust check, eviction is at
	// 1+100<=100 → false. Entry kept.
	if _, ok := tr.quotes["alice"]; !ok {
		t.Fatal("alice's feed should still be in window at tick 100")
	}

	// Advance to tick 200 with continued production but no republish.
	for h := uint64(101); h <= 200; h++ {
		tr.RecordWitnessBlock("alice")
	}
	tr.TickIfDue(200)
	// At tick 200: lastFeedBlk[alice]=1, 1+100=101<=200 → evict.
	if _, ok := tr.quotes["alice"]; ok {
		t.Fatalf("alice's feed should have been evicted at tick 200; quotes=%v", tr.quotes)
	}
	if _, ok := tr.lastFeedBlk["alice"]; ok {
		t.Fatalf("lastFeedBlk should be evicted")
	}
	if _, ok := tr.witnessProps["alice"]; ok {
		t.Fatalf("witnessProps should be evicted")
	}
}

// TestFeedTracker_TickKeepsRecentFeed verifies the boundary: a feed
// published exactly at blockHeight - width + 1 is still inside the trust
// window and must not be evicted.
func TestFeedTracker_TickKeepsRecentFeed(t *testing.T) {
	tr := NewFeedTracker(false)
	for h := uint64(1); h <= 200; h++ {
		tr.RecordWitnessBlock("alice")
	}
	// Publish at block 101: 101+100=201>200 → trusted at tick 200.
	tr.IngestTransactionOps(101, feedPublishOp("alice"))
	tr.TickIfDue(200)
	if _, ok := tr.quotes["alice"]; !ok {
		t.Fatal("publish at block 101 should still be in window at tick 200")
	}
}

// TestFeedTracker_RepublishAfterEvictionReadmits confirms the eviction
// is non-permanent: once aged out, a witness can be re-added by publishing
// again, with the gate still enforcing they're an active producer.
func TestFeedTracker_RepublishAfterEvictionReadmits(t *testing.T) {
	tr := NewFeedTracker(false)
	for h := uint64(1); h <= 200; h++ {
		tr.RecordWitnessBlock("alice")
	}
	tr.IngestTransactionOps(1, feedPublishOp("alice"))
	tr.TickIfDue(200) // evicts (1+100 <= 200)
	if _, ok := tr.quotes["alice"]; ok {
		t.Fatal("evict precondition failed")
	}
	// Republish at 201 — alice still has 100 productions in window → gate passes.
	tr.IngestTransactionOps(201, feedPublishOp("alice"))
	if _, ok := tr.quotes["alice"]; !ok {
		t.Fatal("re-publish after eviction should be re-admitted")
	}
}

// TestFeedTrackerWarmedNaturalFill covers the recovery path when explicit
// Warmup wasn't called or failed — natural ProcessBlock ingest fills both
// the signature window and MA ring after enough live blocks.
func TestFeedTrackerWarmedNaturalFill(t *testing.T) {
	tr := NewFeedTracker(false)
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
	tr := NewFeedTracker(false)
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

func TestFeedTrackerTick_TestnetSymbols(t *testing.T) {
	tr := NewFeedTracker(false)
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("alice")
		tr.IngestTransactionOps(h, hive_blocks.Tx{
			Operations: []hivego.Operation{{
				Type: "feed_publish",
				Value: map[string]interface{}{
					"publisher": "alice",
					"exchange_rate": map[string]interface{}{
						"base":  "0.250 TBD",
						"quote": "1.000 TESTS",
					},
				},
			}},
		})
	}
	tr.TickIfDue(100)
	snap := tr.LastTick()
	if !snap.TrustedHiveOK {
		t.Fatal("TBD/TESTS feeds should produce a trusted hive price on non-mainnet")
	}
	if snap.TrustedHivePriceBps < 2_499 || snap.TrustedHivePriceBps > 2_501 {
		t.Fatalf("priceBps=%d want ~2500", snap.TrustedHivePriceBps)
	}
}

func TestFeedTrackerTick_TestnetSymbols_MainnetIgnored(t *testing.T) {
	tr := NewFeedTracker(true)
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("alice")
		tr.IngestTransactionOps(h, hive_blocks.Tx{
			Operations: []hivego.Operation{{
				Type: "feed_publish",
				Value: map[string]interface{}{
					"publisher": "alice",
					"exchange_rate": map[string]interface{}{
						"base":  "0.250 TBD",
						"quote": "1.000 TESTS",
					},
				},
			}},
		})
	}
	tr.TickIfDue(100)
	snap := tr.LastTick()
	if snap.TrustedHiveOK {
		t.Fatal("TBD/TESTS feeds should be silently dropped on mainnet")
	}
}
