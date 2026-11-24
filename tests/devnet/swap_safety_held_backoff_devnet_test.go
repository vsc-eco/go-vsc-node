//go:build evm_devnet

// S8b: what the held-return keeper does while a refund's floor CANNOT be met.
//
// S8 proves a refused payout's refund comes back once the floor is fillable.
// This proves the other half: how hard the keeper keeps trying while it is not.
// The only thing a refused return waits on is the pool's price, and the price
// moves with every trade, so the wait between dry runs has to stay short.
// held_returns.go caps it at heldReturnsBackoffMax = 5m; it used to inherit
// sendHomeBackoffMax = 1h, which is a ceiling built for a stuck L1 send, where
// nothing the bot can see changes minute to minute.
//
// S8's own run can never exercise that path. With the floor fix the very first
// dry run succeeds — run 6 (2026-11-23) logged ZERO "cannot be swapped back
// yet" lines — so the cap is dead code on a healthy devnet. This scenario makes
// the floor unfillable on purpose, holds it there long enough to measure the
// leash, then lets go and times the recovery.
//
// THE LEVER IS THE PRICE BETWEEN THE TWO LEGS. The floor is written when the
// payout is held, from the forward swap's own rate (returnMin: the trader's
// input valued at this swap's rate, less returnSlipBps), and the contract
// STORES it on the hold record (mapping.PayoutReturnHeld.Floor, from the
// pending spend's ReturnMin) and enforces that stored number on every retry.
// So it is a round-trip-VALUE floor in HBD and does not care about the price
// LEVEL: an adverse trade before the forward swap moves both legs together and
// changes nothing. Placed AFTER the forward swap it makes the return leg settle
// at a worse price than the floor was written at, and the dry run refuses.
//
// Everything here is reversible by construction: the adverse trade is sized
// from the pool's live reserves, and the restore puts back the HBD it drained
// plus an eighth, so the pool ends at least as deep on the HBD side as it
// started and the floor is fillable again.
package devnet

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// prHeldRefusalLine is the keeper's own words for a dry run it could not price
// (held_returns.go). The bot is a child process on slog's default handler, so
// its attrs arrive as key=value text and the backoff reads "next=5m0s".
const prHeldRefusalLine = "cannot be swapped back yet"

// prBackoffLadder is every rung either cap can log: 2x the last, floored at
// heldReturnsEvery (1m). The first four are shared; 8m and up exist only under
// the old sendHomeBackoffMax = 1h ceiling.
var prBackoffLadder = []time.Duration{
	time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute,
	8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour,
}

func prBackoffWord(d time.Duration) string { return "next=" + d.String() }

