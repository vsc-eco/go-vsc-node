package state_engine_test

// GatewaySettlementParityHeight, state-processing half (A and B).
//
// Settlement is the single write that stops a mined gateway payout being
// re-payable: IndexActions flips each listed action to "complete", and
// GetPendingActions selects purely on status == "pending". Both paths tested
// here skipped IndexActions for the WHOLE FLEET -- every node skips identically,
// so every node re-selects the paid actions next tick and pays them again,
// every ~60 s, for as long as the condition lasts.
//
// Both are gated, because settling a header the chain previously ignored
// changes ledger state at a height. These tests pin BOTH sides: the exact old
// behaviour below the height, the fix at or above it.

import (
	"encoding/json"
	"testing"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// A well-formed header settles with or without parity -- the normal path must
// be untouched.
func TestSettleActionHeader_WellFormed(t *testing.T) {
	raw := []byte(`{"ops":["a1","a2"],"cleared_ops":"AQ"}`)
	for _, parity := range []bool{false, true} {
		up, settle, err := stateEngine.SettleActionHeader(raw, parity)
		assert.NoError(t, err)
		assert.True(t, settle, "a decodable header must always settle (parity=%v)", parity)
		assert.Equal(t, []string{"a1", "a2"}, up.Ops)
		assert.Equal(t, "AQ", up.ClearedOps)
	}
}

// ★ THE FLEET-WIDE DOUBLE-PAY. cleared_ops arrives as a number instead of a
// string -- exactly the shape of an encoder change across a rolling upgrade.
// encoding/json still fills ops before erroring, so the payout CAN be settled;
// the old code threw it away and re-paid every action in the header.
func TestSettleActionHeader_TypeSkewKeepsTheOpsList(t *testing.T) {
	raw := []byte(`{"ops":["a1","a2"],"cleared_ops":7}`)

	// Pre-parity: dropped, which is the live behaviour and must be preserved
	// until the height so a mixed fleet cannot diverge.
	up, settle, err := stateEngine.SettleActionHeader(raw, false)
	assert.Error(t, err)
	assert.False(t, settle, "pre-parity must drop it, unchanged")
	assert.Equal(t, []string{"a1", "a2"}, up.Ops,
		"the ops list survives the decode error — which is why dropping it was avoidable")

	// With parity: settled from the ops list, and the error is still returned
	// so the caller logs the skew rather than recovering silently.
	up, settle, err = stateEngine.SettleActionHeader(raw, true)
	assert.Error(t, err, "the skew must still be reported even though we settle")
	assert.True(t, settle, "parity must settle from the surviving ops list — otherwise every action in this header is paid again each tick")
	assert.Equal(t, []string{"a1", "a2"}, up.Ops)
}

// If ops itself is unusable there is nothing to settle and parity must not
// invent anything. The actions stay pending and reconcilePendingActions reports
// them.
func TestSettleActionHeader_UnusableOpsIsNotSettled(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"ops":"not-a-list"}`),
		[]byte(`not json at all`),
		[]byte(`{"ops":[]}`),
	} {
		_, settle, _ := stateEngine.SettleActionHeader(raw, true)
		if string(raw) == `{"ops":[]}` {
			// Decodes cleanly; an empty batch settles nothing either way.
			assert.True(t, settle, "an empty but valid header is not an error")
			continue
		}
		assert.False(t, settle, "parity must not settle %q — there is no id list to settle", raw)
	}
}

// Guard on the payload shape this all rests on: ops must stay a []string in the
// wire format, since parity's fallback is "the ops list survived".
func TestSettleActionHeader_OpsShapeIsLoadBearing(t *testing.T) {
	var probe struct {
		Ops []string `json:"ops"`
	}
	assert.NoError(t, json.Unmarshal([]byte(`{"ops":["x"],"cleared_ops":{"nested":true}}`), &probe),
		"ops must decode independently of cleared_ops")
	assert.Equal(t, []string{"x"}, probe.Ops)
}
