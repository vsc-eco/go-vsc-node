package pendulum

import (
	"testing"

	"vsc-node/modules/common/params"
)

func TestEligiblePools_DAOOwnerOnly(t *testing.T) {
	pools := []PoolPendulumLiquidity{
		{PoolID: "vsc1A", Owner: params.DAO_WALLET, PHbd: 100},
		{PoolID: "vsc1B", Owner: "hive:bob", PHbd: 50},
	}
	got := EligiblePools(pools, params.DAO_WALLET, nil)
	if len(got) != 1 || got[0].PoolID != "vsc1A" {
		t.Fatalf("expected only vsc1A; got %+v", got)
	}
}

func TestEligiblePools_WhitelistOverridesOwnerCheck(t *testing.T) {
	pools := []PoolPendulumLiquidity{
		{PoolID: "vsc1A", Owner: params.DAO_WALLET, PHbd: 100},
		{PoolID: "vsc1B", Owner: "hive:bob", PHbd: 50},      // not DAO-owned but whitelisted
		{PoolID: "vsc1C", Owner: "hive:carol", PHbd: 25},    // neither
	}
	got := EligiblePools(pools, params.DAO_WALLET, []string{"vsc1B"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pools; got %d (%+v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, p := range got {
		seen[p.PoolID] = true
	}
	if !seen["vsc1A"] || !seen["vsc1B"] || seen["vsc1C"] {
		t.Fatalf("wrong selection: %+v", got)
	}
}

func TestEligiblePools_WhitelistDeduplicatesWithOwnerMatch(t *testing.T) {
	// A pool that is BOTH DAO-owned AND on the whitelist should appear once.
	pools := []PoolPendulumLiquidity{
		{PoolID: "vsc1A", Owner: params.DAO_WALLET, PHbd: 100},
	}
	got := EligiblePools(pools, params.DAO_WALLET, []string{"vsc1A"})
	if len(got) != 1 {
		t.Fatalf("expected 1 pool (no duplication); got %d", len(got))
	}
}

func TestEligiblePools_EmptyOwnerDisablesOwnerCheck(t *testing.T) {
	// daoOwner == "" → only whitelist matches. Useful for testnets that ship
	// without a DAO wallet pattern.
	pools := []PoolPendulumLiquidity{
		{PoolID: "vsc1A", Owner: params.DAO_WALLET, PHbd: 100},
		{PoolID: "vsc1B", Owner: "hive:bob", PHbd: 50},
	}
	got := EligiblePools(pools, "", []string{"vsc1B"})
	if len(got) != 1 || got[0].PoolID != "vsc1B" {
		t.Fatalf("expected only vsc1B; got %+v", got)
	}
}

func TestEligiblePools_WhitelistTrimsWhitespace(t *testing.T) {
	pools := []PoolPendulumLiquidity{
		{PoolID: "vsc1A", Owner: "hive:bob", PHbd: 100},
	}
	got := EligiblePools(pools, "", []string{"  vsc1A  "})
	if len(got) != 1 {
		t.Fatalf("expected vsc1A despite padding; got %+v", got)
	}
}

func TestPendulumBolt_WhitelistFlowsIntoEvaluate(t *testing.T) {
	b := NewPendulumBolt()
	b.PoolWhitelist = []string{"vsc1B"}

	net := NetworkSnapshot{
		TotalHiveStake: 1_000_000,
		HivePriceHBD:   0.30,
		TotalBondT:     200_000,
		Pools: []PoolPendulumLiquidity{
			// Not DAO-owned and not whitelisted: must be excluded.
			{PoolID: "vsc1A", Owner: "hive:carol", PHbd: 100},
			// Whitelisted (not DAO-owned): must be included.
			{PoolID: "vsc1B", Owner: "hive:bob", PHbd: 200},
		},
	}
	ev, _ := b.Evaluate(net, 1_000)
	// V = 2*PHbd_total, P = PHbd_total. With only vsc1B (PHbd=200): V=400, P=200.
	if ev.P != 200 || ev.V != 400 {
		t.Fatalf("whitelist not honored; got V=%v P=%v want V=400 P=200", ev.V, ev.P)
	}
}

func TestPendulumBolt_WhitelistEmptyFallsBackToOwnerCheck(t *testing.T) {
	b := NewPendulumBolt() // no whitelist; mainnet default
	net := NetworkSnapshot{
		TotalHiveStake: 1_000_000,
		HivePriceHBD:   0.30,
		TotalBondT:     200_000,
		Pools: []PoolPendulumLiquidity{
			{PoolID: "vsc1A", Owner: params.DAO_WALLET, PHbd: 100},
			{PoolID: "vsc1B", Owner: "hive:bob", PHbd: 200},
		},
	}
	ev, _ := b.Evaluate(net, 1_000)
	// Only vsc1A is eligible: V=200, P=100.
	if ev.P != 100 || ev.V != 200 {
		t.Fatalf("DAO fallback broken; got V=%v P=%v want V=200 P=100", ev.V, ev.P)
	}
}
