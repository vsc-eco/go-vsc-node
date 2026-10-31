//go:build evm_devnet

// The BOTH tests (/mnt/o/BTC-EVM-INTERACTION-2026-10-31/PLAN.md): the BTC vault
// and the EVM bridge's vault together on one devnet, under instability, with
// the failure states that so far ran for one bridge at a time. Harness and the
// full cycle: evm_btc_both_env_test.go. Each test prints TIMING, METER and NOTE
// lines; a test fails when a rotation, migration or withdrawal on EITHER bridge
// does not complete, or L2 stops.
package devnet

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
)

func bbSkipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("devnet")
	}
}

// BB-1: both vaults rotate, migrate and serve withdrawals in the same window,
// clean weather. The reference timings for every other BOTH test.
func TestBothBridges_BB1_FullCycle(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 240*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{})
}

// BB-2: the same cycle under mainnet-like instability (BB_WEATHER, default mid:
// flaky L1 RPC, 300 ms inter-node latency, a slow Hive API on one node and a
// cut one on another, two witness outages, an ETH fee spike with a bot restart,
// reorgs of the ETH and the BTC sweep, a 10 min late prover, 30 withdrawals an
// hour on each bridge). Run at low, mid and high to get the range.
func TestBothBridges_BB2_FullCycleWeather(t *testing.T) {
	bbSkipShort(t)
	w := bbWeatherLevel("mid")
	b := newBothEnv(t, 300*time.Minute, w, true)
	b.runCycle(cycleHooks{})
}

// FS1 (F1 x both): quorum lost while BOTH keygens run (two of five nodes down
// for 4 min). Both rotations must finish after quorum returns: no indefinite stall.
func TestBothBridges_FS1_KeygenQuorumLoss(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{afterCreateKeys: func(start time.Time) {
		for _, n := range []int{4, 5} {
			n := n
			b.at(start, 20*time.Second, fmt.Sprintf("magi-%d down 4 min (quorum lost)", n), func() { b.outage(n, 4*time.Minute) })
		}
	}})
}

// FS2 (F2/F16 x both): L2 halts (two nodes down) while BOTH migrations are in
// flight; both must settle after it resumes.
func TestBothBridges_FS2_HaltMidBothMigrations(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{migrating: func() {
		now := devnetNow()
		for _, n := range []int{4, 5} {
			n := n
			b.at(now, 45*time.Second, fmt.Sprintf("magi-%d down 3 min (L2 halted mid-migrations)", n), func() { b.outage(n, 3*time.Minute) })
		}
	}})
}

// FS3 (F3/F4 x both, 18t): the whole fleet restarts twice: while both keygens
// run and while both migrations run.
func TestBothBridges_FS3_FleetRestartMidBoth(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	restart := func(why string) func() {
		return func() {
			if err := b.d.RestartAllMagiNodes(b.ctx); err != nil {
				t.Errorf("fleet restart (%s): %v", why, err)
			}
		}
	}
	b.runCycle(cycleHooks{
		afterCreateKeys: func(start time.Time) { b.at(start, 2*time.Minute, "fleet restart (keygens running)", restart("keygens")) },
		migrating:       func() { b.at(devnetNow(), time.Minute, "fleet restart (migrations running)", restart("migrations")) },
	})
}

// FS4 (I5): the BTC keysign halt (vsc.tss_halt via the gateway multisig) stops
// BTC withdrawals; the EVM bridge must keep signing and paying out, and BTC
// must resume after the halt is lifted.
func TestBothBridges_FS4_BtcHaltEvmKeepsSigning(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 200*time.Minute, bbWeatherLevel("none"), true)
	d := b.d
	var keys = devnetGatewayKeys(t, d)
	halt := func(active bool) int {
		payload, _ := json.Marshal(map[string]any{"active": active, "keyId": b.btcKeyID(0)})
		want := 0
		if active {
			want = d.cfg.Nodes
		}
		got := -1
		for i := 0; i < 20; i++ {
			if _, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), keys); err == nil {
				if got = countHaltFlag(t, b.ctx, d, vfAllNodes(d.cfg.Nodes)); got == want {
					break
				}
			}
			devnetSleep(15 * time.Second)
		}
		return got
	}
	start := devnetNow()
	if n := halt(true); n != d.cfg.Nodes {
		t.Fatalf("BTC keysign halt not on every node (%d/%d)", n, d.cfg.Nodes)
	}
	b.mark("BTC halted", start)
	recip := b.evmWithdraw(bbEth(1))
	if err := pollUntil(b.ctx, 30*time.Minute, func() bool { return b.l1Balance(recip).Sign() > 0 }); err != nil {
		t.Fatalf("an EVM withdrawal did not pay out while only BTC was halted (I5)\nbot:\n%s", b.botLogs())
	}
	b.mark("EVM withdrawal paid while BTC halted", start)
	if n := halt(false); n != 0 {
		t.Fatalf("BTC keysign halt not lifted on every node (%d still halted)", n)
	}
	if st := b.btcWithdraw(0, 100_000); st == "" {
		t.Fatalf("BTC withdrawals did not resume after the halt was lifted\n%s", strings.Join(b.notes, "\n"))
	}
	b.mark("BTC withdrawal settled after the halt", start)
}

