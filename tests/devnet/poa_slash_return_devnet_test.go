package devnet

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPoaSlashedSeatReturnsAfterTopUp (item 14, floor guard / established-member
// exemption under flat weight). Under POA a seat cannot unstake below MinStake on its
// own (the collateral lock refuses it), so the only way down is a slash. A seat
// slashed below MinStake drops out; it then tops its stake back up.
//
//   - Stake weight (POA_DEVNET_FLOOR=5): the established-member exemption takes
//     min(current, last-ratified weight) = min(stake, stake) >= MinStake, so the
//     member re-enters at the next election.
//   - Flat weight (POA_DEVNET_FLOOR=9): last-ratified weight is 1 < MinStake, so the
//     exemption (and the floor guard) is dead and the member must re-serve the whole
//     inclusion window (W blocks; 86,400 = 3 days on mainnet).
//
// magi.test7 double-signs once (VSC_DOUBLE_SIGN_ONCE): a 10% principal slash takes
// 2000 to 1800 HIVE, below MinStake, set to 1900 here so one slash is enough.
// Expected: GREEN at floor 5, RED at floor 9.
func TestPoaSlashedSeatReturnsAfterTopUp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	requireDocker(t)
	const dipper = "magi.test7"
	cfg := regressionConfig()
	if pj := os.Getenv("DEVNET_PROJECT"); pj != "" {
		cfg.ProjectName = pj
	}
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ElectionInterval = 20
	cp.MinStake = 1_900_000 // 1900.000 HIVE: one 10% slash of 2000 drops below it
	cp.BondInclusionActivationHeight = 40
	cp.BondInclusionWindowBlocks = 400 // short enough that the others are matured when the slash lands (else "initial" mode elects everyone), long enough that maturity cannot explain a quick return
	cp.BondInclusionSampleCount = 8
	cp.MaxNewMembersPerElection = 1
	cp.BondInclusionEstablishedGraceBlocks = 4000
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_DOUBLE_SIGN_ACCOUNT"] = dipper
	cfg.MagiEnv["VSC_DOUBLE_SIGN_ONCE"] = "1"

	ctx, cancel := context.WithTimeout(context.Background(), vfTestBudget(90*time.Minute))
	t.Cleanup(cancel)
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 2, 12*time.Minute); err != nil {
		t.Fatalf("no epoch 2: %v", err)
	}
	m2, _, _ := d.electionMembersWithHeight(ctx, 1, 2)
	t.Logf("floor 0.%d, epoch 2 committee: %v", poaDevnetFloor(), m2)
	var stays []string
	for _, m := range m2 {
		if m != dipper && m != "hive:"+dipper {
			stays = append(stays, m)
		}
	}

	// 1. The slash.
	var slashed int64
	for end := time.Now().Add(15 * time.Minute); time.Now().Before(end); time.Sleep(5 * time.Second) {
		if v := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+dipper); v > 0 && v < cp.MinStake {
			slashed = v
			break
		}
	}
	if slashed == 0 {
		t.Fatalf("PRECONDITION FAILED: %s was never slashed below MinStake (now %d)", dipper, gqlConsensus(t, d.GQLEndpoint(1), "hive:"+dipper))
	}
	_, epSlash, _ := d.LocalNodeInfo(ctx, 1)
	t.Logf("1. %s slashed to %d (< MinStake %d) during epoch %d", dipper, slashed, cp.MinStake, epSlash)

	// 2. It drops out.
	epOut, err := d.pollUntilMembership(ctx, t, stays, dipper, false, epSlash+1, 12, 6*time.Minute)
	if err != nil {
		out, _ := exec.CommandContext(ctx, "bash", "-c", "docker logs "+d.containerName(1)+" 2>&1 | grep 'election processed' | tail -4").CombinedOutput()
		t.Fatalf("PRECONDITION FAILED: slashed %s never left the committee (an 'initial'-type election elects every witness without a stake check; recent elections:\n%s): %v", dipper, strings.TrimSpace(string(out)), err)
	}
	t.Logf("2. %s out of the committee at epoch %d", dipper, epOut)

	// 3. Top-up with fresh capital.
	if _, err := d.Deposit(ctx, d.cfg.Nodes, "2000.000", "hive"); err != nil {
		t.Fatalf("deposit: %v", err)
	}
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(5 * time.Second) {
		if bal, err := d.GetAccountBalance(ctx, 1, "hive:"+dipper); err == nil && bal.Hive >= 2_000_000 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("deposit never credited")
		}
	}
	bhTop, epTop, _ := d.LocalNodeInfo(ctx, 1)
	if _, err := d.ConsensusStake(d.cfg.Nodes, d.cfg.Nodes, "2000.000"); err != nil {
		t.Fatalf("consensus_stake: %v", err)
	}
	for end := time.Now().Add(5 * time.Minute); ; time.Sleep(5 * time.Second) {
		if v := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+dipper); v >= cp.MinStake {
			t.Logf("3. %s topped up to %d at ~block %d (epoch %d)", dipper, v, bhTop, epTop)
			break
		}
		if time.Now().After(end) {
			t.Fatalf("top-up never landed")
		}
	}

	// 4. Does it come back within a few elections (well inside W)?
	const within = 4
	epBack, err := d.pollUntilMembership(ctx, t, stays, dipper, true, epTop+1, within, 6*time.Minute)
	if err != nil {
		bh, _, _ := d.LocalNodeInfo(ctx, 1)
		t.Errorf("SLASH-RETURN: at floor 0.%d the slashed-then-topped-up seat %s did not re-enter within %d elections (block %d, window W=%d): %v", poaDevnetFloor(), dipper, within, bh, cp.BondInclusionWindowBlocks, err)
		return
	}
	bhBack, _, _ := d.LocalNodeInfo(ctx, 1)
	t.Logf("4. %s back at epoch %d (~block %d, %d blocks after the top-up; W=%d)", dipper, epBack, bhBack, bhBack-bhTop, cp.BondInclusionWindowBlocks)
}
