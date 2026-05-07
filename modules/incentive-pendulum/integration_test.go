package pendulum

import (
	"testing"

	"vsc-node/modules/incentive-pendulum/oracle"
)

// TestOracleIntegration exercises the integer-only path: witness window →
// trusted Quote → bps mean → moving-average ring. The float bolt-evaluate path
// has been retired alongside the legacy float pendulum APIs.
func TestOracleIntegration(t *testing.T) {
	win := oracle.NewWitnessSignatureWindow(10)
	for i := 0; i < 4; i++ {
		win.PushBlock([]string{"w1", "w2"})
	}
	if win.SignatureCount("w1") != 4 {
		t.Fatal()
	}

	// HBD/HIVE precision = 3; rational form is base-unit raw integers.
	quotes := map[string]oracle.Quote{
		"w1": {HbdRaw: 240, HiveRaw: 1000}, // 0.240 HBD per 1 HIVE
		"w2": {HbdRaw: 260, HiveRaw: 1000}, // 0.260 HBD per 1 HIVE
	}
	updated := map[string]bool{"w1": true, "w2": true}
	trusted := map[string]bool{}
	for w := range quotes {
		trusted[w] = oracle.FeedTrust(win.SignatureCount(w), updated[w], 4)
	}
	pxBps, ok := oracle.TrustedHivePriceBps(quotes, trusted)
	if !ok {
		t.Fatal()
	}
	// 0.250 HBD/HIVE = 2500 bps; with floor rounding 2499 or 2500 depending
	// on per-witness order.
	if pxBps < 2_499 || pxBps > 2_501 {
		t.Fatalf("pxBps %d", pxBps)
	}

	ring := oracle.NewMovingAverageRing(3)
	ring.Push(pxBps)
	ring.Push(pxBps + pxBps/100) // ~1% step in bps
	if _, ok := ring.Mean(); !ok {
		t.Fatal()
	}
}