func devnetGatewayKeys(t *testing.T, d *Devnet) []*hivego.KeyPair {
	var ks []*hivego.KeyPair
	for n := 1; n <= d.cfg.Nodes; n++ {
		ks = append(ks, devnetGatewayKeypair(t, fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n)))
	}
	return ks[:4]
}

// FS5 (F23 x both, I14): the EVM bridge is paused through the whole BTC
// rotation and migration; BTC must not notice. Then EVM is unpaused and its
// rotation completes.
func TestBothBridges_FS5_PauseOneBridge(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	if err := b.e.proposeExecute("pause", "{}"); err != nil {
		t.Fatalf("EVM pause: %v", err)
	}
	start := devnetNow()
	p1, ok := vfRotate(t, b.d, b.ctx, b.v.cid, 1)
	if !ok {
		t.Fatal("BTC rotation failed with the EVM bridge paused")
	}
	if st := b.btcMigrate(0, p1); st == "" {
		t.Fatalf("BTC migration failed with the EVM bridge paused\n%s", strings.Join(b.notes, "\n"))
	}
	b.mark("BTC rotation + migration with EVM paused", start)
	if err := b.e.proposeExecute("unpause", "{}"); err != nil {
		t.Fatalf("EVM unpause: %v", err)
	}
	if ok, msg := b.e.ownerCall("createKey", "{}"); !ok {
		t.Fatalf("EVM createKey after unpause: %s", msg)
	}
	if err := pollUntil(b.ctx, 45*time.Minute, func() bool { return b.e.keyPub(evmKeyName(1)) != "" }); err != nil {
		t.Fatalf("EVM keygen after unpause: %v", err)
	}
	if err := pollUntil(b.ctx, 60*time.Minute, func() bool { ok, _ := b.e.ownerCall("activateKey", "{}"); return ok }); err != nil {
		t.Fatalf("EVM activation after unpause: %v\nbot:\n%s", err, b.botLogs())
	}
	b.mark("EVM rotation after unpause", start)
}

// FS6 (F20 x both): one node partitioned from the rest while both migrations sign.
func TestBothBridges_FS6_PartitionDuringBothMigrations(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{migrating: func() {
		b.at(devnetNow(), 30*time.Second, "magi-5 partitioned 4 min", func() {
			for n := 1; n <= 4; n++ {
				_ = b.d.Partition(b.ctx, 5, n)
			}
			devnetSleep(4 * time.Minute)
			for n := 1; n <= 4; n++ {
				_ = b.d.Heal(b.ctx, 5, n)
			}
		})
	}})
}

// FS7 (F18 x both): a node loses every key share (BTC and EVM, all
// generations) after both rotations; both migrations must still sign
// (threshold) and the node must get its shares back at the next reshare.
func TestBothBridges_FS7_ShareLossBothVaults(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{afterActivate: func() {
		before, after := vfF18DeleteTssKeys(t, b.d, 5)
		b.note("magi-5 key share files %d -> %d", before, after)
		if err := b.d.StopNode(b.ctx, 5); err == nil {
			_ = b.d.StartNode(b.ctx, 5)
		}
	}})
}

