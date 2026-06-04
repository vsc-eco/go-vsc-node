package chain

// review7 VD-M7 — a degraded oracle start (chain RPC unreachable) must be
// surfaced distinctly. ChainOracle.Start records each configured chain's
// startup probe result as a height string (reachable) or an error string
// (unreachable); degradedStartSymbols extracts the unreachable ones for a
// single clear warning.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditReview7_VDM7_DegradedStartSymbols(t *testing.T) {
	got := degradedStartSymbols(map[string]string{
		"BTC":  "812345",                   // reachable (height)
		"DASH": "rpc: connection refused",  // unreachable (error string)
		"LTC":  "267890",                   // reachable
		"DOGE": "context deadline exceeded", // unreachable
	})
	require.Equal(t, []string{"DASH", "DOGE"}, got, "only unreachable chains, sorted")

	require.Empty(t, degradedStartSymbols(map[string]string{"BTC": "100", "LTC": "200"}),
		"all-reachable start has no degraded chains")
	require.Empty(t, degradedStartSymbols(map[string]string{}),
		"no configured chains -> nothing degraded")
}
