package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// A payout that fails for good goes back to its sender on the sender's chain
// (account-mapping PAYOUT-1, THORChain's reverse swap). The bridge holds it
// ("phr-<id>", ids from "phrh" to "phrt"), never in a Magi balance, and only
// this bot (a registered relayer on the bridge) runs the swap back
// (retryPayoutReturn): anyone else's call could be wrapped and skimmed. Each
// return is dry-run first at min_out 1, which returns what the swap would give
// at today's price; it is then sent with min_out at 99% of that (the bridge
// still keeps the swap's own floor for the first day). A dry run that fails
// (no pool, the floor not met yet) costs no RC and is tried again later, less
// often each time. The sender can also take it back at their own price
// (releasePayoutReturn). Nothing is lost when the bot misses one.

const (
	heldReturnsEvery   = time.Minute
	heldReturnsPerTick = 3
	// heldReturnsScan bounds how many ids past the head are read a tick.
	heldReturnsScan = 32
	// heldReturnsBackoffMax caps the wait between dry runs of ONE held return.
	// sendHomeBackoffMax (an hour) is wrong here for two reasons: what a refused
	// return is waiting for is the PRICE (the bridge keeps the swap's own floor
	// for the first day), which moves with every trade and not on the keeper's
	// schedule, and the dry run that checks it is a simulate — it costs no RC
	// and submits nothing, so waiting longer buys nothing. The per-tick cap and
	// the moving window bound the load whatever this is.
	//
	// Devnet SWAP-SAFETY S8, 2026-11-23: the floor was unfillable, the backoff
	// doubled 1 -> 2 -> 4 -> 8 -> 16 minutes, and when an unrelated swap finally
	// moved the pool the favourable way the refund waited out the rest of a
	// 16-minute sleep before anyone looked again.
	heldReturnsBackoffMax = 5 * time.Minute
	// heldReturnsAlertAfter: say so while an operator can still act. The held
	// floor is flat for constants.PayoutFloorFor (a day) and then decays 5% a
	// day, so sendHomeAlertAfter's 24 hours first alerts exactly when the
	// sender starts losing value. An hour of refusals is already abnormal.
	heldReturnsAlertAfter = time.Hour
)

var (
	lastHeldReturnsAt time.Time
	heldReturnState   = map[uint64]*sendHomeEntry{}
	// heldReturnsCursor is where the next tick's window starts: the window
	// moves on every tick (wrapping at the tail), so returns stuck below their
	// floors at the head never keep the keeper from the ones behind them.
	heldReturnsCursor uint64
)

// heldReturnsWindow is the ids [start, end) a tick reads, and the cursor for
// the next tick.
func heldReturnsWindow(head, tail, cursor uint64) (start, end, next uint64) {
	start = cursor
	if start < head || start >= tail {
		start = head
	}
	end = min(tail, start+heldReturnsScan)
	next = end
	if next >= tail {
		next = head
	}
	return start, end, next
}

type heldReturnRecord struct {
	To    string `json:"t"`
	Asset string `json:"a"`
}

// heldReturnRecordOf parses a held return's record ("" or junk: not held).
func heldReturnRecordOf(raw string) (heldReturnRecord, bool) {
	var h heldReturnRecord
	if raw == "" || json.Unmarshal([]byte(raw), &h) != nil || h.To == "" {
		return h, false
	}
	return h, true
}

// heldReturnMinOut is the min_out the bot sends a swap back with: 99% of what
// its dry run gave (amount_out), at least 1; ok false when the dry run gave
// no amount (then nothing is sent: a swap back is never sent unpriced).
func heldReturnMinOut(amountOut string) (string, bool) {
	out, ok := new(big.Int).SetString(strings.Trim(amountOut, `"`), 10)
	if !ok || out.Sign() <= 0 {
		return "", false
	}
	min := new(big.Int).Div(new(big.Int).Mul(out, big.NewInt(99)), big.NewInt(100))
	if min.Sign() <= 0 {
		min.SetInt64(1)
	}
	return min.String(), true
}

