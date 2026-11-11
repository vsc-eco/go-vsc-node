//go:build evm_devnet

// Three-asset failure states (owner 11-11: "since theres usdc eth and btc pool
// now ... consider every one"): the BOTH cycle with USDC as its third asset,
// where USDC itself fails mid-migration. Circle can pause USDC for everyone or
// blacklist one address; either one makes the token sweep from the retiring
// EVM generation revert on L1. Harness: evm_btc_both_env_test.go (BB_USDC on).
//
// Run: go test -tags evm_devnet -run 'TestBothBridges_FS1[67]' ./tests/devnet/ -timeout 330m -v
package devnet

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

// fs16TokenStuck waits until the retiring generation's USDC sweep has reverted
// MaxSweepReverts (2) times, so migrateVault reports the token stuck and the
// generation stays funded (NN#3). It fails the test if the sweep mined first:
// the fault then came too late to test anything.
func fs16TokenStuck(b *bothEnv) {
	t, e := b.t, b.e
	if err := b.botPoll(60*time.Minute, func() bool { return e.stateStr("msx-0-usdc") == "2" }); err != nil {
		t.Fatalf("the USDC sweep never reverted twice (msx-0-usdc=%q, vb-0-usdc=%q, gen-0 USDC %s): did it mine before the fault?\nbot:\n%s",
			e.stateStr("msx-0-usdc"), e.stateStr("vb-0-usdc"), b.usdcAt(b.vault0), b.botLogs())
	}
	if g0 := b.usdcAt(b.vault0); g0.Cmp(bbUSD(25)) != 0 {
		t.Fatalf("generation 0 holds %s USDC units after two reverted sweeps, want 25000000", g0)
	}
	if evmGenPurged(b.ctx, e, 0) {
		t.Fatal("generation 0 was purged while its USDC is still booked to it")
	}
}

// FS16: USDC is paused (Circle's global pause) right after both cut-overs,
// before the EVM batch is built. The ETH sweep and the whole BTC side must not
// notice; the USDC sweep reverts twice and the generation reports the token
// stuck; the USDC withdrawal queued in the migration waits (no L1 attempt
// against an empty generation). Once USDC is unpaused the owner retries the
// token (retryTokenSweep, D-2): the sweep mines, the queued withdrawal pays,
// generation 0 purges and every unit is where the books say.
func TestBothBridges_FS16_UsdcPausedMidMigration(t *testing.T) {
	bbSkipShort(t)
	if !bbUSDC() {
		t.Skip("BB_USDC=0")
	}
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	usdcTx := func(sig string) {
		t.Helper()
		if rc, err := b.anvil.SendTx(b.ctx, b.usdcOwner, &b.usdc, usdcCall(sig), 100_000); err != nil || rc.Status != 1 {
			t.Fatalf("USDC %s: %v", sig, err)
		}
	}
	b.runCycle(cycleHooks{
		usdcOwn: true,
		afterActivate: func() {
			usdcTx("pause()")
			b.note("USDC paused right after both cut-overs")
		},
		btcSwept: func() {
			// BTC's migration finished with USDC paused. Now the token side.
			fs16TokenStuck(b)
			if got := b.usdcAt(b.usdcRecip); got.Sign() != 0 {
				t.Fatalf("the queued USDC withdrawal paid %s while USDC was paused and unswept", got)
			}
			b.note("USDC sweep reverted twice; generation 0 stuck on usdc; queued withdrawal waiting")
			usdcTx("unpause()")
			if ok, msg := b.e.ownerCall("retryTokenSweep", `{"gen":0,"symbol":"usdc"}`); !ok {
				t.Fatalf("retryTokenSweep: %s", msg)
			}
			b.note("USDC unpaused, owner retried the token sweep")
			paid := func() bool { return b.usdcAt(b.usdcRecip).Cmp(bbUSD(5)) == 0 }
			if err := b.botPoll(20*time.Minute, paid); err != nil {
				// Each reverted attempt booked its gas from the ETH kept back for
				// the token sweeps (2x one sweep's gas), so a third attempt can
				// find too little: the documented way out is a to_reserve deposit
				// to the retiring address (migration.go tooLittleGasStatus).
				t.Logf("MEASURE FS16: the retried token sweep did not pay within 20 min (bot names too little gas: %v, vb-0-eth %q); topping generation 0 up (to_reserve)",
					strings.Contains(b.botLogs(), "too little ETH"), b.e.stateStr("vb-0-eth"))
				fs16TopUp(b)
				if err := b.botPoll(40*time.Minute, paid); err != nil {
					t.Fatalf("the queued USDC withdrawal never paid after the retry and the top-up (holds %s)\nbot:\n%s", b.usdcAt(b.usdcRecip), b.botLogs())
				}
			}
			b.note("queued USDC withdrawal paid after the retry")
		},
		settled: func() {
			vault1, ok := tssKeyAddress(b.e.keyPub(evmKeyName(1)))
			if !ok {
				t.Fatal("no address for EVM generation 1")
			}
			g0, g1, user, sup := b.usdcAt(b.vault0), b.usdcAt(vault1), b.userUSDC(), b.e.stateStr("s-usdc")
			if g0.Sign() != 0 || g1.Cmp(bbUSD(20)) != 0 || user != bbUSD(20).String() || sup != "20000000|20000000|0|0" {
				t.Fatalf("USDC after the paused migration: gen0 %s, gen1 %s (want 20000000), user %q (want 20000000), s-usdc %q",
					g0, g1, user, sup)
			}
			t.Logf("CASE FS16 PASS: USDC paused mid-migration: BTC and ETH unaffected, token stuck then retried; 20 USDC at gen 1, 5 paid out, books whole")
		},
	})
}

