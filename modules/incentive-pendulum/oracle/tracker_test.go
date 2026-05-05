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
