package devnet

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestGatewayLeaversFreeze: who controls vsc.gateway when witnesses leave and
// switch their nodes off. The gateway's authority is the elected committee's
// gateway keys at ceil(2N/3) (modules/gateway/multisig.go keyRotation), and a
// rotation to the new committee must be signed under the OLD authority. Since
// 0.2.0 the owner authority has no vsc.dao backstop (mainnet: committee keys
// only, no recovery roster), so if the old key holders still online fall below
// the threshold, nothing can rotate the keys or pay a withdrawal, while L2 keeps
// producing on the new committee. Owner 10-31: "we didnt test that".
//
// 10 nodes, gateway rotation every 20 blocks, floor GW_FLOOR (default 12, the
// mainnet line after the upgrade: equal key weights). Phases, each a RESULT line:
//
//	P0  baseline: an HBD withdrawal is paid on L1.
//	P1  graceful leave: magi-9/10 disable their witness and keep running. The
//	    next rotation drops their keys (8 left) and a withdrawal is paid.
//	P2  leave + switch off: magi-7/8 disable and stop at once. 6 members are
//	    left, under MIN_GATEWAY_KEYS (8): no rotation can run, so the authority
//	    keeps listing the departed keys (8 keys, threshold 6). The 6 online
//	    holders still meet it: a withdrawal is paid.
//	P3  one more key holder off (magi-6 stops; still elected): 5 of the 8 listed
//	    holders online, under the threshold of 6. Expected: the withdrawal is
//	    NOT paid while L2 keeps producing (5 of 6 members online). The gap.
//	P4  magi-6 comes back: 6 online again, the pending withdrawal is paid.
//	    Recovery needs a departed key holder to return; nothing else can sign.
//
//	go test -v -run TestGatewayLeaversFreeze -timeout 150m ./tests/devnet/
func TestGatewayLeaversFreeze(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	floor := uint64(12)
	if v, err := strconv.ParseUint(os.Getenv("GW_FLOOR"), 10, 64); err == nil && v > 0 {
		floor = v
	}
	cfg := tssTestConfig()
	cfg.Nodes = 10
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, floor, 1
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	d, ctx := startDevnetNoKey(t, cfg, 140*time.Minute)

	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	t.Logf("consensus line 0.%d (floor 0.%d)", vfActiveConsensus(d, ctx), floor)

	waitRotation := func(label string, after uint64, within time.Duration) uint64 {
		for end := devnetNow().Add(within); devnetNow().Before(end); devnetSleep(10 * time.Second) {
			if m := gwRotationMark(t, d); m > after {
				t.Logf("RESULT %s: rotation landed, authority %s", label, gwAuthority(t, d))
				return m
			}
		}
		t.Logf("RESULT %s: no rotation within %s, authority %s", label, within, gwAuthority(t, d))
		return 0
	}
	// holders: the listed gateway key holders (by witness) and the threshold.
	holders := func() (int, map[string]int) {
		thr, weights := gwActiveWeights(t, d)
		owners := gwKeyOwners(t, d, ctx)
		out := map[string]int{}
		for k, w := range weights {
			who := owners[k]
			if who == "" {
				who = "?" + k[len(k)-6:]
			}
			out[who] += w
		}
		return thr, out
	}
	rot := waitRotation("GW-ROTATE-0 (committee keys installed)", 0, 20*time.Minute)
	thr, hs := holders()
	t.Logf("RESULT GW-AUTH-0: threshold %d, holders %v", thr, hs)

	payer := 1
	payeeAcct := d.witnessAccount(2)
	if _, err := d.Deposit(ctx, payer, "20.000", "hbd"); err != nil {
		t.Fatalf("PRECONDITION FAILED: deposit: %v", err)
	}
	for end := devnetNow().Add(5 * time.Minute); ; devnetSleep(5 * time.Second) {
		if b, err := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(payer)); err == nil && b.Hbd >= 20_000 {
			break
		}
		if devnetNow().After(end) {
			t.Fatalf("PRECONDITION FAILED: the HBD deposit was never credited on L2")
		}
	}
	// withdraw sends amount (milli-HBD) to the payee's L1 account and reports
	// whether it arrived within the window; payout is the L1 HBD it waits for.
	withdraw := func(label string, milli int64, within time.Duration) (string, bool) {
		before := gwL1Hbd(t, d, payeeAcct)
		tx, err := d.Withdraw(payer, payeeAcct, fmt.Sprintf("%d.%03d", milli/1000, milli%1000), "hbd", label)
		if err != nil {
			t.Fatalf("%s: withdraw: %v", label, err)
		}
		start := devnetNow()
		for end := devnetNow().Add(within); devnetNow().Before(end); devnetSleep(10 * time.Second) {
			if now := gwL1Hbd(t, d, payeeAcct); now >= before+milli {
				t.Logf("RESULT %s: paid on L1 after %s (tx %s)", label, devnetSince(start).Round(time.Second), tx)
				return tx, true
			}
		}
		t.Logf("RESULT %s: NOT paid within %s (tx %s)", label, within, tx)
		return tx, false
	}
	l2Slots := func() int { _, compared, _, _ := vfScanBlockHeaders(d, ctx, 5); return compared }

	// P0 baseline.
	if _, ok := withdraw("GW-P0 baseline", 1000, 15*time.Minute); !ok {
		t.Fatalf("baseline withdrawal not paid: the gateway does not work before any departure")
	}

	// P1 graceful leave: magi-9/10 disable, keep running through the rotation.
	epoch := custodyCurrentEpoch(d, ctx, 1)
	for _, n := range []int{9, 10} {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), cfg.Nodes)
	}
	// The rotation that removes them runs under the first election without
	// them; one landing before that election re-installs the same keys.
	for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end); devnetSleep(10 * time.Second) {
		m, _ := d.GetElectionMembers(ctx, 1, custodyCurrentEpoch(d, ctx, 1))
		if len(m) > 0 && !slices.Contains(m, d.witnessAccount(9)) && !slices.Contains(m, "hive:"+d.witnessAccount(9)) {
			break
		}
	}
	rot = gwRotationMark(t, d)
	rot = waitRotation("GW-P1 rotation after a graceful leave of magi-9/10", rot, 15*time.Minute)
	thr, hs = holders()
	t.Logf("RESULT GW-AUTH-1: threshold %d, holders %v", thr, hs)
	if _, in9 := hs[d.witnessAccount(9)]; in9 {
		t.Errorf("GW-P1: magi-9 left gracefully but its key is still in the authority")
	}
	if _, ok := withdraw("GW-P1 after graceful leave", 1000, 15*time.Minute); !ok {
		t.Errorf("GW-P1: withdrawal not paid after a graceful leave")
	}

	// P2 leave + switch off: magi-7/8 disable and stop at once.
	epoch = custodyCurrentEpoch(d, ctx, 1)
	for _, n := range []int{7, 8} {
		disableWitnessEverywhere(t, d, ctx, d.witnessAccount(n), cfg.Nodes)
		if err := d.StopNode(ctx, n); err != nil {
			t.Errorf("stop magi-%d: %v", n, err)
		}
	}
	_ = d.waitForElectionEpoch(ctx, 1, epoch+1, 15*time.Minute)
	waitRotation("GW-P2 rotation after magi-7/8 left and switched off (6 members < 8 keys)", rot, 6*time.Minute)
	thr, hs = holders()
	t.Logf("RESULT GW-AUTH-2: threshold %d, holders %v (magi-7/8 are offline)", thr, hs)
	withdraw("GW-P2 6 holders online", 1000, 15*time.Minute)

	// P3 one more key holder off.
	if err := d.StopNode(ctx, 6); err != nil {
		t.Errorf("stop magi-6: %v", err)
	}
	s0 := l2Slots()
	b3 := gwL1Hbd(t, d, payeeAcct)
	_, frozenOK := withdraw("GW-P3 5 holders online (threshold 6)", 1000, 10*time.Minute)
	s1 := l2Slots()
	t.Logf("RESULT GW-P3: L2 block slots compared on magi-1..5 went %d -> %d during the window", s0, s1)
	if frozenOK {
		t.Logf("GW-P3: paid with 5 of 8 holders online: the authority was not what the model expects; read GW-AUTH-2")
	}

	// P4 magi-6 returns.
	if err := d.StartNode(ctx, 6); err != nil {
		t.Errorf("start magi-6: %v", err)
	}
	if !frozenOK {
		recovered := false
		for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end) && !recovered; devnetSleep(10 * time.Second) {
			recovered = gwL1Hbd(t, d, payeeAcct) >= b3+1000
		}
		t.Logf("RESULT GW-P4: the frozen withdrawal paid after magi-6 returned: %v", recovered)
	}
	t.Logf("SUMMARY: graceful leave rotates the keys out; leaving + switching off below 8 members freezes the key list; one more holder off froze=%v while L2 slots advanced %d; recovery needed a departed holder back", !frozenOK, s1-s0)
}