// prBackoffSeen renders the rungs the log actually reported, in order.
func prBackoffSeen(seen map[time.Duration]bool) string {
	out := make([]string, 0, len(seen))
	ds := make([]time.Duration, 0, len(seen))
	for d := range seen {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	for _, d := range ds {
		out = append(out, d.String())
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, ", ")
}

// prS8bHeldReturnBacksOffInMinutes: a refund whose floor the pool cannot meet
// is retried on a leash of minutes, not hours, and follows the price back as
// soon as the pool can pay it.
func prS8bHeldReturnBacksOffInMinutes(t *testing.T, x *dex3, check prCheck) {
	b := x.b
	d, e := b.d, b.e
	if b.usdc == (ethcommon.Address{}) {
		t.Logf("S8b skipped: no MockUSDC (a recipient that refuses ETH)")
		return
	}
	start := devnetNow()
	hiveBefore := gwL1Hbd(t, d, "magi.test3")
	tail0, _ := strconv.ParseUint(e.stateStr("phrt"), 10, 64)
	base := strings.Count(b.botLogs(), prHeldRefusalLine)

	// (1) The forward leg, exactly S8's: HBD in, ETH out to MockUSDC, which
	// refuses ETH. This is the swap whose rate fixes the floor.
	instr := fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"2000","min_amount_out":"1","recipient":"%s","destination_chain":"ETH","return_address":{"chain":"HIVE","address":"hive:magi.test3"}}`,
		strings.ToLower(b.usdc.Hex()))
	x.call(x.router, "execute", instr, hbdAllow(2000), 60_000)

	// (2) The adverse trade, now: the payout reverts on L1 and the hold appears
	// about three and a half minutes later (S8: 3m30s), and the keeper's first
	// dry run follows within one sweep of that, so this has to be in before
	// then. ETH in, HBD out drains the side the return leg needs. An eighth of
	// the ETH reserve costs the return about 1-(8/9)^2 = 21%, well past the
	// floor's headroom (3.5% in run 6, 6% in the unit test's pool) and small
	// enough that one trade puts it back. No destination_chain: the output
	// settles on the trader's Magi balance, with no L1 leg to wait for.
	ethR, hbdR := ufReserves(t, x, x.poolETH)
	adverse := new(big.Int).Div(big.NewInt(ethR), big.NewInt(8))
	x.call(x.router, "execute", fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"eth","asset_out":"hbd","amount_in":"%s","min_amount_out":"1","recipient":"%s"}`,
		adverse, dex3Trader), nil, 0)
	ethR2, hbdR2 := ufReserves(t, x, x.poolETH)
	drained := hbdR - hbdR2
	t.Logf("S8b adverse trade %s wei eth -> hbd: pool eth %d -> %d, hbd %d -> %d (%d milli out, %.1f%% of the HBD side)",
		adverse, ethR, ethR2, hbdR, hbdR2, drained, 100*float64(drained)/float64(hbdR))
	check(drained > 0, "S8b the adverse trade moved the price against the refund (%d milli-HBD out of the pool)", drained)

	// (3) The hold.
	heldID, raced := uint64(0), false
	err := b.botPoll(40*time.Minute, func() bool {
		for id := range prHeld(e) {
			if id >= tail0 {
				heldID = id
				return true
			}
		}
		// Swapped back without ever being seen held: the adverse trade lost the
		// race, so there is nothing to measure. Reported, never passed over.
		if t2, _ := strconv.ParseUint(e.stateStr("phrt"), 10, 64); t2 > tail0 {
			raced = true
			return true
		}
		return false
	})
	check(err == nil, "S8b the refused payout came back to the bridge's hold (%s)", devnetSince(start).Round(time.Second))
	if err != nil {
		return
	}
	if raced {
		check(false, "S8b the adverse trade did not beat the keeper's first dry run: the refund was swapped back before the floor could be made unfillable, so the backoff was never exercised (widen the adverse trade and re-run)")
		return
	}
	t.Logf("S8b held as phr-%d, floor unfillable; watching the keeper's dry runs", heldID)

	// (4) The leash. Each refusal is one log line; the gap between two of them
	// is what a user waits. Measured on the REAL clock, which is the one the
	// bot's child process runs on — devnetNow() carries the harness's test-clock
	// lead, which the bot never sees.
	//
	// The sweep runs once a minute (heldReturnsEvery), so an attempt due at +4m
	// can fire a minute late: the bound is the 5m cap plus that sweep plus this
	// poll's own 3s tick. The old 1h ceiling doubles 1, 2, 4, 8, 16...; its 8m
	// gap closes at +15m, inside the window below and far outside the bound.
	const watch = 18 * time.Minute
	refusals, worst := 0, time.Duration(0)
	first, last := time.Time{}, time.Time{}
	seen := map[time.Duration]bool{}
	_ = b.botPoll(watch+8*time.Minute, func() bool {
		logs := b.botLogs()
		for n := strings.Count(logs, prHeldRefusalLine) - base; refusals < n; refusals++ {
			now := time.Now()
			if refusals == 0 {
				first = now
			} else if gap := now.Sub(last); gap > worst {
				worst = gap
			}
			last = now
		}
		for _, rung := range prBackoffLadder {
			if strings.Contains(logs, prBackoffWord(rung)) {
				seen[rung] = true
			}
		}
		return !first.IsZero() && time.Since(first) >= watch
	})
	t.Logf("S8b %d dry runs refused over %s, longest wait %s, backoff rungs logged: %s",
		refusals, time.Since(first).Round(time.Second), worst.Round(time.Second), prBackoffSeen(seen))

	check(refusals >= 5, "S8b the keeper kept asking while the floor was unfillable: %d dry runs in %s", refusals, time.Since(first).Round(time.Second))
	check(refusals >= 5 && worst > 0 && worst <= 6*time.Minute+20*time.Second,
		"S8b USER: the longest the refund waited between attempts was %s (the 5m cap plus one 1m sweep)", worst.Round(time.Second))
	check(seen[5*time.Minute], "S8b the backoff reached its 5m cap and stopped (rungs: %s)", prBackoffSeen(seen))
	over := []string{}
	for _, rung := range prBackoffLadder {
		if rung > 5*time.Minute && seen[rung] {
			over = append(over, rung.String())
		}
	}
	check(len(over) == 0, "S8b the backoff never climbed past the cap (the old 1h ladder reaches 8m by +15m; logged %v)", over)

	// (5) Let go. This is what the cap buys: the refund follows the price back
	// in minutes. Under the old ceiling the keeper would be waiting 16m by now,
	// and a price that recovered a second later would cost all of it.
	restore := drained + drained/8
	x.call(x.router, "execute", fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"eth","amount_in":"%d","min_amount_out":"1","recipient":"%s"}`,
		restore, dex3Trader), hbdAllow(restore+1_000), 60_000)
	ethR3, hbdR3 := ufReserves(t, x, x.poolETH)
	t.Logf("S8b restore %d milli hbd -> eth: pool eth %d -> %d, hbd %d -> %d", restore, ethR2, ethR3, hbdR2, hbdR3)
	check(hbdR3 >= hbdR, "S8b the pool's HBD side is back where the floor was written (%d -> %d -> %d)", hbdR, hbdR2, hbdR3)

	freed := time.Now()
	got := int64(0)
	err = b.botPoll(20*time.Minute, func() bool {
		got = gwL1Hbd(t, d, "magi.test3") - hiveBefore
		return got > 0
	})
	waited := time.Since(freed)
	check(err == nil && got > 1_500 && got <= 2_000, "S8b USER: %d of 2000 milli-HBD back on Hive, %s after the swap", got, devnetSince(start).Round(time.Second))
	// 5m cap + 1m sweep + the swap back and the Hive payout (S8: ~2m from the
	// swap back to the credit). The old ladder would be 16m into a wait here.
	check(err == nil && waited <= 10*time.Minute, "S8b USER: the refund followed the price back in %s, not the best part of an hour", waited.Round(time.Second))
	check(len(prHeld(e)) == 0 || prHeld(e)[heldID] == "", "S8b nothing left held (%v)", prHeld(e))
}
