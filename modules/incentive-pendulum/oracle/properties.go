package oracle

import "sort"

const (
	// DefaultWitnessGroupSize is the default cap for the running trusted witness set.
	DefaultWitnessGroupSize = 20
	// MaxHBDInterestRateBps is a defensive upper bound (100.00% APR in basis points).
	MaxHBDInterestRateBps = 10000
)

// WitnessProperties stores the subset of witness props needed for pendulum APR logic.
// HBDInterestRateBps is the witness-advertised HBD APR in basis points.
type WitnessProperties struct {
	HBDInterestRateBps int
}

// RunningWitnessGroup returns the active trusted witness set, ranked by
// recent L1 block-production count (busier witnesses sort first; ties broken
// lexicographically by witness name). Only witnesses with trusted[w] = true
// are included; result is capped at maxWitnesses.
func RunningWitnessGroup(
	trusted map[string]bool,
	blocksInWindow map[string]int,
	maxWitnesses int,
) []string {
	if len(trusted) == 0 {
		return nil
	}
	if maxWitnesses < 1 {
		maxWitnesses = DefaultWitnessGroupSize
	}

	group := make([]string, 0, len(trusted))
	for w, ok := range trusted {
		if !ok || w == "" {
			continue
		}
		group = append(group, w)
	}
	if len(group) == 0 {
		return nil
	}

	sort.Slice(group, func(i, j int) bool {
		a, b := group[i], group[j]
		ba, bb := blocksInWindow[a], blocksInWindow[b]
		if ba != bb {
			return ba > bb
		}
		return a < b
	})

	if len(group) > maxWitnesses {
		group = group[:maxWitnesses]
	}
	return group
}

// HBDAPRModeFromGroup returns the mode APR (basis points) for the supplied witness group.
// Ties are resolved by choosing the lower APR for deterministic, conservative behavior.
func HBDAPRModeFromGroup(group []string, props map[string]WitnessProperties) (aprBps int, ok bool) {
	if len(group) == 0 || len(props) == 0 {
		return 0, false
	}

	counts := map[int]int{}
	bestRate := 0
	bestCount := 0

	for _, w := range group {
		p, has := props[w]
		if !has {
			continue
		}
		r := p.HBDInterestRateBps
		if r < 0 || r > MaxHBDInterestRateBps {
			continue
		}
		counts[r]++
		c := counts[r]
		if c > bestCount || (c == bestCount && r < bestRate) {
			bestRate = r
			bestCount = c
		}
	}

	if bestCount == 0 {
		return 0, false
	}
	return bestRate, true
}
