package devnet

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/bson"
)

// topUpTBD sends extra TBD from initminer to an account (devnet funding is only
// 100 TBD/witness; the majority-savings test needs one account able to hold a
// balance larger than the hot float).
func (d *Devnet) topUpTBD(to, amount string) (string, error) {
	c := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	c.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	op := hivego.TransferOperation{From: "initminer", To: to, Amount: amount, Memo: "topup"}
	return c.Broadcast([]hivego.HiveOperation{op}, &wif)
}

// parseHBD converts a Hive amount string ("950.000 HBD") to raw int64 milliunits.
func parseHBD(s string) int64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	parts := strings.SplitN(f[0], ".", 2)
	whole, _ := strconv.ParseInt(parts[0], 10, 64)
	frac := int64(0)
	if len(parts) == 2 {
		frac, _ = strconv.ParseInt((parts[1] + "000")[:3], 10, 64)
	}
	return whole*1000 + frac
}

// onChainHBD reads an account's physical Hive liquid + savings HBD balances.
func (d *Devnet) onChainHBD(account string) (liquid, savings int64, err error) {
	c := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	c.ChainID = devnetChainID
	accts, err := c.GetAccount([]string{account})
	if err != nil {
		return 0, 0, err
	}
	if len(accts) == 0 {
		return 0, 0, fmt.Errorf("no account %s", account)
	}
	return parseHBD(accts[0].HbdBalance), parseHBD(accts[0].SavingsHbdBalance), nil
}

// onChainHIVE reads an account's physical Hive liquid + savings HIVE balances.
func (d *Devnet) onChainHIVE(account string) (liquid, savings int64, err error) {
	c := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	c.ChainID = devnetChainID
	accts, err := c.GetAccount([]string{account})
	if err != nil {
		return 0, 0, err
	}
	if len(accts) == 0 {
		return 0, 0, fmt.Errorf("no account %s", account)
	}
	return parseHBD(accts[0].Balance), parseHBD(accts[0].SavingsBalance), nil
}

// pendingWithdraw is a pending withdrawal action's amount + the block height at
// which its op was processed (used to compute its value-scaled delay expiry).
type pendingWithdraw struct {
	Amount int64  `bson:"amount"`
	Height uint64 `bson:"block_height"`
}

// frSyncEvent is a vsc.fr_sync reserve delta on system:fr_balance: Amount > 0 is a
// sweep (stake to savings), Amount < 0 is a refill (unstake). Height is the exact
// block the delta landed — used to prove a refill beat the regular sync cadence.
type frSyncEvent struct {
	Amount int64  `bson:"amount"`
	Height uint64 `bson:"block_height"`
}

