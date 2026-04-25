package oracle

import (
	"testing"

	"vsc-node/modules/db/vsc/hive_blocks"

	"github.com/vsc-eco/hivego"
)

func TestParseHbdPerHivePair(t *testing.T) {
	p, ok := hiveHBDPerHiveFromFeed("0.250 HBD", "1.000000 HIVE")
	if !ok || p < 0.249 || p > 0.251 {
		t.Fatalf("p=%v ok=%v", p, ok)
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
	if snap.TrustedHiveMean < 0.24 || snap.TrustedHiveMean > 0.26 {
		t.Fatalf("mean=%v", snap.TrustedHiveMean)
	}
	if !snap.HBDInterestRateOK || snap.HBDInterestRateBps != 1500 {
		t.Fatalf("apr=%d ok=%v", snap.HBDInterestRateBps, snap.HBDInterestRateOK)
	}
	if len(snap.TrustedWitnessGroup) != 1 || snap.TrustedWitnessGroup[0] != "alice" {
		t.Fatalf("group=%v", snap.TrustedWitnessGroup)
	}
	if snap.WitnessSlashBps["alice"] != 0 {
		t.Fatalf("alice slash bps=%d want 0", snap.WitnessSlashBps["alice"])
	}
}

func TestFeedTrackerTick_WitnessSlashComputedFromWindowEvidence(t *testing.T) {
	tr := NewFeedTracker()
	for h := uint64(1); h <= 4; h++ {
		tr.RecordWitnessBlock("alice")
	}
	tr.RecordWitnessBlock("bob")
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
	snap := tr.LastTick()

	// alice: 4 sigs + updated feed => 0 bps
	if snap.WitnessSlashBps["alice"] != 0 {
		t.Fatalf("alice slash bps=%d want 0", snap.WitnessSlashBps["alice"])
	}
	// bob: deficit 3 (75 bps) + missing update (50 bps) => 125 bps
	if snap.WitnessSlashBps["bob"] != 125 {
		t.Fatalf("bob slash bps=%d want 125", snap.WitnessSlashBps["bob"])
	}
}

func TestFeedTrackerTick_StaleFeedBoundaryTriggersMissingUpdatePenalty(t *testing.T) {
	tr := NewFeedTracker()
	for h := uint64(1); h <= 200; h++ {
		tr.RecordWitnessBlock("alice")
		if h == 100 {
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
	}

	// lastFeedBlk+width > blockHeight is strict; 100+100 > 200 is false.
	tr.TickIfDue(200)
	snap := tr.LastTick()
	if snap.WitnessSlashBps["alice"] != 50 {
		t.Fatalf("alice slash bps=%d want 50", snap.WitnessSlashBps["alice"])
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
	s1.WitnessSlashBps["alice"] = 999

	s2 := tr.LastTick()
	if len(s2.TrustedWitnessGroup) != 1 || s2.TrustedWitnessGroup[0] != "alice" {
		t.Fatalf("unexpected group copy behavior: %v", s2.TrustedWitnessGroup)
	}
	if s2.WitnessSlashBps["alice"] != 0 {
		t.Fatalf("unexpected slash map copy behavior: %d", s2.WitnessSlashBps["alice"])
	}
}
