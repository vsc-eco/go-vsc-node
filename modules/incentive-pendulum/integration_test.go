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

	quotes := map[string]float64{"w1": 0.24, "w2": 0.26}
	updated := map[string]bool{"w1": true, "w2": true}
	trusted := map[string]bool{}
	for w := range quotes {
		trusted[w] = oracle.FeedTrust(win.SignatureCount(w), updated[w], 4)
	}
	px, ok := oracle.TrustedHivePrice(quotes, trusted)
	if !ok {
		t.Fatal()
	}
	if px < 0.249 || px > 0.251 {
		t.Fatalf("px %v", px)
	}

	ring := oracle.NewMovingAverageRing(3)
	ring.Push(px)
	ring.Push(px * 1.01)
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
			{"main", 500},
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
