package mapper

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/chaincfg"
)

// A deposit swap that does not complete goes back to its sender (refund_to). When
// refund_to is a Bitcoin address, the contract credits that address's own Magi
// account and records the amount as refundable ("rf-<address>"). That account has
// no RC unless it holds HBD, so it cannot move the sats itself. sendHome is
// permissionless and can only send them to that same address, so the bot calls it
// and pays the RC.
//
// The refund lands inside the map, so the bot puts every Bitcoin refund address
// of a map on a watch list and checks it after the block is done. A state read
// can trail the map's confirmation on a lagging node, so an address with no
// record is checked again on the next passes before it is dropped. Once a refund
// is on record the address is kept for about a day: each sendHome is dry-run
// first, and one that cannot go out yet (a fee spike, vault coins held by a
// rotation, a pause) is not broadcast but tried again next pass. A refund still
// waiting after that is logged as an error for an operator.
//
// Anyone can force a refund (an impossible min_amount_out), and every sendHome
// spends this bot's RC, which its maps need too. So the keeper runs after the
// block cursor has moved, on its own time budget, a few calls per pass, and waits
// while the bot's RC is below sendHomeRcReserve: a burst of forced refunds slows
// the refunds, never the deposits. Nothing is lost if the bot misses one (a
// restart clears the list): the record stays and anyone can call sendHome.

// refundWatchPasses is how many passes an address with no refund record is
// checked before it is dropped (a pass per Bitcoin block).
const refundWatchPasses = 3

// refundRetryPasses is how many passes a refund on record is tried before the
// keeper leaves it to an operator: about a day of Bitcoin blocks.
const refundRetryPasses = 144

// sendHomeDustSats mirrors the contract's dustThreshold (btc-mapping-contract
// mapping/unmapping.go): it refuses to send this little, so a record this small
// (the fee-estimate surplus a sendHome leaves behind) is not worth a call.
const sendHomeDustSats = 546

// sendHomeRcReserve is the RC the keeper leaves for the bot's own work: a vault
// rotation costs ~28,000 RC across its ops (VR2-04) and each map up to the
// configured rc_limit (10,000 by default).
const sendHomeRcReserve = 30_000

// sendHomePerPass caps the sendHome calls of one pass, and sendHomePassBudget
// its time; each call waits for its on-chain result.
const (
	sendHomePerPass    = 3
	sendHomePassBudget = 3 * time.Minute
)

type sendHomeOutcome int

const (
	sendHomeDone    sendHomeOutcome = iota // sent home, or refused on chain (kept for /retry)
	sendHomeRecheck                        // not sent this pass: uses up a pass
	sendHomeWait                           // not tried (node unreadable, call cap, bot RC low): no pass used
)

// refundWatchEntry is one watched Bitcoin refund address.
type refundWatchEntry struct {
	passes   int  // passes left before the keeper gives up on it
	recorded bool // a refund was seen on record (passes then reset to refundRetryPasses once)
}

// AddressRefundParams is the chain whose addresses a refund_to may name, or nil
// when only a hive: refund_to is accepted. Only the BTC contract can send a
// refund home on its own chain (sendHome); DASH, LTC, DOGE and BCH hold a swap
// with no hive: refund_to for the owner to release.
func AddressRefundParams(c *chain.ChainConfig) *chaincfg.Params {
	if c == nil || c.Name != "btc" {
		return nil
	}
	return c.ChainParams
}

// refundAddressOf is the Bitcoin address an instruction's refund_to names, or "".
func refundAddressOf(instruction string, b *Bot) string {
	params := AddressRefundParams(b.Chain)
	if params == nil {
		return ""
	}
	q, err := url.ParseQuery(instruction)
	if err != nil {
		return ""
	}
	rt := strings.TrimSpace(q.Get("refund_to"))
	if rt == "" || strings.HasPrefix(rt, "hive:") {
		return ""
	}
	return RefundChainAddress(rt, params)
}

// watchRefundAddress puts one Bitcoin address (a proven sender) on the watch.
func (b *Bot) watchRefundAddress(addr string) {
	b.refundWatchMu.Lock()
	defer b.refundWatchMu.Unlock()
	if b.refundWatch == nil {
		b.refundWatch = map[string]*refundWatchEntry{}
	}
	if _, ok := b.refundWatch[addr]; !ok {
		b.refundWatch[addr] = &refundWatchEntry{passes: refundWatchPasses}
	}
}

