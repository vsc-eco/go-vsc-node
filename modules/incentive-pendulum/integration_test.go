package pendulum

import (
	"testing"

	"vsc-node/lib/intmath"
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
	pxSQ, ok := oracle.TrustedHivePriceSQ64(quotes, trusted)
	if !ok {
		t.Fatal()
	}
	px := pxSQ.ToFloat()
	if px < 0.249 || px > 0.251 {
		t.Fatalf("px %v", px)
	}

	ring := oracle.NewMovingAverageRing(3)
	ring.Push(px)
	ring.Push(intmath.SQ64(float64(pxSQ) * 1.01).ToFloat())
	mx, ok := ring.Mean()
	if !ok {
		t.Fatal()
	}

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
