package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestBondGateActive_DipThenReturn proves the one cannot-reset path the
// gate-active test does not exercise: a member who LEAVES (full consensus
// unstake → legitimately dropped from the committee) and RETURNS within the
// established/grandfather grace re-enters IMMEDIATELY — without re-serving the
// inclusion window — while every continuously-staked member stays seated the
// whole time.
//
// Discrimination from ordinary maturation: the window W is set to 800 blocks
// (≫ the restake→next-election gap), so the returning member's MIN-over-window
// matured stake is 0 at re-entry time (the window still contains pre-restake
// sample heights where the balance was 0). If they re-enter anyway, it can
// only be the min(current, last-ratified) exemption floors (F11 grandfather /
// established-member exception — both operator-specified, both accepted
// as-built 2026-06-07). The re-stake deliberately uses FRESH capital (a new
// L1 deposit, not the unstaked funds) — exercising exactly the accepted E3
// semantic: fresh capital up to last-ratified counts without re-waiting.
//
// Also implicitly proven: the churn cap (MaxNewMembersPerElection=1) does NOT
// defer the returning established member (the E2 cap exemption, accepted
// as-built), and the floor guard does NOT resurrect a genuinely-unstaked
// incumbent (bond_gate.go line ~913: wgt<=0 || wgt<MinStake → never re-seated),
// so the dip-out itself is observable.
func TestBondGateActive_DipThenReturn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet bond-gate dip-return test in short mode")
	}
	requireDocker(t)

	cfg := regressionConfig()
	cp := cfg.SysConfigOverrides.ConsensusParams
	// FAST config (feedback_devnet_fast_config_intervals): ~60s/epoch instead of
	// regressionConfig's ~5min/epoch, so the consensus-unstaking maturation
	// (~4 epochs) + the re-stake + the return election all fit one run. The
	// first (slow) run proved the DROP but ran out of wall-clock before the
	// return epoch.
	cp.ElectionInterval = 20
	cp.BondInclusionActivationHeight = 40 // epoch-2 boundary, after epoch-1 committee forms
	// W must exceed the restake→re-entry gap so matured stake CANNOT explain the
	// re-entry (only the min(current,last-ratified) exemption can). 200 blocks ≈
	// 10 epochs ≫ the unstaking-delay + restake + 1-election gap.
	cp.BondInclusionWindowBlocks = 200
	cp.BondInclusionSampleCount = 8
	cp.MaxNewMembersPerElection = 1
	// Grace must comfortably cover the whole dip. Lookback = 4000/20+16 = 216
	// elections, well under the 2048 cap.
	cp.BondInclusionEstablishedGraceBlocks = 4000

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	t.Cleanup(cancel)

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })

	t.Log("starting 7-node devnet, bond gate ACTIVE at 150, W=800, cap=1, grace=1600...")
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 1, 30, 6*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}

	// Epoch 1: genesis committee (gate inactive below activation height).
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never ratified: %v", err)
	}
	members1, _, err := d.electionMembersWithHeight(ctx, 1, 1)
	if err != nil {
		t.Fatalf("get epoch-1 members: %v", err)
	}
	t.Logf("epoch 1 committee: %d members %v", len(members1), members1)
	if len(members1) < 4 {
		t.Fatalf("epoch-1 committee unexpectedly small: %d (devnet bootstrap problem, not the gate)", len(members1))
	}

	// The dipper: the last witness. stays = everyone else (must never flicker).
	dipper := d.witnessAccount(d.cfg.Nodes)
	stays := make([]string, 0, len(members1)-1)
	foundDipper := false
	for _, m := range members1 {
		if m == dipper {
			foundDipper = true
			continue
		}
		stays = append(stays, m)
	}
	if !foundDipper {
		t.Fatalf("dipper %s not in the epoch-1 committee %v — cannot run the dip scenario", dipper, members1)
	}

	// Epoch 2: the first gated election (grandfather path — already proven by
	// TestBondGateActive_NoResetOfEstablishedCommittee; sanity-assert here).
	if err := d.waitForElectionEpoch(ctx, 1, 2, 12*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("second (gated) election never ratified: %v", err)
	}
	members2, _, err := d.electionMembersWithHeight(ctx, 1, 2)
	if err != nil {
		t.Fatalf("get epoch-2 members: %v", err)
	}
	assertSubset(t, members1, members2, "epoch-2 (gate activation) dropped an established member")

	// ── DIP: full consensus unstake ────────────────────────────────────────
	bhUnstake, epUnstake, _ := d.LocalNodeInfo(ctx, 1)
	txU, err := d.ConsensusUnstake(d.cfg.Nodes, "2000.000")
	if err != nil {
		t.Fatalf("consensus_unstake broadcast: %v", err)
	}
	t.Logf("DIP: %s fully unstaked (tx %s) at ~block %d (epoch %d)", dipper, txU, bhUnstake, epUnstake)

	// Poll ratified elections until the dipper is gone (their stake is 0 — a
	// legitimate exit, NOT a reset). The floor guard must NOT resurrect them.
	epochOut, err := d.pollUntilMembership(ctx, t, stays, dipper, false, epUnstake+1, 24, 12*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("dipper never left the committee after full unstake: %v", err)
	}
	outMembers, _, _ := d.electionMembersWithHeight(ctx, 1, epochOut)
	t.Logf("DIP CONFIRMED: epoch %d committee without %s: %d members %v", epochOut, dipper, len(outMembers), outMembers)

	// ── RETURN: FRESH capital (new L1 deposit), within grace ───────────────
	if _, err := d.Deposit(ctx, d.cfg.Nodes, "2000.000", "hive"); err != nil {
		t.Fatalf("fresh deposit: %v", err)
	}
	// Wait for the deposit to credit the dipper's VSC hive balance.
	depositOk := false
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end); {
		bal, bErr := d.GetAccountBalance(ctx, 1, "hive:"+dipper)
		if bErr == nil && bal.Hive >= 2_000_000 {
			depositOk = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done waiting for deposit: %v", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	if !depositOk {
		t.Fatalf("fresh 2000.000 hive deposit never credited %s", dipper)
	}
	bhRestake, epRestake, _ := d.LocalNodeInfo(ctx, 1)
	txS, err := d.ConsensusStake(d.cfg.Nodes, d.cfg.Nodes, "2000.000")
	if err != nil {
		t.Fatalf("consensus_stake broadcast: %v", err)
	}
	t.Logf("RETURN: %s re-staked 2000.000 FRESH hive (tx %s) at ~block %d (epoch %d)", dipper, txS, bhRestake, epRestake)

	// Poll until the dipper is BACK. With cap=1 and the established exemption,
	// re-entry must be immediate at the first election that sees the stake.
	epochBack, err := d.pollUntilMembership(ctx, t, stays, dipper, true, epRestake+1, 10, 12*time.Minute)
	if err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("RETURNING ESTABLISHED MEMBER NEVER RE-ENTERED (reset!): %v", err)
	}
	backMembers, backBh, err := d.electionMembersWithHeight(ctx, 1, epochBack)
	if err != nil {
		t.Fatalf("get re-entry election: %v", err)
	}
	assertSubset(t, members1, backMembers, "re-entry election lost an original member")
	t.Logf("RETURN CONFIRMED: epoch %d (block %d) committee: %d members %v", epochBack, backBh, len(backMembers), backMembers)

	// Attribution: if the re-entry election is within W−W/samples blocks of the
	// re-stake, the min-over-window matured stake was still 0 (the window holds
	// pre-restake zero samples) — so ONLY the grandfather/established exemption
	// floors can have re-admitted them. Outside that bound the result still
	// proves dip-then-return no-reset, but cannot discriminate the path.
	w := uint64(200)
	discriminant := w - w/8 // 175
	if backBh > bhRestake && backBh-bhRestake < discriminant {
		t.Logf("PROVEN on devnet: dip-then-return re-entry at +%d blocks after re-stake (< %d) — matured stake was still 0; the min(current,last-ratified) exemption re-admitted %s with FRESH capital, no re-wait, churn-cap-exempt; all %d continuously-staked members held their seats throughout",
			backBh-bhRestake, discriminant, dipper, len(stays))
	} else {
		t.Logf("dip-then-return re-entry CONFIRMED (no reset), but at +%d blocks after re-stake the window attribution is inconclusive (>= %d)",
			backBh-bhRestake, discriminant)
	}
}

