//go:build evm_devnet

// Carve-out of the three SWAP-SAFETY scenarios that failed the full
// TestSwapSafety_EveryRefundPathTogether run of 2026-11-22 (46 SS ok, 4 SS FAIL,
// GOTEST_EXIT=1): S8 (a refused Hive-paid payout swapped back to the sender on
// Hive), S10 (a payout stuck while the bot was down, rolled by a third party and
// sent again) and S11 (the bot rolls a priced-out payout itself).
//
// The full suite is ~3h, which is too slow to fix anything against. These three
// need only the devnet, a funded gas reserve and the wired router, so they fit
// in well under an hour.
//
// It also DUMPS THE BOT'S OWN LOG. StartBot runs the bot as a child process and
// captures its stdout/stderr into Bot.logs, but nothing PRINTS them unless some
// t.Fatalf interpolates botLogs() — and none of these scenarios does. Every one
// of the failures is "the user's money never arrived inside the poll budget",
// and the bot's reasoning is the only thing that separates a starved harness
// from a bot that read the work and declined it.
//
// The dump is a t.Cleanup, NOT a tail call in the body. Run 1 (2026-11-23,
// 7471s, 8 ok / 3 FAIL) printed no dump at all: S11's own setup swap went
// through x.call, whose outResult timed out ("no contract output observed for
// tx ... within timeout"), and x.call answers that with t.Fatalf. That killed
// the test function two statements short of the dump and cost the whole run its
// diagnostics. A cleanup registered here runs before the devnet teardown
// (cleanups are LIFO and newBothEnvDEX3 registered its own first), on every
// exit path including t.Fatal.
package devnet

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSwapSafety_PayoutReturnsAndRollsOnly(t *testing.T) {
	// Match the full suite: SS_SLIP_PARITY=1 runs the pools on THORChain's fee
	// rule from block 1, which is how the failing run was configured.
	parity := os.Getenv("SS_SLIP_PARITY") == "1"
	var opts []func(*Config)
	if parity {
		opts = append(opts, func(c *Config) { c.SysConfigOverrides.ConsensusParams.PendulumSlipParityHeight = 1 })
	}
	x := newBothEnvDEX3(t, 170*time.Minute, opts...)
	x.setup()
	prFundReserve(t, x) // payouts borrow their gas gap from it
	b := x.b
	fails := 0
	check := func(ok bool, format string, a ...any) {
		if !ok {
			fails++
			t.Errorf("SS FAIL: "+format, a...)
		} else {
			t.Logf("SS ok: "+format, a...)
		}
	}

	// Every exit path, including a t.Fatal inside a scenario's setup.
	t.Cleanup(func() {
		logs := b.botLogs()
		t.Logf("BOT LOG keeper/roll/payout lines:\n%s", ssPickLines(logs, ssBotNeedles...))
		t.Logf("BOT LOG tail:\n%s", ssTailLines(logs, 160))
	})
	// What the vault actually broadcast, which the bot's log cannot tell apart
	// from a roll's nonce-burning self-transfer. See prL1VaultTxs.
	t.Cleanup(func() { prL1VaultTxs(t, x, "final") })

	prS0WireRouter(t, x, check)
	prS8HivePaidPayoutRefused(t, x, check)
	prS10RolledPayoutResent(t, x, check)
	prL1VaultTxs(t, x, "after-S10")
	prS11BotRollsAPricedOutPayout(t, x, check)
	// Last, and deliberately so: S8b moves the ETH/HBD price on purpose and
	// puts it back. Running it after the three scenarios that assert payout
	// amounts keeps that perturbation out of their way entirely.
	prS8bHeldReturnBacksOffInMinutes(t, x, check)

	if fails == 0 {
		t.Logf("CASE SWAP-SAFETY ROLLS PASS: S8, S10, S11, S8b")
	} else {
		t.Logf("CASE SWAP-SAFETY ROLLS FAIL: %d checks failed", fails)
	}
}

// ssBotNeedles are the bot's own words on the three paths under test: the
// held-return keeper (held_returns.go), the roll/re-send of a stuck payout, and
// the reserve that lends a payout its gas gap. "did not run" and "min_out"
// catch the contract's refusal text coming back through the dry run, which is
// the one line that says whether the swap back was refused on price (the floor)
// or never attempted at all.
var ssBotNeedles = []string{
	"held payout return", "held-return keeper", "retryPayoutReturn", "payout return",
	"did not run", "min_out", "amount_out", "dry run", "floor", "phr-",
	"roll", "rolled", "unstuck", "confirmSpend", "withdrawal", "reserve", "relayer",
	"refus", "error", "warn",
}

// ssPickLines keeps the bot log lines that mention any of the given needles,
// capped so a long run cannot bury the output.
func ssPickLines(s string, needles ...string) string {
	out := make([]string, 0, 400)
	for _, ln := range strings.Split(s, "\n") {
		low := strings.ToLower(ln)
		for _, n := range needles {
			if strings.Contains(low, strings.ToLower(n)) {
				out = append(out, ln)
				break
			}
		}
		if len(out) >= 400 {
			out = append(out, "... (truncated at 400 matching lines)")
			break
		}
	}
	return strings.Join(out, "\n")
}

func ssTailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
