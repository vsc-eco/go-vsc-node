package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A swap paid from Ethereum that does not complete, and a payout of a swap
// that comes back from Ethereum, are owed home: the bridge holds them on
// system:owed-home (never on the sender's Magi balance) and records them under
// "oh-<asset>-<did>", listed in "ohl" ("<asset>|<did>" pairs). sendHome with
// owed=1 sends exactly that amount back to the address on Ethereum, and anyone
// may call it; an ETH send carries the gas_limit the address needs (estimated
// here, +20%). This keeper
// calls it, as the BTC bot's keeper does for a Bitcoin refund
// (cmd/mapping-bot/mapper/send_home.go): THORChain's refundTx, a failed swap
// goes back to its sender on its own chain. A plain deposit is refundable but
// not owed, and stays where its sender put it.
//
// Anyone can force a refund (an impossible min_amount_out), and every sendHome
// spends this bot's RC, which its maps and confirmations need too. So the
// keeper runs after them, a few calls a tick, waits while the bot's RC is below
// sendHomeRcReserve, and dry-runs each call first: one the bridge refuses now
// (every withdrawal slot in flight, a stale L1 header, the gas reserve short,
// the amount below the bridge's minimum withdrawal) is not sent, costs no RC
// and is tried again later, less often each time. Nothing is lost when the bot
// misses one: the record stays on the bridge until it is sent.

const (
	sendHomeEvery     = time.Minute
	sendHomePerTick   = 3
	sendHomeRcReserve = 30_000
	// sendHomeResend: a sent call's result is awaited this long before the
	// entry (still listed) is tried again.
	sendHomeResend = 10 * time.Minute
	// sendHomeBackoffMax bounds the wait after refused dry runs; an entry
	// refused for sendHomeAlertAfter is logged as an error for an operator.
	sendHomeBackoffMax = time.Hour
	sendHomeAlertAfter = 24 * time.Hour
)

type sendHomeEntry struct {
	nextTry   time.Time
	backoff   time.Duration
	refusedAt time.Time // first refused dry run of the current streak (zero: none)
	alerted   bool
}

var (
	lastSendHomeAt time.Time
	sendHomeState  = map[string]*sendHomeEntry{}
)

// owedHomeCall is the sendHome payload for an "ohl" entry, or "" when the entry
// is not an Ethereum account's.
func owedHomeCall(entry string) string {
	asset, did, ok := strings.Cut(entry, "|")
	if !ok || asset == "" || !strings.HasPrefix(did, "did:pkh:eip155:") {
		return ""
	}
	addr := did[strings.LastIndex(did, ":")+1:]
	if len(addr) != 42 || !strings.HasPrefix(addr, "0x") {
		return ""
	}
	return "address=" + addr + "&asset=" + asset + "&owed=1"
}

// owedGasEstimator estimates the gas an ETH transfer from the vault to `to`
// takes on Ethereum now (nil: none, the bridge's default limit is used).
type owedGasEstimator func(ctx context.Context, to string) (uint64, error)

// Mirrors of account-mapping constants.ETHTransferGas / MaxPayoutGasLimit.
const (
	owedTransferGas = 21_000
	owedMaxGasLimit = 300_000
)

// owedGasLimit is an owed ETH send's gas_limit from an estimate: +20% (as
// Altera sizes a payout), at least a transfer, at most the payout bound. A
// contract wallet that needs more than a plain transfer is covered, so its
// refund does not revert on gas (each revert costs the refund a fee, and two
// take it off the list).
func owedGasLimit(estimate uint64) uint64 {
	return min(max((estimate*12+9)/10, owedTransferGas), owedMaxGasLimit)
}

// sendHomeOwed calls sendHome for the bridge's owed-home entries.
func sendHomeOwed(ctx context.Context, gql *vscGraphQL, submitter *l2Submitter, cfg EVMBotConfig, estimate owedGasEstimator) {
	if cfg.Paused || time.Since(lastSendHomeAt) < sendHomeEvery {
		return
	}
	lastSendHomeAt = time.Now()
	st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"ohl"})
	if err != nil {
		slog.Warn("sendHome keeper: could not read the owed-home list; trying again next tick", "err", err)
		return
	}
	listed := map[string]bool{}
	if st["ohl"] != "" {
		for _, e := range strings.Split(st["ohl"], ",") {
			listed[e] = true
		}
	}
	for e := range sendHomeState {
		if !listed[e] {
			delete(sendHomeState, e) // sent home (or sent by someone else)
		}
	}
	if len(listed) == 0 {
		return
	}
	did := submitter.did.String()
	if rc, known, rcErr := gql.fetchAccountRC(ctx, did); rcErr == nil && known && rc < sendHomeRcReserve {
		slog.Warn("sendHome keeper: owed refunds wait, the bot's RC is below the keeper's reserve",
			"owed", len(listed), "rc", rc, "reserve", sendHomeRcReserve)
		return
	}
	entries := make([]string, 0, len(listed))
	for e := range listed {
		entries = append(entries, e)
	}
	sort.Strings(entries)
	calls := 0
	now := time.Now()
	for _, e := range entries {
		if calls >= sendHomePerTick || ctx.Err() != nil {
			return
		}
		payload := owedHomeCall(e)
		if payload == "" {
			continue
		}
		s := sendHomeState[e]
		if s == nil {
			s = &sendHomeEntry{}
			sendHomeState[e] = s
		}
		if now.Before(s.nextTry) {
			continue
		}
		// Estimated only for an entry about to be tried (not every listed one
		// every tick): the RPC also serves deposits and confirmations.
		if estimate != nil && strings.HasPrefix(e, "eth|") {
			addr := e[strings.LastIndex(e, ":")+1:]
			if g, err := estimate(ctx, addr); err == nil && g > 0 {
				payload += "&gas_limit=" + strconv.FormatUint(owedGasLimit(g), 10)
			} else if err != nil {
				slog.Warn("sendHome keeper: gas estimate failed; the bridge's default gas limit is used", "entry", e, "err", err)
			}
		}
		raw, _ := json.Marshal(payload)
		sim, simErr := gql.simulateContractCall(ctx, did, cfg.ContractID, "sendHome", string(raw))
		if simErr == nil && !sim.Success && sim.Err != "gas_limit_hit" {
			if s.refusedAt.IsZero() {
				s.refusedAt = now
			}
			s.backoff = min(max(2*s.backoff, sendHomeEvery), sendHomeBackoffMax)
			s.nextTry = now.Add(s.backoff)
			if !s.alerted && now.Sub(s.refusedAt) >= sendHomeAlertAfter {
				s.alerted = true
				slog.Error("an owed refund could not go home for a day; check it (sendHome owed=1)",
					"entry", e, "err", sim.Err, "reason", sim.ErrMsg)
			} else {
				slog.Warn("sendHome cannot go out yet; trying again later",
					"entry", e, "err", sim.Err, "reason", sim.ErrMsg, "next", s.backoff)
			}
			continue
		}
		tx, err := submitter.callContractL2(ctx, cfg.ContractID, "sendHome", raw, 0)
		calls++
		if err != nil {
			slog.Warn("sendHome did not go out; trying again later", "entry", e, "err", err)
			s.nextTry = now.Add(sendHomeEvery)
			continue
		}
		s.refusedAt, s.backoff, s.alerted = time.Time{}, 0, false
		s.nextTry = now.Add(sendHomeResend)
		slog.Info("owed refund sent home on Ethereum", "entry", e, "tx", tx)
	}
}