func (d *Devnet) frSyncEvents(ctx context.Context, node int) ([]frSyncEvent, error) {
	cli, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := cli.Database(d.nodeDbName(node)).Collection("ledger").
		Find(ctx, bson.M{"owner": "system:fr_balance", "t": "fr_sync", "tk": "hbd_savings"})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []frSyncEvent
	for cur.Next(ctx) {
		var e frSyncEvent
		if cur.Decode(&e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (d *Devnet) pendingWithdraws(ctx context.Context, node int) ([]pendingWithdraw, error) {
	cli, err := d.mongoClient(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := cli.Database(d.nodeDbName(node)).Collection("ledger_actions").
		Find(ctx, bson.M{"status": "pending", "type": "withdraw"})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []pendingWithdraw
	for cur.Next(ctx) {
		var a pendingWithdraw
		if cur.Decode(&a) == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// TestVaultProtectionMajoritySavingsHiveDevnet proves the v0.6.0 HIVE leg
// (incl. consensus bonds) — the piece that touches consensus-state schema
// (HIVE_SAVINGS) and state-processing (vsc.fr_sync HIVE ingest):
//   - syncBalance sweeps the MAJORITY of the gateway's HIVE (liquid + bonds) into
//     Hive savings, leaving only the flow-sized float (on-chain savings > liquid);
//   - a >float HIVE withdrawal stays HELD by the coverage gate EVEN AFTER its
//     value-scaled delay expires (isolating coverage from the delay), while a
//     <float covered one settles.
func TestVaultProtectionMajoritySavingsHiveDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = false
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_SYNC_INTERVAL"] = "20"
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 35*time.Minute)
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// Deposit a little HBD from 6 witnesses to satisfy syncBalance's >=6 HBD-holder
	// guard, and the HIVE for the sweep (witnesses hold 10000 TESTS, no top-up).
	for n := 1; n <= cfg.Nodes; n++ {
		nn := n
		retryBroadcast(t, fmt.Sprintf("dep-hbd-%d", nn), func() (string, error) { return d.Deposit(ctx, nn, "90.000", "hbd") })
	}
	retryBroadcast(t, "dep-hive-1", func() (string, error) { return d.Deposit(ctx, 1, "5000.000", "hive") })
	for n := 2; n <= cfg.Nodes; n++ {
		nn := n
		retryBroadcast(t, fmt.Sprintf("dep-hive-%d", nn), func() (string, error) { return d.Deposit(ctx, nn, "500.000", "hive") })
	}
	for n := 1; n <= cfg.Nodes; n++ {
		full := "hive:" + d.witnessAccount(n)
		if !waitBalancePositive(t, d, ctx, 1, full, "hbd", 3*time.Minute) {
			t.Fatalf("hbd deposit for witness%d never credited", n)
		}
		if !waitBalancePositive(t, d, ctx, 1, full, "hive", 3*time.Minute) {
			t.Fatalf("hive deposit for witness%d never credited", n)
		}
	}
	const totalHive = int64(5_000_000 + 5*500_000) // 7_500_000 raw HIVE (deposits; bonds add more)
	const floatHIVE = int64(1_000_000)             // hotFloatFloorHIVE

	// ===================== Proof 1: majority HIVE swept to savings =================
	swept := false
	var liqOn, savOn int64
	for deadline := time.Now().Add(6 * time.Minute); time.Now().Before(deadline); {
		l, s, err := d.onChainHIVE("vsc.gateway")
		if err == nil {
			liqOn, savOn = l, s
			if savOn > liqOn && savOn > totalHive/2 {
				swept = true
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	if !swept {
		t.Fatalf("HIVE majority never swept to savings: on-chain liquid=%d savings=%d (deposits≈%d)", liqOn, savOn, totalHive)
	}
	t.Logf("HIVE swept: gateway on-chain liquid=%d savings=%d (float=%d, deposits≈%d + bonds)", liqOn, savOn, floatHIVE, totalHive)

	// ===================== Proof 2: coverage isolated from delay ===================
	w1, w2 := d.witnessAccount(1), d.witnessAccount(2)
	retryBroadcast(t, "wd-hive-large", func() (string, error) { return d.Withdraw(1, w1, "2000.000", "hive", "large-held") })
	retryBroadcast(t, "wd-hive-small", func() (string, error) { return d.Withdraw(2, w2, "300.000", "hive", "small-covered") })
	const largeMin = int64(1_500_000) // >= this == the 2000-HIVE "large"

	// Phase 1: both appear pending; capture the large's action height.
	var largeHeight uint64
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
		pw, _ := d.pendingWithdraws(ctx, 1)
		hasLarge, hasSmall := false, false
		for _, w := range pw {
			if w.Amount >= largeMin {
				hasLarge, largeHeight = true, w.Height
			} else if w.Amount > 0 {
				hasSmall = true
			}
		}
		if hasLarge && hasSmall {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if largeHeight == 0 {
		t.Fatal("large HIVE withdrawal never appeared pending with a height")
	}

	// Phase 2: the small (no delay, covered) settles out.
	smallGone := false
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
		pw, _ := d.pendingWithdraws(ctx, 1)
		hasSmall := false
		for _, w := range pw {
			if w.Amount > 0 && w.Amount < largeMin {
				hasSmall = true
			}
		}
		if !hasSmall {
			smallGone = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !smallGone {
		t.Fatal("small covered HIVE withdrawal never settled")
	}

	// Phase 3: wait past the large's value-scaled delay (tier-1 = 200 blocks). If
	// only the delay were holding it, it would settle now; if it is STILL held, the
	// COVERAGE gate is holding it (the float can't cover 2000 HIVE) — isolating
	// coverage from delay.
	const tier1DelayBlocks = uint64(200) // ~10 min at ~3s/block
	if err := d.WaitForBlockProcessing(ctx, 1, largeHeight+tier1DelayBlocks+3, 13*time.Minute); err != nil {
		t.Fatalf("chain never passed the large withdrawal's delay expiry: %v", err)
	}
	pw, _ := d.pendingWithdraws(ctx, 1)
	stillHeld := false
	for _, w := range pw {
		if w.Amount >= largeMin {
			stillHeld = true
		}
	}
	if !stillHeld {
		t.Fatal("coverage NOT isolated: the large HIVE withdrawal cleared once its delay expired — coverage did not hold it")
	}
	t.Log("HIVE coverage isolated: the over-float withdrawal is still held AFTER its value-scaled delay expired")
}

// TestVaultProtectionProactiveReorderDevnet proves the v0.6.0 proactive low-water
// reorder (#2), and in doing so exercises the aggregate GetAll (#1) and the
// ConsensusParams-sourced sizing (#7). With SYNC_INTERVAL=100 but ACTION_INTERVAL
// at its default 20, there are off-sync action ticks. After the first sweep, a
// withdrawal drains the float below the 50% low-water mark; the gateway must refill
// (reserve drops) on an off-sync tick — BEFORE the next regular sync could fire
// (sweepBlock + SYNC_INTERVAL) — which only the low-water bypass can do.
func TestVaultProtectionProactiveReorderDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	const syncInterval = uint64(200)
	cfg := tssTestConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = false
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_SYNC_INTERVAL"] = "200" // ACTION_INTERVAL stays 20 -> off-sync ticks exist
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 30*time.Minute)
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		if err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute); err != nil {
			cancel()
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
		cancel()
	}

	w1 := d.witnessAccount(1)
	retryBroadcast(t, "topup-w1", func() (string, error) { return d.topUpTBD(w1, "900.000 TBD") })
	topped := false
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		if liq, _, err := d.onChainHBD(w1); err == nil && liq >= 600_000 {
			topped = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !topped {
		t.Fatal("witness1 top-up never landed")
	}
	retryBroadcast(t, "dep-1", func() (string, error) { return d.Deposit(ctx, 1, "600.000", "hbd") })
	for n := 2; n <= cfg.Nodes; n++ {
		nn := n
		retryBroadcast(t, fmt.Sprintf("dep-%d", nn), func() (string, error) { return d.Deposit(ctx, nn, "90.000", "hbd") })
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if !waitBalancePositive(t, d, ctx, 1, "hive:"+d.witnessAccount(n), "hbd", 3*time.Minute) {
			t.Fatalf("deposit for witness%d never credited", n)
		}
	}
	// Phase 1: first sweep -> a POSITIVE fr_sync (stake to savings) on
	// system:fr_balance at exact block S1. (Also validates the aggregate GetAll (#1)
	// and the ConsensusParams-sourced sizing (#7): the sweep only parks the majority
	// if both compute correctly.)
	var s1 uint64
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline); {
		for _, e := range mustFrSync(d, ctx) {
			if e.Amount > 0 {
				s1 = e.Height
				break
			}
		}
		if s1 > 0 {
			break
		}
		time.Sleep(4 * time.Second)
	}
	if s1 == 0 {
		t.Fatal("first sweep never emitted a fr_sync stake (majority not parked)")
	}
	t.Logf("swept: fr_sync stake at block %d", s1)

	// Phase 2: drain the float below low-water. 60 HBD (< the ~100 HBD float) settles,
	// leaving ~40 HBD liquid < 50% of the float.
	w1Full := "hive:" + w1
	before, _ := d.GetAccountBalance(ctx, 1, w1Full)
	retryBroadcast(t, "drain", func() (string, error) { return d.Withdraw(1, w1, "60.000", "hbd", "drain-float") })
	drained := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
		if b, _ := d.GetAccountBalance(ctx, 1, w1Full); b != nil && before != nil && b.Hbd <= before.Hbd-60_000 {
			drained = true
			break
		}
		time.Sleep(4 * time.Second)
	}
	if !drained {
		t.Fatal("drain withdrawal never settled (float not drained)")
	}

	// Phase 3: a refill -> a NEGATIVE fr_sync (unstake) at exact block S2. AIRTIGHT
	// proof of the proactive bypass: a REGULAR sync cannot fire within SYNC_INTERVAL
	// of the previous sync (the freshness guard), so S2 - S1 < SYNC_INTERVAL means the
	// refill could ONLY be the low-water proactive reorder.
	var s2 uint64
	for deadline := time.Now().Add(7 * time.Minute); time.Now().Before(deadline); {
		for _, e := range mustFrSync(d, ctx) {
			if e.Amount < 0 && e.Height > s1 {
				s2 = e.Height
				break
			}
		}
		if s2 > 0 {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if s2 == 0 {
		t.Fatal("reserve never refilled (no fr_sync unstake) after the float was drained below low-water")
	}
	if s2 >= s1+syncInterval {
		t.Fatalf("refill at block %d is a full SYNC_INTERVAL+ after the sweep at %d — could be a regular sync, not proactive", s2, s1)
	}
	t.Logf("PROACTIVE refill: fr_sync unstake at block %d, only %d blocks after the sweep (< SYNC_INTERVAL %d) — a regular sync could not have fired that soon",
		s2, s2-s1, syncInterval)
}

// mustFrSync reads node 1's fr_sync reserve deltas, tolerating transient read errors.
func mustFrSync(d *Devnet, ctx context.Context) []frSyncEvent {
	evs, _ := d.frSyncEvents(ctx, 1)
	return evs
}

// TestVaultProtectionMajoritySavingsDevnet proves the v0.6.0 majority-to-savings
// feature end-to-end on a real multi-node devnet with consensus 0.6.0 pinned:
//   - after deposits, syncBalance sweeps the MAJORITY of gateway HBD into Hive
//     savings (both the tracked reserve AND the physical on-chain savings balance),
//     leaving only a flow-sized hot float liquid;
//   - a withdrawal that EXCEEDS the liquid float is HELD by the coverage gate while
//     a small covered withdrawal settles out — with no overdraft (the in-flight
//     unstake accounting keeps the estimate correct across the 3-day refill window).
func TestVaultProtectionMajoritySavingsDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.Nodes = 6 // syncBalance requires >= 6 non-system HBD accounts
	cfg.SkipFunding = false
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_SYNC_INTERVAL"] = "20" // sweep ~every minute, not every 6h
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 6
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 30*time.Minute)
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 1, 8*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// Top up witness1 so it can deposit more than the 100-HBD hot float. Wait for
	// the extra TBD to land on L1 before depositing from it.
	w1 := d.witnessAccount(1)
	retryBroadcast(t, "topup-w1", func() (string, error) { return d.topUpTBD(w1, "900.000 TBD") })
	topped := false
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		if liq, _, err := d.onChainHBD(w1); err == nil && liq >= 600_000 {
			topped = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !topped {
		t.Fatal("witness1 top-up never landed on L1")
	}

	// Deposit: 600 HBD from w1 (big), 90 HBD from w2..w6 (>= 6 HBD accounts).
	retryBroadcast(t, "dep-1", func() (string, error) { return d.Deposit(ctx, 1, "600.000", "hbd") })
	for n := 2; n <= cfg.Nodes; n++ {
		nn := n
		retryBroadcast(t, fmt.Sprintf("dep-%d", nn), func() (string, error) { return d.Deposit(ctx, nn, "90.000", "hbd") })
	}
	for n := 1; n <= cfg.Nodes; n++ {
		if !waitBalancePositive(t, d, ctx, 1, "hive:"+d.witnessAccount(n), "hbd", 3*time.Minute) {
			t.Fatalf("deposit for witness%d never credited", n)
		}
	}
	const total = int64(600_000 + 5*90_000) // 1_050_000 raw HBD of L2 liability
	const floatHBD = int64(100_000)         // hotFloatFloorHBD on a fresh chain

	// ===================== Proof 1: majority swept to savings =====================
	// hotFloat on a fresh chain = the 100-HBD floor, so ~950 of ~1050 is parked.
	swept := false
	var reserve int64
	for deadline := time.Now().Add(6 * time.Minute); time.Now().Before(deadline); {
		if fr, _ := d.GetAccountBalance(ctx, 1, "system:fr_balance"); fr != nil {
			reserve = fr.HbdSavings
		}
		if reserve > total/2 { // majority parked
			swept = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !swept {
		t.Fatalf("majority never swept to savings: tracked reserve=%d, total=%d (want > %d)", reserve, total, total/2)
	}
	// Physical corroboration: the gateway's ON-CHAIN savings holds the majority.
	liqOn, savOn, err := d.onChainHBD("vsc.gateway")
	if err != nil {
		t.Fatalf("read gateway on-chain HBD: %v", err)
	}
	t.Logf("swept: tracked reserve=%d; gateway on-chain liquid=%d savings=%d (total≈%d, float=%d)",
		reserve, liqOn, savOn, total, floatHBD)
	if savOn <= liqOn {
		t.Fatalf("on-chain savings is not the majority: liquid=%d savings=%d", liqOn, savOn)
	}
	if liqOn > 3*floatHBD {
		t.Fatalf("liquid float not collapsed to ~hot-float: on-chain liquid=%d (float=%d)", liqOn, floatHBD)
	}

	// ===================== Proof 2: coverage/hold, no overdraft ====================
	// Liquid float ≈ 100 HBD. A 300-HBD withdrawal (> float) must be HELD; a 30-HBD
	// one (< float) settles. Both are < the tier-1 delay threshold, so the hold is
	// the COVERAGE gate, not the value-scaled delay.
	const largeMin = int64(200_000) // pending action >= this == the "large" (300 HBD)
	hasLarge := func(a []int64) bool {
		for _, x := range a {
			if x >= largeMin {
				return true
			}
		}
		return false
	}
	hasSmall := func(a []int64) bool {
		for _, x := range a {
			if x > 0 && x < largeMin {
				return true
			}
		}
		return false
	}
	retryBroadcast(t, "wd-large", func() (string, error) { return d.Withdraw(1, w1, "300.000", "hbd", "large-held") })
	retryBroadcast(t, "wd-small", func() (string, error) { return d.Withdraw(2, d.witnessAccount(2), "30.000", "hbd", "small-covered") })

	// Phase 1: both withdrawal actions appear pending (ops take a few blocks).
	seen := false
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); {
		amts, _ := d.pendingActionAmounts(ctx, 1, "withdraw")
		if hasLarge(amts) && hasSmall(amts) {
			seen = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !seen {
		t.Fatal("withdrawal actions never both appeared pending")
	}

	// Phase 2: the small (covered) settles OUT while the large (over-float) stays
	// held. If the large clears, coverage failed or the gateway overdrafted.
	proven := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
		amts, _ := d.pendingActionAmounts(ctx, 1, "withdraw")
		if !hasLarge(amts) {
			t.Fatal("coverage FAILED: the over-float withdrawal cleared pending (settled or overdrafted the batch)")
		}
		if !hasSmall(amts) {
			proven = true
			t.Log("coverage/hold holds: small covered withdrawal settled while the over-float one is held")
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !proven {
		t.Fatal("coverage inconclusive: small never settled while large held (gateway slow / batch stuck?)")
	}
}