// FS8 (I6, I7): a member holding shares of BOTH vaults asks to unstake: held
// while either vault's generation that it holds is funded. Records the verdict
// with both funded, and again once the EVM side is drained (BTC alone must
// still hold it: the lock is the union).
func TestBothBridges_FS8_BondLockUnion(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 200*time.Minute, bbWeatherLevel("none"), true)
	st, pending := vfUnstakeVerdict(t, b.d, b.ctx, 5, 10*time.Minute)
	b.note("unstake with both vaults funded: status=%s pending=%d (want refused: FAILED or pending 0)", st, pending)
	if st == "CONFIRMED" && pending > 0 {
		t.Errorf("magi-5 could unstake while holding shares of two funded vaults")
	}
	// Drain the user's ETH from the bridge: the EVM vault keeps only dust.
	bal, _ := strconv.ParseInt(strings.SplitN(b.e.stateStr("a-"+b.user+"-eth"), "|", 2)[0], 10, 64)
	if bal > 0 {
		b.evmWithdraw(bbEth(9))
		devnetSleep(5 * time.Minute)
	}
	st, pending = vfUnstakeVerdict(t, b.d, b.ctx, 4, 10*time.Minute)
	b.note("unstake with the EVM side drained, BTC funded: status=%s pending=%d (want refused)", st, pending)
	if st == "CONFIRMED" && pending > 0 {
		t.Errorf("magi-4 could unstake while the BTC vault it holds a share of is funded (union broken)")
	}
}

// FS9 (I15): the network runs 0.12 (EVM custody rules inert) while the BTC
// rotation starts; 0.13 is proposed and activates mid-rotation; then the EVM
// rotation runs. No node may diverge.
func TestBothBridges_FS9_Activate013MidBtcRotation(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), false)
	start := devnetNow()
	b.at(start, 90*time.Second, "propose 0.13", func() {
		epoch := custodyCurrentEpoch(b.d, b.ctx, 1)
		for w := 1; w <= b.d.cfg.Nodes; w++ {
			if _, err := b.d.proposeConsensusVersion(w, 0, 13, epoch+1); err == nil {
				return
			}
		}
		t.Errorf("no witness could propose 0.13")
	})
	p1, ok := vfRotate(t, b.d, b.ctx, b.v.cid, 1)
	if !ok {
		t.Fatal("BTC rotation failed across the 0.13 activation")
	}
	b.mark("BTC rotation across the 0.13 proposal", start)
	if err := waitForConsensus(b.ctx, b.d, "0.13", 30*time.Minute); err != nil {
		t.Fatalf("0.13 never activated: %v", err)
	}
	b.mark("0.13 active", start)
	assertAllNodesHealthy(t, b.d, b.ctx, 20)
	if st := b.btcMigrate(0, p1); st == "" {
		t.Fatalf("BTC migration after 0.13 activation failed\n%s", strings.Join(b.notes, "\n"))
	}
	if ok, msg := b.e.ownerCall("createKey", "{}"); !ok {
		t.Fatalf("EVM createKey under 0.13: %s", msg)
	}
	if err := pollUntil(b.ctx, 45*time.Minute, func() bool { return b.e.keyPub(evmKeyName(1)) != "" }); err != nil {
		t.Fatalf("EVM keygen under 0.13: %v", err)
	}
	if err := pollUntil(b.ctx, 60*time.Minute, func() bool { ok, _ := b.e.ownerCall("activateKey", "{}"); return ok }); err != nil {
		t.Fatalf("EVM activation under 0.13: %v\nbot:\n%s", err, b.botLogs())
	}
	b.mark("EVM rotation after 0.13 activated", start)
	assertAllNodesHealthy(t, b.d, b.ctx, 20)
}

// FS10 (I1-I3, KEY-SPAM-1, A13): extra TSS keys (BB_JUNK_KEYS, default 6; 36
// for the A13 shape) waiting for keygen while both vaults rotate. Compare the
// TIMING lines with BB-1: the reshare round-robin (4 per rotate block) and the
// keygens holding tssMgr.lock stretch both rotations.
func TestBothBridges_FS10_KeyLoad(t *testing.T) {
	bbSkipShort(t)
	n := 6
	if v, err := strconv.Atoi(os.Getenv("BB_JUNK_KEYS")); err == nil && v > 0 {
		n = v
	}
	b := newBothEnv(t, 360*time.Minute, bbWeatherLevel("none"), true)
	for i := 0; i < n; i++ {
		insertTssKey(t, b.d.MongoURI(), fmt.Sprintf("bbjunk%02d-key", i), "ecdsa")
	}
	b.note("%d extra TSS keys seeded before the cycle", n)
	b.runCycle(cycleHooks{})
}
