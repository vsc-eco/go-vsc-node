package ledgerSystem_test

// Focused coverage for the feature/simulate-dep flow: a deposit appended to
// an in-memory session must be visible to a subsequent transfer on that same
// session. Milo's node-level repro had the deposit "silently succeed" and the
// follow-up swap still report "insufficient balance" — these tests pin down
// whether that's a primitive-layer bug (session cache miss / wrong key /
// ExecuteOplog doesn't see the deposit) or wiring above it.

import (
	"testing"

	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Session.AppendLedger → subsequent GetBalance returns the credit.
func TestSimulatedDeposit_BalanceVisibleToSameSession(t *testing.T) {
	state := makeTestLedgerState()
	session := ledgerSystem.NewSession(state)

	require.Equal(t, int64(0), session.GetBalance("hive:alice", 100, "hbd"),
		"alice starts with no on-chain HBD")

	session.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:          "sim-dep-1",
		BlockHeight: 100,
		Owner:       "hive:alice",
		Amount:      1000,
		Asset:       "hbd",
		Memo:        "to=alice",
		Type:        "deposit",
	})

	assert.Equal(t, int64(1000), session.GetBalance("hive:alice", 100, "hbd"),
		"deposit credit must be visible to GetBalance on the same session")
}

// ExecuteTransfer reads the deposit-updated balance and succeeds for an
// amount that would fail against on-chain state alone.
func TestSimulatedDeposit_TransferSucceedsAgainstInSessionBalance(t *testing.T) {
	state := makeTestLedgerState()
	session := ledgerSystem.NewSession(state)

	session.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:          "sim-dep-1",
		BlockHeight: 100,
		Owner:       "hive:alice",
		Amount:      1000,
		Asset:       "hbd",
		Type:        "deposit",
	})

	res := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "sim-xfer-1",
		From:        "hive:alice",
		To:          "contract:vsc1pool",
		Amount:      700,
		Asset:       "hbd",
		BlockHeight: 100,
	})

	require.True(t, res.Ok, "transfer within deposit amount should succeed: %s", res.Msg)
	assert.Equal(t, int64(300), session.GetBalance("hive:alice", 100, "hbd"),
		"alice's HBD should be the deposit minus the transfer")
	assert.Equal(t, int64(700), session.GetBalance("contract:vsc1pool", 100, "hbd"),
		"the pool contract should have received 700 HBD")
}

// ExecuteTransfer honours the HBD RC-exclusion reserve. When rc_limit =
// rcFreeRemaining the exclusion is 0 and the transfer only needs the pull
// amount; matches PullBalance's behaviour in execution-context.go.
func TestSimulatedDeposit_TransferRespectsExclusion(t *testing.T) {
	state := makeTestLedgerState()
	session := ledgerSystem.NewSession(state)

	session.AppendLedger(ledgerSystem.LedgerUpdate{
		BlockHeight: 100,
		Owner:       "hive:alice",
		Amount:      1000,
		Asset:       "hbd",
		Type:        "deposit",
	})

	// exclusion = 500 leaves 500 spendable. 700 > 500 → rejected.
	res := session.ExecuteTransfer(
		ledgerSystem.OpLogEvent{
			Id:          "xfer-over",
			From:        "hive:alice",
			To:          "contract:vsc1pool",
			Amount:      700,
			Asset:       "hbd",
			BlockHeight: 100,
		},
		ledgerSystem.TransferOptions{Exclusion: 500},
	)
	assert.False(t, res.Ok, "(1000 - 500) < 700 should fail")
	assert.Equal(t, "insufficient balance", res.Msg)

	// exclusion = 0 (rcFreeRemaining covers the whole rc_limit) → passes.
	res = session.ExecuteTransfer(
		ledgerSystem.OpLogEvent{
			Id:          "xfer-ok",
			From:        "hive:alice",
			To:          "contract:vsc1pool",
			Amount:      700,
			Asset:       "hbd",
			BlockHeight: 100,
		},
		ledgerSystem.TransferOptions{Exclusion: 0},
	)
	assert.True(t, res.Ok, "with exclusion=0 the deposit covers the pull: %s", res.Msg)
}

// Revert() discards the deposit so the next simulation run starts clean —
// the resolver's `defer ledgerSession.Revert()` relies on this.
func TestSimulatedDeposit_RevertDropsInSessionBalance(t *testing.T) {
	state := makeTestLedgerState()
	session := ledgerSystem.NewSession(state)

	session.AppendLedger(ledgerSystem.LedgerUpdate{
		BlockHeight: 100,
		Owner:       "hive:alice",
		Amount:      1000,
		Asset:       "hbd",
		Type:        "deposit",
	})
	require.Equal(t, int64(1000), session.GetBalance("hive:alice", 100, "hbd"))

	session.Revert()

	assert.Equal(t, int64(0), session.GetBalance("hive:alice", 100, "hbd"),
		"Revert() must clear the in-session credit")
	assert.Equal(t, 0, len(state.VirtualLedger),
		"live VirtualLedger must stay untouched — simulation never persists")
}

// Owner-resolution parity: the helper used by executeSimulatedDeposit must
// produce the same account string the live Deposit() flow writes to.
func TestSimulatedDeposit_ResolveDepositTargetMatchesLivePath(t *testing.T) {
	// Bare username via memo → hive: prefix added.
	assert.Equal(t,
		"hive:alice",
		ledgerSystem.ResolveDepositTarget("to=alice", "alice"),
		"bare 'to=alice' must resolve to hive:alice")

	// Pre-prefixed hive: passes through.
	assert.Equal(t,
		"hive:bob",
		ledgerSystem.ResolveDepositTarget("to=hive:bob", "alice"),
		"explicit hive:bob memo should target bob, not the sender")

	// Empty 'to=' parses cleanly but yields an empty target; the
	// "invalid, default to sender" branch kicks in and returns from as-is.
	// (Bug in live Deposit() arguably — it doesn't re-prefix — but the sim
	// has to match for identical behaviour, so pin the current semantics.)
	assert.Equal(t,
		"alice",
		ledgerSystem.ResolveDepositTarget("to=", "alice"),
		"empty 'to=' mirrors the live Deposit() fallback: return from as-is")
}
