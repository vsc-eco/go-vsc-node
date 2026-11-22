package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// AUTO-ROLL: the bot rolls the head withdrawal only when it cannot mine (past
// the bridge's window, not mined, fee cap below today's base fee), after a dry
// run; never one that mined, could still mine, or that the bridge refuses.

const arVault = "0x00000000000000000000000000000000000000Aa"

// arUnsigned is an unsigned EIP-1559 transfer as the bridge builds it
// (BuildETHWithdrawalTx): 0x02 || rlp(chainId, nonce, tip, feeCap, gas, to,
// value, data, accessList).
func arUnsigned(t *testing.T, nonce uint64, feeCap int64) string {
	t.Helper()
	to := common.HexToAddress("0x00000000000000000000000000000000000000B1")
	body, err := gethrlp.EncodeToBytes([]any{big.NewInt(1), nonce, big.NewInt(2e9), big.NewInt(feeCap), uint64(25_200), &to, big.NewInt(1e16), []byte{}, types.AccessList{}})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(append([]byte{types.DynamicFeeTxType}, body...))
}

type arChain struct {
	mined   uint64
	base    int64
	nonceEr error
}

func (c *arChain) l1() *headL1 {
	return &headL1{
		nonce: func(context.Context, string) (uint64, error) { return c.mined, c.nonceEr },
		baseFee: func(context.Context) (*big.Int, error) {
			return big.NewInt(c.base), nil
		},
	}
}

// arWorld: head nonce 5 queued at L1 block 1,000, fee cap 10 gwei; the bridge's
// verified L1 tip is past the 1,200-block window; Ethereum's base fee is 30
// gwei and the vault has mined nonces up to 4.
func arWorld(t *testing.T) (*fakeL2, *vscGraphQL, *l2Submitter, EVMBotConfig, *arChain) {
	t.Helper()
	l2, gql, sub, cfg := shWorld(t)
	lastAutoRollAt = time.Time{}
	ps := fmt.Sprintf(`{"nonce":5,"amount":10000000000000000,"from":"contract:vsc1router","to":"0x00000000000000000000000000000000000000b1","asset":"eth","unsigned_tx_hex":"%s","block_height":1000,"vault_at_queue":"%s","payout":true}`,
		arUnsigned(t, 5, 10e9), arVault)
	l2.set("vsc1bridge", "n", "5")
	l2.set("vsc1bridge", "np", "6")
	l2.set("vsc1bridge", "d-5", ps)
	l2.set("vsc1bridge", "h", "2300")
	return l2, gql, sub, cfg, &arChain{mined: 5, base: 30e9}
}

func arRolls(t *testing.T, l2 *fakeL2) []string {
	t.Helper()
	var out []string
	for _, c := range submittedCalls(t, l2) {
		if c.Action == "rollWithdrawal" {
			out = append(out, c.Payload)
		}
	}
	return out
}

func TestAutoRoll_RollsAPricedOutHeadPastTheWindow(t *testing.T) {
	l2, gql, sub, cfg, ch := arWorld(t)
	rollStuckHead(context.Background(), gql, sub, cfg, ch.l1())
	if got := arRolls(t, l2); len(got) != 1 || got[0] != `{"nonce":5}` {
		t.Fatalf("rollWithdrawal calls %q, want one {\"nonce\":5}", got)
	}
	// Within a minute nothing is tried again (the next roll is the bridge's
	// cooldown's call anyway).
	rollStuckHead(context.Background(), gql, sub, cfg, ch.l1())
	if n := len(arRolls(t, l2)); n != 1 {
		t.Fatalf("%d rolls after a second tick within a minute, want 1", n)
	}
}

func TestAutoRoll_NeverRollsWhatCanStillMineOrHasMined(t *testing.T) {
	cases := []struct {
		name string
		edit func(l2 *fakeL2, ch *arChain)
	}{
		{"inside the roll window", func(l2 *fakeL2, ch *arChain) { l2.set("vsc1bridge", "h", "2199") }},
		{"already mined (the vault's nonce is past it)", func(l2 *fakeL2, ch *arChain) { ch.mined = 6 }},
		{"its fee cap covers today's base fee", func(l2 *fakeL2, ch *arChain) { ch.base = 10e9 }},
		{"nothing in flight", func(l2 *fakeL2, ch *arChain) { l2.set("vsc1bridge", "np", "5") }},
		{"the vault's nonce cannot be read", func(l2 *fakeL2, ch *arChain) { ch.nonceEr = errors.New("rpc down") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l2, gql, sub, cfg, ch := arWorld(t)
			c.edit(l2, ch)
			rollStuckHead(context.Background(), gql, sub, cfg, ch.l1())
			if got := arRolls(t, l2); len(got) != 0 {
				t.Fatalf("rolled %q, want no roll", got)
			}
		})
	}
}

func TestAutoRoll_RefusedDryRunPausedOrNoRPCSendsNothing(t *testing.T) {
	l2, gql, sub, cfg, ch := arWorld(t)
	l2.sim = func(action string) map[string]any {
		if action == "rollWithdrawal" {
			return map[string]any{"success": false, "err": "bad_input", "err_msg": "rollWithdrawal: cooldown not elapsed since the last re-sign"}
		}
		return map[string]any{"success": true, "rc_used": 6000}
	}
	rollStuckHead(context.Background(), gql, sub, cfg, ch.l1())
	if got := arRolls(t, l2); len(got) != 0 {
		t.Fatalf("rolled %q after a refused dry run", got)
	}

	l2, gql, sub, cfg, ch = arWorld(t)
	cfg.Paused = true
	rollStuckHead(context.Background(), gql, sub, cfg, ch.l1())
	rollStuckHead(context.Background(), gql, sub, cfg, nil) // no L1 RPC
	if got := arRolls(t, l2); len(got) != 0 {
		t.Fatalf("rolled %q while paused or without an L1 RPC", got)
	}
}

func TestUnsignedFeeCap(t *testing.T) {
	if got := unsignedFeeCap(arUnsigned(t, 5, 12e9)); got == nil || got.Int64() != 12e9 {
		t.Fatalf("fee cap %v, want 12 gwei", got)
	}
	for _, bad := range []string{"", "f86c", strings.Repeat("0", 10)} {
		if got := unsignedFeeCap(bad); got != nil {
			t.Fatalf("fee cap of %q = %v, want nil", bad, got)
		}
	}
}
