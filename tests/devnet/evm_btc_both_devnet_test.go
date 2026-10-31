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
	"path/filepath"
	"slices"
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
	b.runCycle(cycleHooks{btcKeygen: func() {
		now := devnetNow()
		for _, n := range []int{4, 5} {
			n := n
			b.at(now, 20*time.Second, fmt.Sprintf("magi-%d down 4 min (quorum lost, both keygens pending)", n), func() { b.outage(n, 4*time.Minute) })
		}
	}})
}

// FS2 (F2/F16 x both): L2 halts (two nodes down) while BOTH migrations are in
// flight; both must settle after it resumes.
func TestBothBridges_FS2_HaltMidBothMigrations(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{btcSigning: func() {
		now := devnetNow()
		for _, n := range []int{4, 5} {
			n := n
			b.at(now, 10*time.Second, fmt.Sprintf("magi-%d down 3 min (L2 halted mid-migrations)", n), func() { b.outage(n, 3*time.Minute) })
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
		btcKeygen:  func() { b.at(devnetNow(), 20*time.Second, "fleet restart (keygens running)", restart("keygens")) },
		btcSigning: func() { b.at(devnetNow(), 10*time.Second, "fleet restart (migrations running)", restart("migrations")) },
	})
}

// FS4 (I5): the BTC keysign halt (vsc.tss_halt via the gateway multisig) stops
// BTC withdrawals; the EVM bridge must keep signing and paying out, and BTC
// must resume after the halt is lifted.
func TestBothBridges_FS4_BtcHaltEvmKeepsSigning(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 200*time.Minute, bbWeatherLevel("none"), true)
	d := b.d
	halt := func(active bool) int {
		// keyId is reserved (the halt is BTC-global), as TestVaultTssHalt sends it.
		payload, _ := json.Marshal(map[string]any{"active": active, "keyId": "btc"})
		want := 0
		if active {
			want = d.cfg.Nodes
		}
		got := -1
		for deadline := devnetNow().Add(15 * time.Minute); devnetNow().Before(deadline); devnetSleep(15 * time.Second) {
			_, err := broadcastGatewayMultisig(t, d, "vsc.tss_halt", []string{"vsc.gateway"}, string(payload), devnetGatewayKeys(t, d))
			if err != nil {
				b.note("tss_halt(active=%v) broadcast: %s", active, firstLine(err.Error()))
				continue
			}
			if got = countHaltFlag(t, b.ctx, d, vfAllNodes(d.cfg.Nodes)); got == want {
				break
			}
		}
		return got
	}
	start := devnetNow()
	if n := halt(true); n != d.cfg.Nodes {
		t.Fatalf("BTC keysign halt not on every node (%d/%d)", n, d.cfg.Nodes)
	}
	b.mark("BTC halted", start)
	recip := b.evmWithdraw(bbEth(1))
	if err := b.botPoll(30*time.Minute, func() bool { return b.l1Balance(recip).Sign() > 0 }); err != nil {
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

// devnetGatewayKeys: witness gateway keys that are in vsc.gateway's active
// authority, exactly as many as its threshold (weight 1 each): Hive refuses a
// transaction with an unnecessary signature, so a fixed 4 (a 6-node devnet's
// threshold) fails on 5 nodes, where it is 3. FS4's first run broadcast nothing.
func devnetGatewayKeys(t *testing.T, d *Devnet) []*hivego.KeyPair {
	hc := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	accs, err := hc.GetAccount([]string{"vsc.gateway"})
	if err != nil || len(accs) != 1 {
		return nil
	}
	in := map[string]bool{}
	for _, ka := range accs[0].Active.KeyAuths {
		if k, ok := ka[0].(string); ok {
			in[k] = true
		}
	}
	var ks []*hivego.KeyPair
	for n := 1; n <= d.cfg.Nodes && len(ks) < int(accs[0].Active.WeightThreshold); n++ {
		kp := devnetGatewayKeypair(t, fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, n))
		if in[*kp.GetPublicKeyString()] {
			ks = append(ks, kp)
		}
	}
	return ks
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
	if err := b.botPoll(60*time.Minute, func() bool { ok, _ := b.e.ownerCall("activateKey", "{}"); return ok }); err != nil {
		t.Fatalf("EVM activation after unpause: %v\nbot:\n%s", err, b.botLogs())
	}
	b.mark("EVM rotation after unpause", start)
}

// FS6 (F20 x both): one node partitioned from the rest while both migrations sign.
func TestBothBridges_FS6_PartitionDuringBothMigrations(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	b.runCycle(cycleHooks{btcSigning: func() {
		b.at(devnetNow(), 10*time.Second, "magi-5 partitioned 4 min", func() {
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

// FS8 (I6, I7): the bond lock is the UNION of the BTC and EVM retiring vaults.
// A committee member holding shares of a retiring, still-funded generation
// cannot unstake its consensus bond (transactions.go:896, its own refusal text,
// checked BEFORE the POA exit-halt, which refuses any seated member too). So the
// verdict is the refusal TEXT, not the status. Three points in one cycle:
//   - both gen-0s retiring and funded: refused by the bond lock;
//   - BTC gen-0 swept, EVM gen-0 still draining: still refused by the bond lock;
//   - both drained: not refused by the bond lock (the exit-halt may still refuse).
//
// The first version unstaked before any rotation (no retiring generation, the
// lock cannot fire) and passed on the exit-halt's refusal: it proved nothing.
func TestBothBridges_FS8_BondLockUnion(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	const lockText = "consensus bond is locked: your committee holds a retiring"
	check := func(node int, when string, wantLock bool) {
		st, errText := b.unstakeVerdict(node)
		b.note("unstake magi-%d (%s): status=%s error=%q", node, when, st, errText)
		if locked := strings.Contains(errText, lockText); locked != wantLock {
			t.Errorf("unstake magi-%d (%s): bond-lock refusal=%v, want %v (status=%s error=%q)", node, when, locked, wantLock, st, errText)
		}
	}
	b.runCycle(cycleHooks{
		afterActivate: func() { check(5, "both gen-0 retiring + funded", true) },
		btcSwept: func() {
			if b.evmMigrated() {
				b.note("FS8: the EVM batch finished before the BTC sweep; the EVM-only lock point is not measured this run")
				return
			}
			check(4, "BTC gen-0 swept, EVM gen-0 still draining", true)
		},
		settled: func() { check(3, "both gen-0 drained", false) },
	})
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
	if err := b.botPoll(60*time.Minute, func() bool { ok, _ := b.e.ownerCall("activateKey", "{}"); return ok }); err != nil {
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

// FS11 (I8): a member stands down while both keygens run, so the next election
// changes the committee and rotates the gateway multisig in the same window as
// both vault rotations: three rotations at once. Every one must finish, the
// leaver must drop out of the committee and the gateway, and L2 must keep
// producing (the whole-cycle marks and the meter show the cost).
func TestBothBridges_FS11_CommitteeChangeMidBoth(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true)
	d := b.d
	leaver := d.cfg.Nodes
	acct := d.witnessAccount(leaver)
	leaverKey := *devnetGatewayKeypair(t, acct).GetPublicKeyString()
	hc := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	gatewayHas := func(key string) (bool, int) {
		accs, err := hc.GetAccount([]string{"vsc.gateway"})
		if err != nil || len(accs) != 1 {
			return false, -1
		}
		has := false
		for _, ka := range accs[0].Active.KeyAuths {
			if k, _ := ka[0].(string); k == key {
				has = true
			}
		}
		return has, len(accs[0].Active.KeyAuths)
	}
	if has, n := gatewayHas(leaverKey); !has {
		t.Fatalf("precondition: %s's gateway key is not in vsc.gateway's active authority (%d keys)", acct, n)
	}
	var epoch0 uint64
	var start time.Time
	b.runCycle(cycleHooks{
		afterCreateKeys: func(s time.Time) {
			start = s
			epoch0 = custodyCurrentEpoch(d, b.ctx, 1)
			n := disableWitnessEverywhere(t, d, b.ctx, acct, d.cfg.Nodes)
			b.note("%s stood down (%d witness rows) at epoch %d while both keygens run", acct, n, epoch0)
			go func() {
				if err := pollUntil(b.ctx, 45*time.Minute, func() bool {
					m, err := d.GetElectionMembers(b.ctx, 1, custodyCurrentEpoch(d, b.ctx, 1))
					return err == nil && len(m) > 0 && !slices.Contains(m, acct)
				}); err == nil {
					b.mark("committee change (leaver out of the election)", start)
				}
				if err := pollUntil(b.ctx, 45*time.Minute, func() bool { has, _ := gatewayHas(leaverKey); return !has }); err == nil {
					b.mark("gateway multisig rotated (leaver's key out)", start)
				}
			}()
		},
		settled: func() {
			ep := custodyCurrentEpoch(d, b.ctx, 1)
			m, err := d.GetElectionMembers(b.ctx, 1, ep)
			if err != nil || slices.Contains(m, acct) {
				t.Errorf("%s still elected at epoch %d after standing down at %d (members %v, err %v)", acct, ep, epoch0, m, err)
			}
			if has, n := gatewayHas(leaverKey); has {
				t.Errorf("the gateway multisig still lists %s's key after the committee change (%d keys)", acct, n)
			}
			assertAllNodesHealthy(t, d, b.ctx, 20)
		},
	})
}

// FS12 (I15): the fleet mid-upgrade, as testnet ran on 10-31: nodes 4-5 on the
// 0.12 release (BB_OLD_CODE_DIR, a checkout of c29e935e), 1-3 on this tree, the
// network at 0.12. A full BOTH cycle must run with no fork: EVM keys sign on old
// and new nodes alike (only BTC keys are scoped below 0.13). Real 3 s blocks:
// old images have no quiescence probe. Then every node must still agree.
func TestBothBridges_FS12_MixedFleet012(t *testing.T) {
	bbSkipShort(t)
	old := os.Getenv("BB_OLD_CODE_DIR")
	if old == "" {
		t.Skip("set BB_OLD_CODE_DIR to a go-vsc-node checkout of the 0.12 release (c29e935e)")
	}
	b := newBothEnv(t, 360*time.Minute, bbWeatherLevel("none"), false, func(c *Config) {
		c.FastBlocks = false
		c.RealCadence = true
		c.OldCodeSourceDir = old
		c.OldCodeNodes = []int{4, 5}
		c.OldCodeSysconfig = true
	})
	base := requireConverged(t, b.ctx, b.d, b.d.cfg.Nodes, 0, 10*time.Minute)
	b.note("mixed fleet (4-5 on %s) converged at block %d", filepath.Base(old), base)
	b.runCycle(cycleHooks{settled: func() {
		after := requireConverged(t, b.ctx, b.d, b.d.cfg.Nodes, base+20, 10*time.Minute)
		b.note("mixed fleet still converged at block %d after the cycle", after)
	}})
}