// retryHeldReturns tries the bridge's held payout returns again.
func retryHeldReturns(ctx context.Context, gql *vscGraphQL, submitter *l2Submitter, cfg EVMBotConfig) {
	if cfg.Paused || time.Since(lastHeldReturnsAt) < heldReturnsEvery {
		return
	}
	lastHeldReturnsAt = time.Now()
	st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"phrh", "phrt"})
	if err != nil {
		slog.Warn("held-return keeper: could not read the hold; trying again next tick", "err", err)
		return
	}
	head, _ := strconv.ParseUint(st["phrh"], 10, 64)
	tail, _ := strconv.ParseUint(st["phrt"], 10, 64)
	if tail <= head {
		clear(heldReturnState)
		return
	}
	start, end, next := heldReturnsWindow(head, tail, heldReturnsCursor)
	heldReturnsCursor = next
	keys := make([]string, 0, end-start)
	for id := start; id < end; id++ {
		keys = append(keys, "phr-"+strconv.FormatUint(id, 10))
	}
	recs, err := gql.fetchContractState(ctx, cfg.ContractID, keys)
	if err != nil {
		slog.Warn("held-return keeper: could not read the held returns; trying again next tick", "err", err)
		return
	}
	for id := range heldReturnState {
		if id < head || (id >= start && id < end && recs["phr-"+strconv.FormatUint(id, 10)] == "") {
			delete(heldReturnState, id) // sent (by us or by someone else)
		}
	}
	did := submitter.did.String()
	if rc, known, rcErr := gql.fetchAccountRC(ctx, did); rcErr == nil && known && rc < sendHomeRcReserve {
		slog.Warn("held-return keeper: returns wait, the bot's RC is below the keeper's reserve", "rc", rc, "reserve", sendHomeRcReserve)
		return
	}
	calls := 0
	now := time.Now()
	for id := start; id < end; id++ {
		if calls >= heldReturnsPerTick || ctx.Err() != nil {
			return
		}
		rec, ok := heldReturnRecordOf(recs["phr-"+strconv.FormatUint(id, 10)])
		if !ok {
			continue // sent
		}
		s := heldReturnState[id]
		if s == nil {
			s = &sendHomeEntry{}
			heldReturnState[id] = s
		}
		if now.Before(s.nextTry) {
			continue
		}
		probe, _ := json.Marshal(map[string]any{"id": id, "min_out": "1"})
		sim, simErr := gql.simulateContractCall(ctx, did, cfg.ContractID, "retryPayoutReturn", string(probe))
		minOut, priced := "", false
		if simErr == nil && sim.Success {
			minOut, priced = heldReturnMinOut(sim.Ret)
		}
		if !priced {
			if s.refusedAt.IsZero() {
				s.refusedAt = now
			}
			s.backoff = min(max(2*s.backoff, heldReturnsEvery), heldReturnsBackoffMax)
			s.nextTry = now.Add(s.backoff)
			if !s.alerted && now.Sub(s.refusedAt) >= heldReturnsAlertAfter {
				s.alerted = true
				slog.Error("a held payout return could not be swapped back for an hour; check the pool, the bot's relayer registration, or let its sender release it (releasePayoutReturn)",
					"id", id, "to", rec.To, "asset", rec.Asset, "err", sim.Err, "reason", sim.ErrMsg, "ret", sim.Ret)
			} else {
				slog.Info("held payout return cannot be swapped back yet; trying again later", "id", id, "reason", sim.ErrMsg, "next", s.backoff)
			}
			continue
		}
		payload, _ := json.Marshal(map[string]any{"id": id, "min_out": minOut})
		tx, err := submitter.callContractL2(ctx, cfg.ContractID, "retryPayoutReturn", payload, max(uint64(sim.RcUsed)*2, 1))
		calls++
		if err != nil {
			slog.Warn("retryPayoutReturn did not go out; trying again later", "id", id, "err", err)
			s.nextTry = now.Add(heldReturnsEvery)
			continue
		}
		s.refusedAt, s.backoff, s.alerted = time.Time{}, 0, false
		s.nextTry = now.Add(sendHomeResend)
		slog.Info("held payout return swapped back", "id", id, "to", rec.To, "min_out", minOut, "tx", tx)
	}
}
