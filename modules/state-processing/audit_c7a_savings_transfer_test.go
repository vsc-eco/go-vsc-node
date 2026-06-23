package state_engine

// review7 C7-a — a transfer_to_savings to the gateway was silently dropped.
//
// The handler for it was dead code (nested inside the `op.Type == "transfer"`
// branch, so the inner `op.Type == "transfer_to_savings"` check could never be
// true), so the op was neither credited nor flagged — the funds, sitting in the
// gateway's L1 savings, were permanently stranded with no audit trail. The op
// is now detected at the top of the op loop and flagged for manual refund
// (crediting it on L2 would back the liability with illiquid savings, so it is
// deliberately NOT auto-credited).

import (
	"testing"

	"vsc-node/modules/common/params"

	"github.com/stretchr/testify/require"
)

func TestAuditReview7_C7a_DetectsGatewaySavingsTransfer(t *testing.T) {
	// External user sends transfer_to_savings to the gateway — stranding case.
	require.True(t, isUnsupportedGatewaySavingsDeposit("transfer_to_savings", "user123", params.GATEWAY_WALLET),
		"transfer_to_savings to the gateway from an external user must be flagged")

	// Gateway sends transfer_to_savings to itself — normal operation, not flagged.
	require.False(t, isUnsupportedGatewaySavingsDeposit("transfer_to_savings", params.GATEWAY_WALLET, params.GATEWAY_WALLET),
		"gateway doing its own transfer_to_savings must not be flagged")

	// A plain transfer to the gateway is the supported deposit path — not flagged.
	require.False(t, isUnsupportedGatewaySavingsDeposit("transfer", "user123", params.GATEWAY_WALLET),
		"a normal transfer to the gateway must not be flagged")

	// transfer_to_savings to some other account is unrelated to the gateway.
	require.False(t, isUnsupportedGatewaySavingsDeposit("transfer_to_savings", "user123", "someuser"),
		"transfer_to_savings to a non-gateway account is not our concern")
}
