package gateway

import (
	"strconv"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/vsc-eco/hivego"
)

// ── quantizeStakeWeights: pure-function property tests ──────────────────────

func sumWeights(w []int) int {
	t := 0
	for _, x := range w {
		t += x
	}
	return t
}

// stake_i > stake_j ⇒ weight_i >= weight_j (non-strict: equal/near stakes may
// tie within ±1 from the largest-remainder top-up, but a strictly larger stake
// never receives a strictly smaller weight).
func assertMonotone(t *testing.T, stakes []uint64, weights []int) {
	t.Helper()
	for i := range stakes {
		for j := range stakes {
			if stakes[i] > stakes[j] && weights[i] < weights[j] {
				t.Fatalf("monotonicity violated: stake[%d]=%d > stake[%d]=%d but weight %d < %d",
					i, stakes[i], j, stakes[j], weights[i], weights[j])
			}
		}
	}
}

func assertBounds(t *testing.T, weights []int) {
	t.Helper()
	for i, w := range weights {
		if w < 1 || w > uint16Max {
			t.Fatalf("weight[%d]=%d out of uint16 signing range [1,%d]", i, w, uint16Max)
		}
	}
}

func TestQuantizeStakeWeights_Proportional(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	// Skewed stakes (satoshi-scale, well above uint16) including one large node.
	stakes := []uint64{900_000_000, 120_000_000, 60_000_000, 40_000_000,
		25_000_000, 10_000_000, 4_000_000, 2_000_000}

	w := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)

	assertBounds(t, w)
	assertMonotone(t, stakes, w)

	// Σ weights ∈ [scale, scale+n]: the largest-remainder pass makes the base
	// sum exactly `scale`, and the min-1 floor adds at most one per node.
	sum := sumWeights(w)
	if sum < GATEWAY_WEIGHT_SCALE || sum > GATEWAY_WEIGHT_SCALE+len(stakes) {
		t.Fatalf("Σweights=%d outside [%d,%d]", sum, GATEWAY_WEIGHT_SCALE, GATEWAY_WEIGHT_SCALE+len(stakes))
	}

	// Roughly proportional: the 900M node is 900/1161 ≈ 77.5% of total stake, so
	// its weight should land within ±2 of floor(stake·scale/total).
	var total uint64
	for _, s := range stakes {
		total += s
	}
	want := int(stakes[0] * GATEWAY_WEIGHT_SCALE / total)
	if w[0] < want-2 || w[0] > want+2 {
		t.Fatalf("largest staker weight=%d not ~proportional (want ≈%d)", w[0], want)
	}
}

func TestQuantizeStakeWeights_EqualStakes(t *testing.T) {
	// Initial-election shape: every node has the same fixed weight.
	const n = 9
	accounts := make([]string, n)
	stakes := make([]uint64, n)
	for i := range accounts {
		accounts[i] = "n" + strconv.Itoa(i)
		stakes[i] = 10
	}
	w := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)
	assertBounds(t, w)
	min, max := w[0], w[0]
	for _, x := range w {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
	}
	if max-min > 1 {
		t.Fatalf("equal stakes must yield near-equal weights, got spread min=%d max=%d", min, max)
	}
}

// Documents the ACCEPTED no-cap tradeoff: a node holding >= 2/3 of stake gets a
// weight that alone clears the 2/3 threshold (it controls the gateway).
func TestQuantizeStakeWeights_DominantNodeExceedsThreshold(t *testing.T) {
	accounts := []string{"whale", "a", "b", "c", "d", "e", "f", "g"}
	stakes := []uint64{70, 6, 6, 5, 5, 4, 2, 2} // whale = 70/100
	w := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)

	threshold := gatewayWeightThreshold(sumWeights(w))
	if w[0] < threshold {
		t.Fatalf("dominant node weight=%d should exceed threshold=%d (no-cap policy)", w[0], threshold)
	}
}

