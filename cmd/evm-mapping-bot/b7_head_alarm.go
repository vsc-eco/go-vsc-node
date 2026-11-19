package main

import (
	"context"
	"fmt"
	"strconv"
)

// withdrawalRollWindow mirrors the bridge's WithdrawalRollWindow
// (evm-mapping-contract constants.go): the L1 blocks after a withdrawal was
// queued before anyone may roll it back with rollWithdrawal (~4 h at 12 s).
const withdrawalRollWindow = 1200

// B-7: a withdrawal that never settles (TSS never signed it, nobody broadcast
// it, or it is priced out) holds every later one behind it, and nothing said
// so. The bot does not roll or re-price it itself: rolling one that was about
// to mine turns a payout into a refund, so that stays the operator's call.
// headWithdrawalAlarm only reports it: the head withdrawal (nonce "n") that has
// not settled withdrawalRollWindow L1 blocks after it was queued, with what can
// move it. It reads state and sends nothing. "" means no alarm.
func headWithdrawalAlarm(ctx context.Context, gql *vscGraphQL, contractID string) string {
	st, err := gql.fetchContractState(ctx, contractID, []string{"n", "np"})
	if err != nil {
		return "B-7: cannot read the bridge's withdrawal nonces: " + err.Error()
	}
	n, _ := strconv.ParseUint(st["n"], 10, 64)
	np, _ := strconv.ParseUint(st["np"], 10, 64)
	if np <= n {
		return "" // no withdrawal in flight
	}
	key := "d-" + strconv.FormatUint(n, 10)
	ds, err := gql.fetchContractState(ctx, contractID, []string{key})
	if err != nil {
		return "B-7: cannot read the head withdrawal " + key + ": " + err.Error()
	}
	ps := parsePendingSpend(n, ds[key])
	if ps == nil {
		// Settled between the two reads (n moved on), or a head record that cannot
		// be read: the bot cannot broadcast or confirm it either, so that is an
		// alarm of its own rather than silence.
		again, err := gql.fetchContractState(ctx, contractID, []string{"n"})
		if err != nil || again["n"] != st["n"] {
			return ""
		}
		return fmt.Sprintf("B-7: the head withdrawal nonce %d has no readable record (%s = %q): the bot can neither broadcast nor confirm it, and every later withdrawal waits behind it", n, key, ds[key])
	}
	tip, err := fetchContractLastHeight(ctx, gql, contractID)
	if err != nil {
		return "B-7: cannot read the verified L1 height: " + err.Error()
	}
	if tip < ps.BlockHeight+withdrawalRollWindow {
		return ""
	}
	return fmt.Sprintf("B-7: withdrawal nonce %d (%s %s to %s) has not settled %d L1 blocks after it was queued at block %d, and every later withdrawal waits behind it: check its TSS signature, its broadcast and its fee; it can be re-priced (replaceWithdrawal) or rolled back to a refund (rollWithdrawal, permissionless now)",
		n, ps.Amount, ps.Asset, ps.To, tip-ps.BlockHeight, ps.BlockHeight)
}
