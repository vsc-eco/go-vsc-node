package gateway

import (
	"cmp"
	"math/big"
	"slices"
)

// Gateway multisig sizing/weighting constants.
//
// MAX_GATEWAY_KEYS bounds the number of key_auths in the gateway account
// authority (Hive caps authorities at 40 entries). MIN_GATEWAY_KEYS is the
// floor below which a rotation refuses to proceed. GATEWAY_WEIGHT_SCALE is the
// integer budget that on-chain stake is apportioned across as per-key signing
// weights (see quantizeStakeWeights): ~0.01% granularity, and small enough that
// any single key's weight stays well under the uint16 ceiling Hive serializes
// auth weights as.
const (
	MAX_GATEWAY_KEYS     = 40
	MIN_GATEWAY_KEYS     = 8
	GATEWAY_WEIGHT_SCALE = 10000
)

// uint16Max is the largest weight Hive's binary format can carry for a single
// key_auth (the serializer writes each weight as a uint16).
const uint16Max = 65535

// gatewayWeightThreshold returns the owner-auth weight threshold for the
// gateway multisig account: a strict 2/3 supermajority of totalWeight, where
// totalWeight is the SUM OF THE ASSIGNED PER-KEY WEIGHTS (not the key count).
//
// review2 HIGH #29: this was previously int(totalWeight * 2 / 3), i.e.
// floor(2N/3). For 10 keys that yields 6, letting 6-of-10 signers move
// funds when a 2/3 supermajority should require 7. ceil(2N/3) is the correct
// threshold; ceil(a/b) == (a + b - 1) / b, so ceil(2N/3) == (2N + 2) / 3.
func gatewayWeightThreshold(totalWeight int) int {
	if totalWeight <= 0 {
		return 0
	}
	return (totalWeight*2 + 2) / 3
}

// weightMeetsThreshold reports whether the collected signature weight satisfies
// the gateway multisig threshold and is safe to broadcast.
//
// It is >= (not ==): waitForSigs stops collecting at the FIRST signature whose
// cumulative weight reaches the threshold, and with stake-proportional (i.e.
// variable) per-key weights that crossing signature can push the total strictly
// PAST the threshold. An == gate would then reject a fully-signed transaction
// and never broadcast it. The threshold > 0 guard is defensive — callers
// already abort when getThreshold returns <= 0.
func weightMeetsThreshold(collected uint64, threshold int) bool {
	return threshold > 0 && collected >= uint64(threshold)
}

// quantizeStakeWeights apportions `scale` integer weight units across the nodes
// in proportion to their on-chain stake, returning one Go int weight per node
// (aligned with the input order). The result is what each gateway key gets in
// the Hive multisig authority, so control tracks economic stake rather than
// node count.
//
// Determinism is mandatory: every node independently rebuilds the identical
// account_update transaction (same per-key weights, same threshold) so their
// collected signatures aggregate. All math is integer/big.Int (no float64), and
// the leftover-unit distribution tiebreaks on the globally-unique account name,
// so two honest nodes always produce byte-identical weights.
//
// Properties:
//   - every included node gets weight >= 1 (so each elected signer can sign);
//   - weights are monotonic in stake and sum to ~scale;
//   - no cap — a node holding >=2/3 of stake gets >=2/3 of weight (accepted
//     tradeoff: stake-majority controls the gateway).
func quantizeStakeWeights(stakes []uint64, accounts []string, scale uint64) []int {
	n := len(stakes)
	weights := make([]int, n)
	if n == 0 {
		return weights
	}

	// Total stake (big.Int so summing many uint64 stakes can never overflow).
	total := new(big.Int)
	for _, s := range stakes {
		total.Add(total, new(big.Int).SetUint64(s))
	}

	// Degenerate: no stake recorded (shouldn't happen post-MinStake). Fall back
	// to flat weighting so the rotation still yields a valid multisig.
	if total.Sign() == 0 {
		for i := range weights {
			weights[i] = 1
		}
		return weights
	}

	scaleBig := new(big.Int).SetUint64(scale)

	// Hamilton's largest-remainder apportionment: base_i = floor(stake_i*scale/
	// total); the leftover units (scale - Σ base_i) go to the largest remainders.
	type part struct {
		idx     int
		base    uint64
		rem     *big.Int
		account string
	}
	parts := make([]part, n)
	assigned := uint64(0)
	for i, s := range stakes {
		prod := new(big.Int).Mul(new(big.Int).SetUint64(s), scaleBig)
		base, rem := new(big.Int), new(big.Int)
		base.DivMod(prod, total, rem) // floor + remainder (operands non-negative)
		b := base.Uint64()
		parts[i] = part{idx: i, base: b, rem: rem, account: accounts[i]}
		assigned += b
	}

	// Distribute the deficit one unit at a time to the largest remainders.
	// Tiebreak on the unique account name so the distribution is identical on
	// every node. Σ of fractional parts == scale - assigned < n, so deficit < n.
	deficit := uint64(0)
	if scale > assigned {
		deficit = scale - assigned
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		if c := parts[b].rem.Cmp(parts[a].rem); c != 0 {
			return c // larger remainder first
		}
		return cmp.Compare(parts[a].account, parts[b].account)
	})
	for k := uint64(0); k < deficit && k < uint64(n); k++ {
		parts[order[k]].base++
	}

	// Floor every node to weight 1 (so each elected signer retains a share) and
	// clamp to the uint16 ceiling Hive serializes weights as (defensive: with
	// scale <= uint16Max, base_i <= scale already holds).
	for i := range parts {
		w := parts[i].base
		if w < 1 {
			w = 1
		}
		if w > uint16Max {
			w = uint16Max
		}
		weights[parts[i].idx] = int(w)
	}
	return weights
}
