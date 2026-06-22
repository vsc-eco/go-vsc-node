package settlement

import (
	"math/big"
	"sort"
	"vsc-node/lib/intmath"
	"vsc-node/modules/incentive-pendulum"
	"vsc-node/modules/incentive-pendulum/rewards"
)

type SplitPreview struct {
	R int64
	T int64
	E int64
	V int64
	P int64

	FinalNodeShare int64
	FinalPoolShare int64
}

type Distribution struct {
	Account string
	Amount  int64
}

// RewardReductionApplied is one row of the per-epoch effective-bond
// computation. ReductionAmount is what gets removed from the bond in the
// distribution math; the underlying ledger HIVE_CONSENSUS balance is NOT
// debited (that would be true slashing — out of scope).
type RewardReductionApplied struct {
	Account         string
	Bps             int
	OriginalBond    int64
	ReductionAmount int64
	EffectiveBond   int64
}

func CalculateSplitPreviewFixed(r int64, t int64, effectiveNumerator int64, effectiveDenominator int64, v int64, p int64) SplitPreview {
	out := SplitPreview{
		R: r,
		T: t,
		V: v,
		P: p,
	}
	if r <= 0 || t <= 0 || effectiveNumerator <= 0 || effectiveDenominator <= 0 {
		return out
	}
	e := (t * effectiveNumerator) / effectiveDenominator
	if e <= 0 {
		return out
	}
	out.E = e

	split, ok := pendulum.SplitInt(pendulum.SplitInputsInt{
		R: big.NewInt(r),
		E: big.NewInt(e),
		T: big.NewInt(t),
		V: big.NewInt(v),
		P: big.NewInt(p),
	})
	if !ok {
		return out
	}

	node := split.FinalNodeShare.Int64()
	if node < 0 {
		node = 0
	}
	if node > r {
		node = r
	}
	out.FinalNodeShare = node
	out.FinalPoolShare = r - node
	return out
}

// ComputeNodeDistributions splits nodeShare across the committee pro-rata by
// effective bond, using integer floor division: each account receives
// floor(nodeShare * stake / total).
//
// The rounding remainder (nodeShare - sum of floors) is intentionally NOT
// assigned to any node. ComposeRecord captures it as ResidualHBD, which the
// apply path leaves sitting in the pendulum:nodes bucket so it rolls into the
// next epoch's distributable balance. This keeps an equal-stake committee
// exactly equal — no base-unit advantage to whoever sorts first / stakes
// most — while still conserving every base unit across epochs.
func ComputeNodeDistributions(nodeShare int64, bonds map[string]int64) []Distribution {
	if nodeShare <= 0 || len(bonds) == 0 {
		return nil
	}
	total := int64(0)
	for _, b := range bonds {
		if b > 0 {
			total += b
		}
	}
	if total <= 0 {
		return nil
	}

	accounts := make([]string, 0, len(bonds))
	for account := range bonds {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)

	out := make([]Distribution, 0, len(accounts))
	for _, account := range accounts {
		stake := bonds[account]
		if stake <= 0 {
			continue
		}
		// floor(nodeShare * stake / total) via big.Int to avoid int64 overflow
		// on the intermediate product. stake ≤ total so the result always fits
		// int64; the !ok branch is defensive and deterministic across nodes.
		amount, ok := intmath.MulDivFloorI64(nodeShare, stake, total)
		if !ok {
			continue
		}
		out = append(out, Distribution{
			Account: account,
			Amount:  amount,
		})
	}

	return out
}

