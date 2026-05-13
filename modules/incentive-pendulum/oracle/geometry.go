package oracle

import (
	"math/big"
	"strings"

	"vsc-node/lib/intmath"
)

// PoolReserveReader fetches the HBD-side reserve for a single pool at a
// specific block height. The production reader returns the pool contract's
// HBD ledger balance: pool HBD reserves live as the contract account's HBD
// balance, so the ledger snapshot is the source of truth — no per-pool
// state-key publication required.
//
// The only HBD a pool holds that is NOT liquidity is the network's
// claimable protocol-fee accumulation (kept inside the pool per the W3
// design); that is collected regularly via the pool's claim flow, so the
// difference between balance and live reserves stays small. Erring slightly
// high on P only inflates V, which biases the split slightly toward LPs —
// the safe direction.
//
// Returns (amount, true) when the contract holds a positive HBD balance,
// (0, false) otherwise. Implementations MUST be deterministic: two nodes
// calling the same (contractID, blockHeight) must receive identical results.
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
// HivePriceHBDBps is HBD per HIVE in basis points (BpsScale = 1.0).
type GeometryInputs struct {
	BlockHeight       uint64
	HivePriceHBDBps   int64
	HivePriceOK       bool
	WhitelistedPools  []string
	EffectiveStakeNum int64 // numerator of the effective-stake fraction (default 2)
	EffectiveStakeDen int64 // denominator (default 3)
}

// GeometryOutputs is the integer-form geometry block written to a snapshot.
// All HBD-denominated fields are base units (int64). SBps is the V/E ratio in
// basis points. OK == false means at least one input was unavailable; SDK
// callers gate on this to refuse swaps before geometry can be computed.
type GeometryOutputs struct {
	OK   bool
	V    int64 // 2 * P
	P    int64 // sum of HBD-side pool depths
	E    int64 // T * hivePriceHBD * effectiveFraction (HBD base units)
	T    int64 // sum of HIVE_CONSENSUS (HIVE base units)
	SBps int64 // V/E in basis points
}

// GeometryComputer wires the readers + the per-network effective fraction.
// Stateless — safe to construct once and reuse.
type GeometryComputer struct {
	pools PoolReserveReader
	bonds CommitteeBondReader
}

func NewGeometryComputer(pools PoolReserveReader, bonds CommitteeBondReader) *GeometryComputer {
	return &GeometryComputer{pools: pools, bonds: bonds}
}

// Compute produces the integer geometry block for one tick.
//
// Determinism: every input either originates on-chain (committee, balances,
// pool state at a pinned block height) or is itself a deterministic basis-
// point integer (hivePriceHBDBps from the trusted-witness aggregation).
// Computation uses int64 + big.Int arithmetic; no floats.
//
// Failure modes — all surface as OK == false:
//   - hive price unavailable (trusted-mean miss)
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
	if !in.HivePriceOK || in.HivePriceHBDBps <= 0 {
		return out
	}

	// T = sum of HIVE_CONSENSUS over current committee.
	_, totalHive := g.bonds.ReadCommitteeBond(in.BlockHeight)
	if totalHive <= 0 {
		return out
	}

	// E = T · hivePriceHBDBps · (num/den) / BpsScale, all in HBD base units.
	// Compute the bps-scaled value first, then apply num/den; saturate-to-zero
	// on overflow so the OK gate trips.
	eHBDBpsScaled := mulDivSafe(totalHive, in.HivePriceHBDBps, intmath.BpsScale)
	if eHBDBpsScaled <= 0 {
		return out
	}
	e := mulDivSafe(eHBDBpsScaled, num, den)
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

	// s = V / E in bps. If V == 0 we leave s == 0 (cliff); the SDK applier
	// detects V >= E to route everything to nodes.
	var sBps int64
	if v > 0 {
		sBps = mulDivSafe(v, intmath.BpsScale, e)
	}

	out.OK = true
	out.V = v
	out.P = p
	out.E = e
	out.T = totalHive
	out.SBps = sBps
	return out
}

// mulDivSafe is a saturating int64 mul-div used only by the geometry path,
// where overflow would set a corrupt snapshot field that nodes might
// disagree on. Returns 0 on overflow / divide-by-zero so the caller's OK
// gate trips.
func mulDivSafe(a, b, c int64) int64 {
	if c == 0 || a == 0 || b == 0 {
		return 0
	}
	prod := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	prod.Quo(prod, big.NewInt(c))
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

