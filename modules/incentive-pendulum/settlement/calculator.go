package settlement

import (
	"math/big"
	"sort"
	"strings"
	"vsc-node/modules/incentive-pendulum"
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

type SlashApplied struct {
	Account       string
	Bps           int
	OriginalBond  int64
	SlashedAmount int64
	PostSlashBond int64
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
	assigned := int64(0)
	maxAccount := ""
	maxStake := int64(0)

	for _, account := range accounts {
		stake := bonds[account]
		if stake <= 0 {
			continue
		}
		amount := (nodeShare * stake) / total
		assigned += amount
		out = append(out, Distribution{
			Account: account,
			Amount:  amount,
		})

		if stake > maxStake || (stake == maxStake && strings.Compare(account, maxAccount) < 0) {
			maxStake = stake
			maxAccount = account
		}
	}

	residual := nodeShare - assigned
	if residual > 0 {
		for i := range out {
			if out[i].Account == maxAccount {
				out[i].Amount += residual
				break
			}
		}
	}

	return out
}

func ApplySlashesToBonds(bonds map[string]int64, slashBps map[string]int) (map[string]int64, []SlashApplied) {
	out := make(map[string]int64, len(bonds))
	for k, v := range bonds {
		out[k] = v
	}

	accounts := make([]string, 0, len(slashBps))
	for acc := range slashBps {
		accounts = append(accounts, acc)
	}
	sort.Strings(accounts)

	applied := make([]SlashApplied, 0, len(accounts))
	for _, acc := range accounts {
		orig := out[acc]
		if orig <= 0 {
			continue
		}
		bps := slashBps[acc]
		if bps <= 0 {
			continue
		}
		if bps > 1000 {
			bps = 1000
		}
		slashed := (orig * int64(bps)) / 10000
		post := orig - slashed
		if post < 0 {
			post = 0
		}
		out[acc] = post
		applied = append(applied, SlashApplied{
			Account:       acc,
			Bps:           bps,
			OriginalBond:  orig,
			SlashedAmount: slashed,
			PostSlashBond: post,
		})
	}
	return out, applied
}
