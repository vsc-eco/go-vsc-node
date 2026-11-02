//go:build evm_devnet

package devnet

// 11-02 parity batch, devnet proof of the EVM keysign halt (N2 + C1/C2 + B6):
// the three ways the EVM bridge's keysigns stop, each while the BTC bridge
// keeps signing, and each lifted again.
//
//   A. governance: vsc.tss_halt {"keyId":"evm"} (0.13) -> evm_keysign_halted
//      on every node.
//   B. theft: the vault's L1 nonce moves past every nonce the bridge
//      authorized (anvil_setNonce stands in for a stolen key); the bot's
//      reporter (B6) proves it (reportUnauthorizedSpend), the bridge sets
//      "th" and every node refuses EVM signs; the owner clears it.
//   C. custody: the vault's L1 balance falls below the bridge's ETH supply by
//      more than the gap (anvil_setBalance); the bot proves custody, the
//      bridge sets "si"; a later full proof lifts it.
//
// Run with REPORTER_EVERY=1m (the bot inherits the test's environment).

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"go.mongodb.org/mongo-driver/bson"
)

// countEvmHaltFlag is countHaltFlag for the EVM governance halt (N2).
func countEvmHaltFlag(t *testing.T, ctx context.Context, d *Devnet, nodes []int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	n := 0
	for _, node := range nodes {
		var doc struct {
			Halted bool `bson:"evm_keysign_halted"`
		}
		if err := client.Database(d.nodeDbName(node)).Collection("chain_consensus_state").
			FindOne(ctx, bson.M{"_id": "singleton"}).Decode(&doc); err != nil {
			continue
		}
		if doc.Halted {
			n++
		}
	}
	return n
}

// unpaidWhileHalted: a withdrawal requested during a halt stays unpaid for
// `hold`; returns its recipient (paid once the halt lifts).
func (b *bothEnv) unpaidWhileHalted(what string, hold time.Duration) ethcommon.Address {
	b.t.Helper()
	recip := b.evmWithdraw(bbEth(1))
	if err := b.botPoll(hold, func() bool { return b.l1Balance(recip).Sign() > 0 }); err == nil {
		b.t.Fatalf("%s: an EVM withdrawal paid out during the halt\nbot:\n%s", what, b.botLogs())
	}
	return recip
}

func TestBothBridges_FS14_EvmHaltsBtcKeepsSigning(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	d := b.d
	start := devnetNow()

	// ---- A. governance halt ----
	govHalt := func(active bool) int {
		payload, _ := json.Marshal(map[string]any{"active": active, "keyId": "evm"})
		want := 0
		if active {
			want = d.cfg.Nodes
		}
		got := -1
		for deadline := devnetNow().Add(15 * time.Minute); devnetNow().Before(deadline); devnetSleep(15 * time.Second) {
			if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), devnetGatewayKeys(t, d)); err != nil {
				b.note("tss_halt evm(active=%v) broadcast: %s", active, firstLine(err.Error()))
				continue
			}
			if got = countEvmHaltFlag(t, b.ctx, d, vfAllNodes(d.cfg.Nodes)); got == want {
				break
			}
		}
		return got
	}
	if n := govHalt(true); n != d.cfg.Nodes {
		t.Fatalf("A: EVM keysign halt not on every node (%d/%d)", n, d.cfg.Nodes)
	}
	if n := countHaltFlag(t, b.ctx, d, vfAllNodes(d.cfg.Nodes)); n != 0 {
		t.Fatalf("A: keyId evm also halted BTC on %d nodes (0.13 must read it as the EVM halt)", n)
	}
	b.mark("A: EVM halted (governance)", start)
	recipA := b.unpaidWhileHalted("A", 8*time.Minute)
	if st := b.btcWithdraw(0, 100_000); st == "" {
		t.Fatalf("A: a BTC withdrawal did not settle while only EVM was halted\n%s", strings.Join(b.notes, "\n"))
	}
	b.mark("A: BTC withdrawal settled during the EVM halt", start)
	if n := govHalt(false); n != 0 {
		t.Fatalf("A: EVM halt not lifted on every node (%d still halted)", n)
	}
	if err := b.botPoll(30*time.Minute, func() bool { return b.l1Balance(recipA).Sign() > 0 }); err != nil {
		t.Fatalf("A: the withdrawal held by the halt never paid after the lift\nbot:\n%s", b.botLogs())
	}
	b.mark("A: held withdrawal paid after the lift", start)

	// ---- C. custody shortfall (before B: B moves the vault nonce) ----
	vault := b.vault0
	keep := b.l1Balance(vault)
	if err := b.anvil.SetBalance(b.ctx, vault, big.NewInt(0)); err != nil {
		t.Fatal(err)
	}
	if err := b.botPoll(20*time.Minute, func() bool { return b.e.stateStr("si") != "" }); err != nil {
		t.Fatalf("C: the custody shortfall never set \"si\" (bot reporter, REPORTER_EVERY?)\nbot:\n%s", b.botLogs())
	}
	b.mark("C: custody halt set by the bot's proof", start)
	recipC := b.unpaidWhileHalted("C", 6*time.Minute)
	if err := b.anvil.SetBalance(b.ctx, vault, keep); err != nil {
		t.Fatal(err)
	}
	if err := b.botPoll(20*time.Minute, func() bool { return b.e.stateStr("si") == "" }); err != nil {
		t.Fatalf("C: a full custody proof never lifted \"si\"\nbot:\n%s", b.botLogs())
	}
	if err := b.botPoll(30*time.Minute, func() bool { return b.l1Balance(recipC).Sign() > 0 }); err != nil {
		t.Fatalf("C: the withdrawal held by the custody halt never paid\nbot:\n%s", b.botLogs())
	}
	b.mark("C: custody halt lifted, held withdrawal paid", start)

	// ---- B. theft ----
	n0, err := b.anvil.Nonce(b.ctx, vault)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.anvil.SetNonce(b.ctx, vault, n0+3); err != nil {
		t.Fatal(err)
	}
	if err := b.botPoll(20*time.Minute, func() bool { return b.e.stateStr("th") != "" }); err != nil {
		t.Fatalf("B: the moved vault nonce was never reported (\"th\" unset)\nbot:\n%s", b.botLogs())
	}
	b.mark("B: theft halt set by the bot's report", start)
	b.unpaidWhileHalted("B", 6*time.Minute)
	if st := b.btcWithdraw(0, 100_000); st == "" {
		t.Fatalf("B: a BTC withdrawal did not settle during the EVM theft halt\n%s", strings.Join(b.notes, "\n"))
	}
	b.mark("B: BTC withdrawal settled during the EVM theft halt", start)
	// The owner clears it (here after "rotating": the nonce is put back so the
	// bridge's own nonces are valid again on this devnet).
	if err := b.anvil.SetNonce(b.ctx, vault, n0); err != nil {
		t.Fatal(err)
	}
	if err := b.e.proposeExecute("clearTheftHalt", "{}"); err != nil {
		t.Fatalf("B: clearTheftHalt: %v", err)
	}
	if b.e.stateStr("th") != "" {
		t.Fatal("B: clearTheftHalt left \"th\" set")
	}
	b.mark("B: theft halt cleared by the owner", start)
}