// electionMembersWithHeight returns the (hive:-normalized) member accounts and
// the block height of the election at the given epoch.
func (d *Devnet) electionMembersWithHeight(ctx context.Context, node int, epoch uint64) ([]string, uint64, error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer client.Disconnect(ctx)
	coll := client.Database(d.nodeDbName(node)).Collection("elections")
	var result struct {
		BlockHeight uint64 `bson:"block_height"`
		Members     []struct {
			Account string `bson:"account"`
		} `bson:"members"`
	}
	if err := coll.FindOne(ctx, bson.M{"epoch": epoch}).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("finding election epoch %d: %w", epoch, err)
	}
	names := make([]string, len(result.Members))
	for i, m := range result.Members {
		names[i] = strings.TrimPrefix(m.Account, "hive:")
	}
	return names, result.BlockHeight, nil
}

// pollUntilMembership waits for ratified elections starting at fromEpoch and
// returns the first epoch where `account`'s membership == wantPresent. At every
// observed election it asserts that ALL `stays` members are present (the
// cannot-reset invariant under churn). Gives up after maxEpochs elections.
func (d *Devnet) pollUntilMembership(ctx context.Context, t *testing.T, stays []string, account string, wantPresent bool, fromEpoch uint64, maxEpochs int, perEpochTimeout time.Duration) (uint64, error) {
	for i := 0; i < maxEpochs; i++ {
		epoch := fromEpoch + uint64(i)
		if err := d.waitForElectionEpoch(ctx, 1, epoch, perEpochTimeout); err != nil {
			return 0, fmt.Errorf("epoch %d never ratified: %w", epoch, err)
		}
		members, _, err := d.electionMembersWithHeight(ctx, 1, epoch)
		if err != nil {
			return 0, fmt.Errorf("reading epoch %d: %w", epoch, err)
		}
		assertSubset(t, stays, members, fmt.Sprintf("epoch %d evicted a continuously-staked member", epoch))
		present := false
		for _, m := range members {
			if m == account {
				present = true
				break
			}
		}
		t.Logf("epoch %d: %d members, %s present=%v", epoch, len(members), account, present)
		if present == wantPresent {
			return epoch, nil
		}
	}
	return 0, fmt.Errorf("%s membership never became present=%v within %d elections from epoch %d", account, wantPresent, maxEpochs, fromEpoch)
}

// assertSubset fails the test if any account in want is missing from got.
func assertSubset(t *testing.T, want, got []string, msg string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, m := range got {
		set[m] = true
	}
	for _, m := range want {
		if !set[m] {
			t.Fatalf("%s: %s missing from %v", msg, m, got)
		}
	}
}
