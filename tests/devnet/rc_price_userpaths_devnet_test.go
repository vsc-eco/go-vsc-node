//go:build evm_devnet

package devnet

// The user's real path at the two write prices, on a devnet with the real BTC
// bridge contract, the DEX router and its pools: a swap paid out to a Bitcoin
// address under the 10,000 RC a wallet declares (the free allowance of a Hive
// account) fails before the floor reaches consensus 0.15.0 and passes after.
//
// Needs BTC_MAPPING_WASM_PATH, DEX_WASM, DEX_ROUTER_WASM and the EVM artifacts
// (the environment brings both bridges up), like the other DEX3 tests.
//
//	go test -tags evm_devnet -run TestRcPrice_UserPaths ./tests/devnet/ -timeout 330m -v

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRcPrice_UserPaths(t *testing.T) {
	bbSkipShort(t)
	x := newBothEnvDEX3(t, 320*time.Minute)
	b := x.b
	d, ctx := b.d, b.ctx
	x.setup()

	const trader = "magi.test1" // dex3Trader without the prefix
	const walletLimit = 10_000  // what a wallet declares; a Hive account's free RC
	newAddr := func() string {
		a, err := d.bitcoinCli(ctx, "getnewaddress")
		if err != nil {
			t.Fatalf("getnewaddress: %v", err)
		}
		return strings.TrimSpace(a)
	}
	// 5 HBD into the BTC/HBD pool (0.01 BTC against 50 HBD): about 90,000 sats out.
	payout := func(to string) string {
		return fmt.Sprintf(`{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"btc","amount_in":"5000","recipient":"%s","destination_chain":"BTC","max_fee":50000}`, to)
	}
	onMagi := `{"type":"swap","version":"1.0.0","asset_in":"hbd","asset_out":"btc","amount_in":"2000","recipient":"hive:` + trader + `"}`
	received := func(addr string) string {
		got, err := d.bitcoinCli(ctx, "getreceivedbyaddress", addr, "0")
		if err != nil {
			t.Fatalf("getreceivedbyaddress: %v", err)
		}
		return strings.TrimSpace(got)
	}
	must := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.Fatalf("%s failed; the cases after it depend on it\n%s", name, strings.Join(b.notes, "\n"))
		}
	}

	var before struct{ payout, onMagi int64 }

	must("before/01_the_chain_is_on_0.14", func(t *testing.T) {
		if err := pollAllNodes(d.cfg.Nodes, 8*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.14"
		}); err != nil {
			line, running, _, _ := d.ConsensusInfo(ctx, 1)
			t.Fatalf("want active line 0.14, got line=%q running=%q: %v (run with DEVNET_FLOOR_CONSENSUS=14)", line, running, err)
		}
	})

	must("before/02_a_payout_swap_under_the_wallet_limit_fails", func(t *testing.T) {
		to := newAddr()
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", payout(to), walletLimit, hbdAllow(5000))
		if o.Status != "FAILED" {
			t.Fatalf("a swap paid out to a Bitcoin address under rc_limit %d: status %s, want FAILED at 19 RC a new byte", walletLimit, o.Status)
		}
		if got := received(to); got != "0.00000000" {
			t.Fatalf("the failed swap still paid %s BTC", got)
		}
		b.note("RC price: before the switch a payout swap under rc_limit %d FAILED and was charged %d RC", walletLimit, o.Used)
	})

	must("before/03_the_same_swap_with_room_passes_and_costs_over_10000", func(t *testing.T) {
		to := newAddr()
		spends := txSpendIds(t, d, ctx, b.v.cid)
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", payout(to), 60_000, hbdAllow(5000))
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if o.Used <= walletLimit {
			t.Fatalf("the payout swap cost %d RC; it is expected to cost more than the %d a wallet declares", o.Used, walletLimit)
		}
		before.payout = o.Used
		if st := x.btcCompletePending(spends, b.btcKeyID(0)); st == "" {
			t.Fatalf("the payout was never signed and confirmed\n%s", strings.Join(b.notes, "\n"))
		}
		if got := received(to); got == "0.00000000" {
			t.Fatalf("the payout never reached %s", to)
		}
		b.note("RC price: before the switch a payout swap cost %d RC", o.Used)
	})

	must("before/04_a_swap_kept_on_magi", func(t *testing.T) {
		// The first swap of a route creates its bookkeeping entries; the one
		// measured is the second, which only rewrites them.
		if first := rcpMeasure(t, d, ctx, trader, x.router, "execute", onMagi, 60_000, hbdAllow(2000)); first.Status != "CONFIRMED" {
			t.Fatalf("the first swap kept on Magi: status %s", first.Status)
		}
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", onMagi, walletLimit, hbdAllow(2000))
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		before.onMagi = o.Used
	})

	must("switch/05_the_proposal_raises_the_floor_to_0.15", func(t *testing.T) {
		epoch, height := rcpRaiseFloor(t, d, ctx, d.cfg.Nodes, 15, 60*time.Minute)
		t.Logf("epoch %d at height %d carries 0.15", epoch, height)
		if err := pollAllNodes(d.cfg.Nodes, 10*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.15"
		}); err != nil {
			t.Fatalf("not every node adopted 0.15: %v", err)
		}
	})

	t.Run("after/05b_a_swap_kept_on_magi_costs_exactly_what_it_did", func(t *testing.T) {
		// Measured first after the switch, so the state it reads is the state
		// before/04 read: it stores nothing new, so neither rule may change it.
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", onMagi, walletLimit, hbdAllow(2000))
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if o.Used != before.onMagi {
			t.Fatalf("a swap kept on Magi cost %d RC right after the switch, %d right before it", o.Used, before.onMagi)
		}
	})

	var after struct{ payout int64 }
	t.Run("after/06_the_payout_swap_passes_under_the_wallet_limit", func(t *testing.T) {
		to := newAddr()
		spends := txSpendIds(t, d, ctx, b.v.cid)
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", payout(to), walletLimit, hbdAllow(5000))
		if o.Status != "CONFIRMED" {
			t.Fatalf("a swap paid out to a Bitcoin address under rc_limit %d: status %s, want CONFIRMED at 1 RC a new byte", walletLimit, o.Status)
		}
		after.payout = o.Used
		if st := x.btcCompletePending(spends, b.btcKeyID(0)); st == "" {
			t.Fatalf("the payout was never signed and confirmed\n%s", strings.Join(b.notes, "\n"))
		}
		if got := received(to); got == "0.00000000" {
			t.Fatalf("the payout never reached %s", to)
		}
		b.note("RC price: after the switch a payout swap under rc_limit %d cost %d RC and paid %s BTC", walletLimit, o.Used, received(to))
	})

	t.Run("after/07_the_saving_is_the_new_bytes_a_payout_stores", func(t *testing.T) {
		// A payout stores a few hundred new bytes (the signing record, the
		// settle record, the pending list) and saves 18 RC on each. The two
		// payouts read slightly different pool state, so the saving is a range,
		// not an exact multiple.
		saved := before.payout - after.payout
		if saved < 18*300 || saved > 18*900 {
			t.Fatalf("the payout cost %d before and %d after: saved %d RC, want 18 RC on each of 300 to 900 new bytes (%d to %d)",
				before.payout, after.payout, saved, 18*300, 18*900)
		}
		t.Logf("the payout cost %d RC before and %d after: about %d new bytes", before.payout, after.payout, saved/18)
	})

	t.Run("after/09_a_direct_btc_withdrawal_passes_under_the_wallet_limit", func(t *testing.T) {
		to := newAddr()
		spends := txSpendIds(t, d, ctx, b.v.cid)
		o := rcpMeasure(t, d, ctx, trader, b.v.cid, "unmap", fmt.Sprintf(`{"amount":"30000","to":"%s"}`, to), walletLimit, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("a BTC withdrawal under rc_limit %d: status %s", walletLimit, o.Status)
		}
		if st := x.btcCompletePending(spends, b.btcKeyID(0)); st == "" {
			t.Fatalf("the withdrawal was never signed and confirmed\n%s", strings.Join(b.notes, "\n"))
		}
		if got := received(to); got == "0.00000000" {
			t.Fatalf("the withdrawal never reached %s", to)
		}
		b.note("RC price: after the switch a direct BTC withdrawal cost %d RC", o.Used)
	})

	t.Run("after/10_a_payout_that_runs_out_is_charged_its_limit", func(t *testing.T) {
		const limit = 1500 // under what a payout costs at either price
		to := newAddr()
		o := rcpMeasure(t, d, ctx, trader, x.router, "execute", payout(to), limit, hbdAllow(5000))
		if o.Status != "FAILED" || o.Used != limit {
			t.Fatalf("status %s, charged %d RC, want FAILED and exactly the limit %d", o.Status, o.Used, limit)
		}
		if got := received(to); got != "0.00000000" {
			t.Fatalf("the failed swap still paid %s BTC", got)
		}
	})

	t.Run("after/11_every_node_froze_the_same_rc_for_the_trader_within_one_percent", func(t *testing.T) {
		// Not record for record: this environment restarts every node while the
		// trader is active, and a node that stops in the middle of a slot loses
		// the RC of that slot (it is kept in memory until the slot ends) or
		// files it one slot later. That is the same before and after 0.15; what
		// the two rules must not do is make nodes drift apart.
		head := nodeBlock(t, d, ctx, rcpRefNode)
		var ref int64
		for n := 1; n <= d.cfg.Nodes; n++ {
			if err := d.WaitForBlockProcessing(ctx, n, head, 5*time.Minute); err != nil {
				t.Fatalf("magi-%d did not reach block %d: %v", n, head, err)
			}
		}
		rec, ok, err := rcpLatest(ctx, d, rcpRefNode, "hive:"+trader)
		if err != nil || !ok {
			t.Fatalf("magi-%d record: %v", rcpRefNode, err)
		}
		ref = rec.Amount
		for n := 1; n <= d.cfg.Nodes; n++ {
			got, ok, err := rcpLatest(ctx, d, n, "hive:"+trader)
			if err != nil || !ok {
				t.Fatalf("magi-%d record: %v", n, err)
			}
			diff := got.Amount - ref
			if diff < 0 {
				diff = -diff
			}
			t.Logf("magi-%d: %d RC frozen at %d (magi-%d: %d)", n, got.Amount, got.Height, rcpRefNode, ref)
			if diff*100 > ref {
				t.Errorf("magi-%d froze %d RC for the trader, magi-%d froze %d: more than 1%% apart", n, got.Amount, rcpRefNode, ref)
			}
		}
	})
}
