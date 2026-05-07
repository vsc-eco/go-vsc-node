package pendulum

import (
	"testing"

	"vsc-node/modules/incentive-pendulum/oracle"
)

// TestBoltOracleIntegration exercises witness window → trusted price → MA → bolt Evaluate.
func TestBoltOracleIntegration(t *testing.T) {
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
	mxBps, ok := ring.Mean()
	if !ok {
		t.Fatal()
	}

	// Bolt Evaluate is the float reference path; convert bps→float at the
	// boundary just for this informational test.
	mx := float64(mxBps) / float64(BpsScale)
	b := NewPendulumBolt()
	ev, ok := b.Evaluate(NetworkSnapshot{
		TotalHiveStake: 20_000,
		HivePriceHBD:   mx,
		TotalBondT:     12_000,
		Pools: []PoolPendulumLiquidity{
			{PoolID: "main", Owner: "hive:vsc.dao", PHbd: 500},
		},
	}, 99_000)
	if !ok {
		t.Fatal()
	}
	if ev.Split.FinalNodeShare+ev.Split.FinalPoolShare != 99_000 {
		t.Fatal("split")
	}
	if ev.Collateral.UnderSecured {
		t.Fatal("unexpected cliff in fixture")
	}
}