// Determinism: input order must not change any node's assigned weight (every
// honest node may iterate elected members in a different order yet must build
// the byte-identical authority). The account tiebreak guarantees this.
func TestQuantizeStakeWeights_DeterministicUnderReorder(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g"}
	stakes := []uint64{50, 30, 30, 30, 10, 7, 3} // ties at 30 exercise the tiebreak

	w := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)
	byAccount := map[string]int{}
	for i, a := range accounts {
		byAccount[a] = w[i]
	}

	// Reverse the input order and recompute; per-account weights must match.
	n := len(accounts)
	ra := make([]string, n)
	rs := make([]uint64, n)
	for i := range accounts {
		ra[i] = accounts[n-1-i]
		rs[i] = stakes[n-1-i]
	}
	w2 := quantizeStakeWeights(rs, ra, GATEWAY_WEIGHT_SCALE)
	for i, a := range ra {
		if w2[i] != byAccount[a] {
			t.Fatalf("non-deterministic: account %s got weight %d in forward order, %d reversed",
				a, byAccount[a], w2[i])
		}
	}
}

func TestQuantizeStakeWeights_ZeroTotalFallsBackToFlat(t *testing.T) {
	accounts := []string{"a", "b", "c"}
	stakes := []uint64{0, 0, 0}
	w := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)
	for i, x := range w {
		if x != 1 {
			t.Fatalf("zero-total fallback should give every node weight 1, got w[%d]=%d", i, x)
		}
	}
}

// ── weightMeetsThreshold: broadcast-gate regression (== → >=) ───────────────

func TestWeightMeetsThreshold(t *testing.T) {
	cases := []struct {
		collected uint64
		threshold int
		want      bool
	}{
		{6667, 6667, true}, // exact crossing
		{7000, 6667, true}, // OVERSHOOT — the case an == gate wrongly rejected
		{6666, 6667, false},
		{0, 6667, false},
		{100, 0, false}, // defensive threshold<=0 guard (callers abort earlier)
		{0, 0, false},
	}
	for _, c := range cases {
		if got := weightMeetsThreshold(c.collected, c.threshold); got != c.want {
			t.Errorf("weightMeetsThreshold(%d, %d) = %v, want %v", c.collected, c.threshold, got, c.want)
		}
	}
}

// ── keyRotation: integration through the real authority construction ────────

// runKeyRotation drives keyRotation for the given accounts/stakes and returns
// the owner authority captured from UpdateAccount.
func runKeyRotation(t *testing.T, accounts []string, stakes []uint64) (*hivego.Auths, map[string]string, error) {
	t.Helper()
	const bh = uint64(20) // bh % ACTION_INTERVAL == 0

	witnessDb := &test_utils.MockWitnessDb{ByAccount: map[string]*witnesses.Witness{}}
	members := make([]elections.ElectionMember, len(accounts))
	gatewayKeys := make(map[string]string, len(accounts))
	for i, acc := range accounts {
		gk := fixtureGatewayKey(acc)
		gatewayKeys[acc] = gk
		witnessDb.ByAccount[acc] = &witnesses.Witness{Account: acc, GatewayKey: gk}
		members[i] = elections.ElectionMember{Account: acc, Key: acc}
	}
	electionDb := &test_utils.MockElectionDb{
		ElectionsByHeight: map[uint64]elections.ElectionResult{
			bh: {ElectionDataInfo: elections.ElectionDataInfo{Members: members, Weights: stakes}},
		},
	}
	fake := &fakeHiveCreator{}
	ms := &MultiSig{
		sconf:       systemconfig.MocknetConfig(),
		electionDb:  electionDb,
		witnessDb:   witnessDb,
		hiveCreator: fake,
	}
	_, err := ms.keyRotation(bh)
	return fake.owner, gatewayKeys, err
}

func weightOfKey(owner *hivego.Auths, key string) (int, bool) {
	for _, ka := range owner.KeyAuths {
		if ka[0].(string) == key {
			return ka[1].(int), true
		}
	}
	return 0, false
}