// ExpandShareDistributions rewrites node-level distributions into per-recipient
// distributions for nodes that share their pendulum rewards with delegators
// (delegationmode.Share). Consensus 0.3.0+.
//
// For a node present in shareDelegations, its HBD amount is split across the
// node's stake edges pro-rata by stake using integer floor division
// (intmath.MulDivFloorI64). The rounding remainder (amount − Σ floors) is
// credited to the node operator account, so each node's TOTAL payout is
// preserved EXACTLY — TotalDistributedHBD and ResidualHBD are therefore
// identical to the node-level result, and the settlement's conservation
// invariant is untouched. Nodes absent from shareDelegations (Custom,
// Deactivated, or non-committee) pass through unchanged: the operator keeps the
// whole amount.
//
// The operator's own self-stake is just another edge (node -> node), so the
// operator earns strictly in proportion to their own stake — no separate
// commission. Results are merged by recipient account (a delegator across
// several share nodes, or an operator who also delegates elsewhere, collapses
// to a single entry — which both avoids duplicate ledger ids at apply time and
// keeps the post-sort order byte-stable) and sorted lexicographically.
//
// Deterministic: edge maps are iterated in sorted account order and all
// arithmetic is integer, so two honest nodes produce a byte-identical result.
func ExpandShareDistributions(dists []Distribution, shareDelegations map[string]map[string]int64) []Distribution {
	merged := make(map[string]int64)
	add := func(acct string, amt int64) {
		if amt != 0 {
			merged[acct] += amt
		}
	}

	for _, d := range dists {
		edges := shareDelegations[d.Account]
		if d.Amount <= 0 || len(edges) == 0 {
			// Non-share node (or nothing to split): operator keeps it.
			add(d.Account, d.Amount)
			continue
		}

		// Denominator = Σ positive edge stakes; collect delegators for a
		// deterministic (sorted) split order.
		total := int64(0)
		delegators := make([]string, 0, len(edges))
		for dlg, st := range edges {
			if st > 0 {
				total += st
				delegators = append(delegators, dlg)
			}
		}
		if total <= 0 {
			add(d.Account, d.Amount)
			continue
		}
		sort.Strings(delegators)

		distributed := int64(0)
		for _, dlg := range delegators {
			share, ok := intmath.MulDivFloorI64(d.Amount, edges[dlg], total)
			if !ok || share <= 0 {
				continue
			}
			add(dlg, share)
			distributed += share
		}
		// Floor remainder → operator (the node account), preserving the node's
		// exact total. distributed ≤ d.Amount, so the remainder is ≥ 0.
		add(d.Account, d.Amount-distributed)
	}

	accounts := make([]string, 0, len(merged))
	for a := range merged {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)

	out := make([]Distribution, 0, len(accounts))
	for _, a := range accounts {
		if merged[a] <= 0 {
			continue
		}
		out = append(out, Distribution{Account: a, Amount: merged[a]})
	}
	return out
}

// ApplyRewardReductionsToBonds returns a new bond map with each account's
// effective bond reduced by reductionBps[acct]/10000 (capped at
// rewards.PerEpochCapBps = 100%). The original ledger HIVE_CONSENSUS bonds
// are NOT touched — this only computes the per-epoch effective bond used by
// the pro-rata distribution math.
//
// Returns (effectiveBonds, applied) where applied lists every account that
// had a non-zero reduction, sorted lexicographically for deterministic
// payload construction.
func ApplyRewardReductionsToBonds(bonds map[string]int64, reductionBps map[string]int) (map[string]int64, []RewardReductionApplied) {
	out := make(map[string]int64, len(bonds))
	for k, v := range bonds {
		out[k] = v
	}

	accounts := make([]string, 0, len(reductionBps))
	for acc := range reductionBps {
		accounts = append(accounts, acc)
	}
	sort.Strings(accounts)

	applied := make([]RewardReductionApplied, 0, len(accounts))
	for _, acc := range accounts {
		// The bond map (out) is keyed "hive:<account>" (see ReadCommitteeBonds /
		// normalizeHiveAccount), but reductionBps is keyed by the bare account
		// name (committee members are bare m.Account). Without normalizing here
		// every lookup missed, orig was always 0, and the entire reward-reduction
		// (penalty) system was silently inert. Normalize to the bond namespace.
		key := normalizeHiveAccount(acc)
		orig := out[key]
		if orig <= 0 {
			continue
		}
		bps := reductionBps[acc]
		if bps <= 0 {
			continue
		}
		if bps > rewards.PerEpochCapBps {
			bps = rewards.PerEpochCapBps
		}
		// floor(orig * bps / 10000) via big.Int to avoid int64 overflow on the
		// intermediate product for large bonds. reduction ≤ orig so it always
		// fits int64; !ok is defensive (treat as no reduction this epoch).
		reduction, ok := intmath.MulDivFloorI64(orig, int64(bps), intmath.BpsScale)
		if !ok {
			reduction = 0
		}
		eff := orig - reduction
		if eff < 0 {
			eff = 0
		}
		out[key] = eff
		applied = append(applied, RewardReductionApplied{
			Account:         acc,
			Bps:             bps,
			OriginalBond:    orig,
			ReductionAmount: reduction,
			EffectiveBond:   eff,
		})
	}
	return out, applied
}