// FS17: Circle blacklists the retiring generation's address right after both
// cut-overs (the case with no undo: the token can never leave that address).
// BTC and ETH must finish. The USDC sweep reverts twice; the owner writes the
// token off (writeOffToken books the loss: custody drops, users' balances do
// not). The test does not decide what is right for users; it records what the
// bridge does next: the custody and user totals, what happens to the USDC
// withdrawal queued in the migration, and whether generation 0 then purges so
// later rotations are not blocked.
func TestBothBridges_FS17_UsdcBlacklistedOldVault(t *testing.T) {
	bbSkipShort(t)
	if !bbUSDC() {
		t.Skip("BB_USDC=0")
	}
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	cancelled := false
	b.runCycle(cycleHooks{
		usdcOwn: true,
		afterActivate: func() {
			if rc, err := b.anvil.SendTx(b.ctx, b.usdcOwner, &b.usdc, usdcCall("blacklist(address)", b.vault0), 100_000); err != nil || rc.Status != 1 {
				t.Fatalf("USDC blacklist(gen 0): %v", err)
			}
			b.note("USDC blacklisted generation 0 %s right after both cut-overs", b.vault0.Hex())
		},
		btcSwept: func() {
			fs16TokenStuck(b)
			before := b.e.stateStr("s-usdc")
			ok, msg := b.e.ownerCall("writeOffToken", `{"gen":0,"symbol":"usdc"}`)
			if !ok {
				// OPS-2: the queued USDC withdrawal's debit left the custody
				// (Active) when it was requested but is still in generation 0's
				// book, so with less USDC at generation 1 than is queued the
				// book is above Active and the write-off aborts. The owner's
				// way out with this contract: cancel the queued USDC
				// withdrawals (refunded on L2), then write off.
				if !strings.Contains(msg, "above the custody") {
					t.Fatalf("writeOffToken: %s", msg)
				}
				t.Logf("MEASURE FS17 writeOffToken refused with a USDC withdrawal queued: %q (s-usdc %q, vb-0-usdc %q, wqd-usdc %q)",
					msg, before, b.e.stateStr("vb-0-usdc"), b.e.stateStr("wqd-usdc"))
				n := fs17CancelQueued(b, "usdc")
				cancelled = true
				b.note("owner cancelled %d queued USDC withdrawal(s) before the write-off: s-usdc %s -> %s, user L2 %s",
					n, before, b.e.stateStr("s-usdc"), b.userUSDC())
				before = b.e.stateStr("s-usdc")
				if ok, msg = b.e.ownerCall("writeOffToken", `{"gen":0,"symbol":"usdc"}`); !ok {
					t.Fatalf("writeOffToken after cancelling the queued USDC withdrawals: %s", msg)
				}
			}
			b.note("owner wrote off generation 0's USDC: s-usdc %s -> %s", before, b.e.stateStr("s-usdc"))
			// What the queued 5 USDC withdrawal does once the books say the
			// active generation has nothing: watch it for 15 minutes.
			deadline := devnetNow().Add(15 * time.Minute)
			for !cancelled && devnetNow().Before(deadline) {
				if b.usdcAt(b.usdcRecip).Sign() != 0 {
					break
				}
				devnetSleep(30 * time.Second)
			}
			t.Logf("MEASURE FS17 queued USDC withdrawal after the write-off: recipient holds %s; user L2 %q; s-usdc %q; gas reserve %q",
				b.usdcAt(b.usdcRecip), b.userUSDC(), b.e.stateStr("s-usdc"), b.e.stateStr("gr"))
		},
		settled: func() {
			vault1, _ := tssKeyAddress(b.e.keyPub(evmKeyName(1)))
			t.Logf("MEASURE FS17 end: gen0 holds %s USDC (frozen), gen1 %s, user L2 %q, recipient %s, s-usdc %q, si %q",
				b.usdcAt(b.vault0), b.usdcAt(vault1), b.userUSDC(), b.usdcAt(b.usdcRecip), b.e.stateStr("s-usdc"), b.e.stateStr("si"))
			if !evmGenPurged(b.ctx, b.e, 0) {
				t.Fatal("generation 0 did not purge after the write-off: every later EVM rotation stays blocked (NN#3)")
			}
			if cancelled {
				// The 25 USDC at generation 0 are gone; users still hold all 25
				// on L2 (20 + the refunded 5): custody 0, user 25.
				if sup, u, r := b.e.stateStr("s-usdc"), b.userUSDC(), b.usdcAt(b.usdcRecip); sup != "0|25000000|0|0" || u != "25000000" || r.Sign() != 0 {
					t.Fatalf("after cancel + write-off: s-usdc %q (want 0|25000000|0|0), user L2 %q (want 25000000), recipient %s (want 0)", sup, u, r)
				}
			}
			t.Logf("CASE FS17 PASS: blacklisted old vault: BTC and ETH finished, token written off, generation 0 purged (user outcome in the MEASURE lines)")
		},
	})
	if !strings.Contains(strings.Join(b.notes, "\n"), "wrote off") {
		t.Fatal("the write-off never ran")
	}
}