func TestKeyRotation_WeightsStakeProportionalAndThreshold(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	stakes := []uint64{900_000_000, 120_000_000, 60_000_000, 40_000_000,
		25_000_000, 10_000_000, 4_000_000, 2_000_000}

	owner, gwKeys, err := runKeyRotation(t, accounts, stakes)
	if err != nil {
		t.Fatalf("keyRotation error: %v", err)
	}
	if owner == nil {
		t.Fatal("UpdateAccount was not called")
	}

	// Every weight is a Go int in [1,65535] (hivego serializes via key[1].(int)).
	total := 0
	for _, ka := range owner.KeyAuths {
		w, ok := ka[1].(int)
		if !ok {
			t.Fatalf("key weight is %T, must be int for the hivego serializer", ka[1])
		}
		if w < 1 || w > uint16Max {
			t.Fatalf("weight %d out of [1,%d]", w, uint16Max)
		}
		total += w
	}

	// Monotonic in stake: larger stake ⇒ weight not smaller.
	for i := range accounts {
		for j := range accounts {
			wi, _ := weightOfKey(owner, gwKeys[accounts[i]])
			wj, _ := weightOfKey(owner, gwKeys[accounts[j]])
			if stakes[i] > stakes[j] && wi < wj {
				t.Fatalf("%s(stake %d) weight %d < %s(stake %d) weight %d",
					accounts[i], stakes[i], wi, accounts[j], stakes[j], wj)
			}
		}
	}

	// Threshold is ceil(2/3 · Σ assigned weights), not a count.
	if owner.WeightThreshold != gatewayWeightThreshold(total) {
		t.Fatalf("threshold=%d, want gatewayWeightThreshold(Σ=%d)=%d",
			owner.WeightThreshold, total, gatewayWeightThreshold(total))
	}
	if owner.WeightThreshold <= 0 || owner.WeightThreshold*3 < total*2 {
		t.Fatalf("threshold=%d is not a positive 2/3 supermajority of %d", owner.WeightThreshold, total)
	}
}

func TestKeyRotation_DominantNodeControls(t *testing.T) {
	// One node owns ~80% of stake → its weight alone clears the threshold.
	accounts := []string{"whale", "a", "b", "c", "d", "e", "f", "g"}
	stakes := []uint64{80_000_000, 4_000_000, 4_000_000, 3_000_000,
		3_000_000, 2_500_000, 2_000_000, 1_500_000}

	owner, gwKeys, err := runKeyRotation(t, accounts, stakes)
	if err != nil {
		t.Fatalf("keyRotation error: %v", err)
	}
	whaleWeight, ok := weightOfKey(owner, gwKeys["whale"])
	if !ok {
		t.Fatal("whale missing from authority")
	}
	if whaleWeight < owner.WeightThreshold {
		t.Fatalf("dominant node weight=%d should meet threshold=%d (no-cap policy)",
			whaleWeight, owner.WeightThreshold)
	}
}

func TestKeyRotation_Deterministic(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	stakes := []uint64{50, 30, 30, 30, 20, 15, 10, 8, 8} // ties exercise tiebreak

	o1, keys, err := runKeyRotation(t, accounts, stakes)
	if err != nil {
		t.Fatalf("keyRotation error: %v", err)
	}
	o2, _, err := runKeyRotation(t, accounts, stakes)
	if err != nil {
		t.Fatalf("keyRotation error: %v", err)
	}
	if o1.WeightThreshold != o2.WeightThreshold {
		t.Fatalf("threshold differs across runs: %d vs %d", o1.WeightThreshold, o2.WeightThreshold)
	}
	for _, acc := range accounts {
		w1, _ := weightOfKey(o1, keys[acc])
		w2, _ := weightOfKey(o2, keys[acc])
		if w1 != w2 {
			t.Fatalf("weight for %s differs across runs: %d vs %d", acc, w1, w2)
		}
	}
}

func TestKeyRotation_BelowMinKeysErrors(t *testing.T) {
	accounts := []string{"a", "b", "c", "d", "e", "f", "g"} // 7 < MIN_GATEWAY_KEYS
	stakes := []uint64{7, 6, 5, 4, 3, 2, 1}
	_, _, err := runKeyRotation(t, accounts, stakes)
	if err == nil {
		t.Fatal("expected 'not enough keys' error with fewer than MIN_GATEWAY_KEYS signers")
	}
}