// watchRefunds puts the Bitcoin refund addresses of mapped instructions on the watch.
func (b *Bot) watchRefunds(instructions []string) {
	b.refundWatchMu.Lock()
	defer b.refundWatchMu.Unlock()
	for _, instr := range instructions {
		if addr := refundAddressOf(instr, b); addr != "" {
			if b.refundWatch == nil {
				b.refundWatch = map[string]*refundWatchEntry{}
			}
			if _, ok := b.refundWatch[addr]; !ok {
				b.refundWatch[addr] = &refundWatchEntry{passes: refundWatchPasses}
			}
		}
	}
}

// sendHomeRefunds calls sendHome for watched addresses with a refund waiting.
func (b *Bot) sendHomeRefunds(parent context.Context) {
	// One pass at a time: the block loop and a new address's history scan
	// (HandleExistingDeposits, its own goroutine) both end with a pass, and two at
	// once would send the same refund twice (the second fails on chain). A pass
	// that finds one running leaves its addresses to it or to the next.
	if !b.sendHomeMu.TryLock() {
		return
	}
	defer b.sendHomeMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, sendHomePassBudget)
	defer cancel()

	b.refundWatchMu.Lock()
	addrs := make([]string, 0, len(b.refundWatch))
	for a := range b.refundWatch {
		addrs = append(addrs, a)
	}
	b.refundWatchMu.Unlock()
	sort.Strings(addrs)

	calls := 0
	for _, addr := range addrs {
		if ctx.Err() != nil {
			return
		}
		outcome, called, sats := b.sendHomeOnce(ctx, addr, calls < sendHomePerPass)
		if called {
			calls++
		}
		b.refundWatchMu.Lock()
		e := b.refundWatch[addr]
		if e != nil && sats > sendHomeDustSats && !e.recorded {
			e.recorded, e.passes = true, refundRetryPasses
		}
		switch {
		case e == nil:
		case outcome == sendHomeDone:
			delete(b.refundWatch, addr)
		case outcome == sendHomeRecheck:
			if e.passes--; e.passes <= 0 {
				delete(b.refundWatch, addr)
				if e.recorded {
					b.L.Error("a refund is still waiting after a day of tries; send it home by hand (sendHome with the address)",
						"address", addr, "sats", sats)
				}
			}
		}
		b.refundWatchMu.Unlock()
	}
}

// sendHomeOnce checks addr's refund record and, when one is waiting and mayCall
// allows, calls sendHome for it. called reports whether it broadcast; sats is the
// refund on record (0 when unread).
func (b *Bot) sendHomeOnce(ctx context.Context, addr string, mayCall bool) (outcome sendHomeOutcome, called bool, sats int64) {
	sats, err := b.gql().FetchRefundable(ctx, addr)
	if err != nil {
		b.L.Warn("could not read a refund record; checking again next pass", "address", addr, "err", err)
		return sendHomeWait, false, 0
	}
	if sats <= sendHomeDustSats {
		return sendHomeRecheck, false, sats
	}
	if !mayCall {
		return sendHomeWait, false, sats
	}
	did := b.botEthDID.String()
	if rc, known, rcErr := b.gql().FetchAccountRC(ctx, did); rcErr == nil && known && rc < sendHomeRcReserve {
		b.L.Warn("refund waiting: bot RC is below the keeper's reserve, sending it home later",
			"address", addr, "sats", sats, "rc", rc, "reserve", sendHomeRcReserve)
		return sendHomeWait, false, sats
	}
	// JSON: a failed call's payload is kept for /retry and served as JSON on /health.
	payload, _ := json.Marshal(addr)
	// Dry run first: a sendHome that cannot go out now (the refund does not cover
	// the miner fee at today's rate, vault coins held by a rotation, a pause) is not
	// broadcast, so it costs no RC and leaves nothing for /retry; it is tried again
	// next pass. A node that cannot dry-run does not stop it.
	if sim, simErr := b.gql().SimulateContractCall(ctx, did, b.BotConfig.ContractId(), "sendHome", string(payload), simulateRcCap); simErr == nil && !sim.Success && sim.Err != "gas_limit_hit" {
		b.L.Warn("sendHome cannot go out yet; trying again next pass",
			"address", addr, "sats", sats, "err", sim.Err, "reason", sim.ErrMsg)
		return sendHomeRecheck, false, sats
	}
	if _, err := b.callWithRetry(ctx, payload, "sendHome", broadcastRetryAttempts); err != nil {
		if errors.Is(err, ErrTxFailed) {
			b.L.Error("sendHome refused on chain; the refund stays recorded, retry it from /retry",
				"address", addr, "sats", sats, "err", err)
			return sendHomeDone, true, sats
		}
		b.L.Warn("sendHome did not land; trying again next pass", "address", addr, "sats", sats, "err", err)
		return sendHomeRecheck, true, sats
	}
	b.L.Info("refund sent home on Bitcoin", "address", addr, "sats", sats)
	return sendHomeDone, true, sats
}