// fs17CancelQueued cancels, as the contract owner, every queued (not yet
// issued) withdrawal of asset (cancelQueuedWithdrawal: the debit goes back to
// the user's L2 balance and into Active and User). Returns how many.
func fs17CancelQueued(b *bothEnv, asset string) int {
	t, e := b.t, b.e
	head, _ := strconv.ParseUint(e.stateStr("wqh"), 10, 64)
	tail, _ := strconv.ParseUint(e.stateStr("wqt"), 10, 64)
	n := 0
	for seq := head; seq < tail; seq++ {
		raw := e.stateStr("wq-" + strconv.FormatUint(seq, 10))
		var q struct {
			Asset string `json:"asset"`
		}
		if raw == "" || json.Unmarshal([]byte(raw), &q) != nil || q.Asset != asset {
			continue
		}
		if ok, msg := e.ownerCall("cancelQueuedWithdrawal", fmt.Sprintf(`{"seq":%d}`, seq)); !ok {
			t.Fatalf("cancelQueuedWithdrawal %d: %s", seq, msg)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no queued %s withdrawal to cancel (wqh %d, wqt %d)", asset, head, tail)
	}
	return n
}

// fs16TopUp sends 0.05 ETH to the retiring generation's address with the
// to_reserve instruction as calldata (handlers.go parseCalldataInstructions):
// the gas the token sweep needs, booked to that generation like a late deposit.
func fs16TopUp(b *bothEnv) {
	t := b.t
	key := mustKey(t)
	from := cryptoPubkeyAddr(key)
	if err := b.anvil.SetBalance(b.ctx, from, bbEth(1)); err != nil {
		t.Fatal(err)
	}
	nonce, err := b.anvil.cli.PendingNonceAt(b.ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	to := b.vault0
	tx, err := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID: b.anvil.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(50e9),
		Gas: 60_000, To: &to, Value: new(big.Int).Div(bbEth(1), big.NewInt(20)), Data: []byte("to_reserve=1"),
	}), ethtypes.LatestSignerForChainID(b.anvil.ChainID), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.anvil.cli.SendTransaction(b.ctx, tx); err != nil {
		t.Fatalf("to_reserve top-up: %v", err)
	}
	if rc, err := b.anvil.waitReceipt(b.ctx, tx.Hash(), 30*time.Second); err != nil || rc.Status != 1 {
		t.Fatalf("to_reserve top-up not mined: %v", err)
	}
	b.note("0.05 ETH to_reserve top-up sent to generation 0 %s (tx %s)", to.Hex(), tx.Hash().Hex())
}
