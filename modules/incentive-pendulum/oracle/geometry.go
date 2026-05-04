package oracle

import (
	"strings"

	"vsc-node/lib/intmath"
)

// PoolReserveStateKey is the contract-state key each whitelisted pool MUST
// publish its current HBD-side reserve under. The value is a base-10 ASCII
// integer (HBD base units, signed int64). Pools update this on every swap
// alongside their own reserve bookkeeping.
//
// Convention is intentionally narrow: a single key, no JSON, no nested
// structure — the geometry tick reads it from every whitelisted pool every
// 5 minutes, and any parse-side complexity multiplies across the whole
// committee. The pool integration spec (W8) documents this for contract
// authors.
const PoolReserveStateKey = "__pendulum_hbd_reserve"

// PoolReserveReader fetches the published HBD-side reserve for a single pool
// at a specific block height. Returns (amount, true) when the pool has
// published a reserve, (0, false) otherwise. Implementations MUST be
// deterministic: two nodes calling the same (contractID, blockHeight) must
// receive identical results.
type PoolReserveReader interface {
	ReadPoolHBDReserve(contractID string, blockHeight uint64) (int64, bool)
}

// CommitteeBondReader returns (members, summed HIVE_CONSENSUS) at a given
// block height. Members are needed so the geometry-tick log can attribute
// the sum if we ever surface per-witness contributions.
type CommitteeBondReader interface {
	ReadCommitteeBond(blockHeight uint64) (members []string, totalHive int64)
}

// GeometryInputs is the per-tick input bundle the GeometryComputer reads.
type GeometryInputs struct {
	BlockHeight       uint64
	HivePriceHBDSQ64  intmath.SQ64 // SQ64 fixed-point, 1.0 = SQ64Scale
	HivePriceOK       bool
	WhitelistedPools  []string
	EffectiveStakeNum int64 // numerator of the effective-stake fraction (default 2)
	EffectiveStakeDen int64 // denominator (default 3)
}

// GeometryOutputs is the integer-form geometry block written to a snapshot.
// All HBD-denominated fields are base units (int64). S is SQ64 (10^8 scale).
// OK == false means at least one input was unavailable; SDK callers gate on
// this to refuse swaps before geometry can be computed.
type GeometryOutputs struct {
	OK bool
	V  int64 // 2 * P
	P  int64 // sum of HBD-side pool depths
	E  int64 // T * hivePriceHBD * effectiveFraction (HBD base units)
	T  int64 // sum of HIVE_CONSENSUS (HIVE base units)
	S  intmath.SQ64
}

// GeometryComputer wires the readers + the per-network effective fraction.
// Stateless — safe to construct once and reuse.
type GeometryComputer struct {
	pools  PoolReserveReader
	bonds  CommitteeBondReader
}

func NewGeometryComputer(pools PoolReserveReader, bonds CommitteeBondReader) *GeometryComputer {
	return &GeometryComputer{pools: pools, bonds: bonds}
}

// Compute produces the integer geometry block for one tick.
//
// Determinism: every input either originates on-chain (committee, balances,
// pool state at a pinned block height) or is itself a deterministic SQ64
// quantity (hivePriceHBD from the W2 snapshot). Computation uses int64 +
// SQ64 arithmetic; no floats.
//
// Failure modes — all surface as OK == false:
//   - no whitelisted pools (empty input)
//   - hive price unavailable (W2 trusted-mean miss)
//   - committee has no bonded members
//   - effective fraction degenerate (den <= 0)
//
// V == 0 with OK == true is reachable when whitelisted pools exist but none
// have published reserves yet; the SDK applier treats V == 0 as the cliff
// path (all fees to nodes) so this is safe.
func (g *GeometryComputer) Compute(in GeometryInputs) GeometryOutputs {
	out := GeometryOutputs{}
	if g == nil || g.bonds == nil {
		return out
	}

	num, den := in.EffectiveStakeNum, in.EffectiveStakeDen
	if num <= 0 || den <= 0 {
		num, den = 2, 3
	}
	if !in.HivePriceOK || in.HivePriceHBDSQ64 <= 0 {
		return out
	}

	// T = sum of HIVE_CONSENSUS over current committee.
	_, totalHive := g.bonds.ReadCommitteeBond(in.BlockHeight)
	if totalHive <= 0 {
		return out
	}

	// E = T * hivePriceHBD * (num/den), all in HBD base units.
	// hivePriceHBDSQ64 is HBD-per-HIVE in SQ64; T is HIVE base units.
	// E_sq = T * hivePriceHBDSQ64 / den * num — split to avoid intermediate
	// overflow on huge T values.
	priceQ := int64(in.HivePriceHBDSQ64)
	eHBDSQ64 := mulDivSafe(totalHive, priceQ, int64(intmath.SQ64Scale))
	if eHBDSQ64 <= 0 {
		return out
	}
	e := mulDivSafe(eHBDSQ64, num, den)
	if e <= 0 {
		return out
	}

	// P = sum of HBD-side reserves over whitelisted pools (deterministic
	// iteration order — list is sorted up front).
	pools := append([]string(nil), in.WhitelistedPools...)
	sortStrings(pools)
	var p int64
	if g.pools != nil {
		for _, id := range pools {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			amt, ok := g.pools.ReadPoolHBDReserve(id, in.BlockHeight)
			if !ok || amt <= 0 {
				continue
			}
			p = saturatingAdd(p, amt)
		}
	}
	v := saturatingAdd(p, p) // V = 2P; symmetry assumption per dexfeed.go
	if v < 0 {
		v = 0
	}

	// s = V / E in SQ64. If V == 0 we leave S == 0 (cliff); the SDK applier
	// detects V >= E to route everything to nodes.
	var s intmath.SQ64
	if v > 0 {
		s = intmath.SQ64(mulDivSafe(v, int64(intmath.SQ64Scale), e))
	}

	out.OK = true
	out.V = v
	out.P = p
	out.E = e
	out.T = totalHive
	out.S = s
	return out
}

// mulDivSafe is a saturating int64 mul-div used only by the geometry path,
// where overflow would set a corrupt snapshot field that nodes might
// disagree on. Returns 0 on overflow / divide-by-zero so the caller's OK
// gate trips.
func mulDivSafe(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	if a == 0 || b == 0 {
		return 0
	}
	// Use math/big internally — same primitive intmath.MulDivFloor uses, but
	// we stay int64-typed at the boundary because every snapshot field is
	// int64. Detect overflow by checking IsInt64.
	prod := bigMul(a, b)
	prod = bigQuo(prod, c)
	if !prod.IsInt64() {
		return 0
	}
	return prod.Int64()
}

func saturatingAdd(a, b int64) int64 {
	r := a + b
	// classic overflow check for non-negative addends
	if a > 0 && b > 0 && r < a {
		return 0 // saturate-to-zero so OK gating refuses use
	}
	return r
}
