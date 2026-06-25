package devnet

import (
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	governance "vsc-node/modules/governance"
)

// sumReserveCredits sums the safety_slash_reserve credit rows on the keyless
// insurance reserve account (the matured slash residual it now holds), read from
// node 1.
func sumReserveCredits(t *testing.T, d *Devnet) int64 {
	t.Helper()
	rows := findLedgerTXs(t, d.GQLEndpoint(1), params.ProtocolSlashReserveAccount, []string{"safety_slash_reserve"})
	var total int64
	for _, r := range rows {
		if r.Amount > 0 && r.Asset == "hive" {
			total += r.Amount
		}
	}
	return total
}

// sumReservePayoutDebits sums the reserve_payout_debit rows on the reserve
// account (negative; the amount drawn out by governance payouts), read from node 1.
func sumReservePayoutDebits(t *testing.T, d *Devnet) int64 {
	t.Helper()
	rows := findLedgerTXs(t, d.GQLEndpoint(1), params.ProtocolSlashReserveAccount, []string{"reserve_payout_debit"})
	var total int64
	for _, r := range rows {
		if r.Asset == "hive" {
			total += r.Amount // negative
		}
	}
	return total
}

// TestGovernanceReservePayoutVote proves the heavy make-whole path end-to-end on
// a real multi-node devnet: after a slash residual MATURES into the keyless
// insurance reserve, a quorum of witnesses disburses it back to the slashed
// account via vsc.reserve_payout (the create == proposer's first vote) +
// vsc.reserve_vote (votes by proposal id). This is the first-ever debit of the
// reserve and the exact recovery path for an already-matured slash (the mengao
// case).
//
// VSC_SLASH_BURN_DELAY=0 sends the residual STRAIGHT to the reserve (no pending
// window), so the funds are immediately reserve-held and the only way out is this
// vote. The recipient is the slashed witness (magi.test3), which is therefore the
// excluded beneficiary — it cannot vote for its own payout, and the OTHER
// witnesses decide.
//
// MINIMUM-QUORUM CHECK: the recipient (test3) is excluded, leaving 5 effective
// witnesses (test1,2,4,5,6) and a threshold of ceil(2*5/3)=4. We sign with
// EXACTLY 4 — the create from test1 plus votes from test2/test4/test5 — and leave
// test6 abstaining, proving the payout lands on the minimum quorum, not all 5.
//
// Run manually (long-running, excluded from `make test`):
//
//	go test ./tests/devnet/ -run TestGovernanceReservePayoutVote -timeout 45m -v
func TestGovernanceReservePayoutVote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	const (
		attacker = "magi.test3" // forced double-signer → slashed; its residual funds the reserve; the payout recipient (excluded)
		honest   = "magi.test1"
	)

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"VSC_DOUBLE_SIGN_ACCOUNT":  attacker,
		"VSC_DOUBLE_SIGN_ONCE":     "1",
		"VSC_SLASH_BURN_DELAY":     "0", // residual goes straight to the keyless reserve (no pending window)
		"DEVNET_DETERMINISTIC_BLS": "1", // deterministic witness keys so genesis-elector forms a valid committee
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	baseH := waitForStableBond(t, d, "hive:"+honest, 8*time.Minute)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("[reserve-pay] head=%d baseline(stable honest)=%d", head, baseH)

	// 1) Wait for the double-sign to slash node-3.
	slashDeadline := time.Now().Add(10 * time.Minute)
	var cur3 int64 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	for time.Now().Before(slashDeadline) {
		cur3 = gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if cur3 >= 0 && cur3 < baseH {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !(cur3 >= 0 && cur3 < baseH) {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve-pay] node-3 bond never dropped below baseline (baseH=%d cur=%d) — slash did not fire", baseH, cur3)
	}
	t.Logf("[reserve-pay] node-3 slashed: bond now=%d (baseline=%d); slash-debit total=%d", cur3, baseH, sumSlashDebits(t, d, "hive:"+attacker))

	// 2) Wait for the residual to land in the reserve and STABILIZE (a single
	// incident can trip two detectors → two reserve credits arriving slightly
	// apart). The stable total is the amount we will disburse in full.
	var reserveAmt int64
	reserveDeadline := time.Now().Add(5 * time.Minute)
	var lastReserve int64 = -1
	for time.Now().Before(reserveDeadline) {
		v := sumReserveCredits(t, d)
		if v > 0 && v == lastReserve {
			reserveAmt = v
			break
		}
		lastReserve = v
		time.Sleep(3 * time.Second)
	}
	if reserveAmt <= 0 {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve-pay] reserve was never funded by the slash residual (last=%d)", lastReserve)
	}
	t.Logf("[reserve-pay] reserve funded and stable at %d (will disburse in full to %s)", reserveAmt, attacker)

	preRecipient := gqlHiveSpendable(t, d.GQLEndpoint(1), "hive:"+attacker)
	if preRecipient < 0 {
		preRecipient = 0
	}
	wantHive := preRecipient + reserveAmt
	t.Logf("[reserve-pay] recipient pre-payout spendable hive=%d; expect rise to %d", preRecipient, wantHive)

	// 3) Propose the payout. The CREATE op (from test1) introduces {recipient,
	// amount, reason} and counts as test1's first vote; the proposal id is derived
	// from the content + this create tx id, exactly as the handler derives it.
	reason := "reserve make-whole (devnet test)"
	createPayload := map[string]interface{}{
		"recipient": attacker,
		"amount":    reserveAmt,
		"reason":    reason,
	}
	createTx, err := d.BroadcastCustomJSON("vsc.reserve_payout", []string{honest}, createPayload, d.cfg.InitminerWIF)
	if err != nil {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[reserve-pay] broadcasting vsc.reserve_payout create: %v", err)
	}
	proposalID := governance.ReservePayoutProposalID(attacker, reserveAmt, reason, createTx)
	t.Logf("[reserve-pay] create tx=%s proposal_id=%s (test1's create == vote 1/4)", createTx, proposalID)

	// Let the create land + be processed so the proposal exists on every node
	// before the votes reference it (a vote for an unknown proposal is dropped).
	time.Sleep(18 * time.Second)

	// 4) Sign with EXACTLY the threshold: 3 more votes (test2, test4, test5) →
	// create + 3 = 4 = ceil(2*5/3). test6 ABSTAINS (recipient test3 is excluded).
	votePayload := map[string]interface{}{"id": proposalID}
	for _, voter := range []string{"magi.test2", "magi.test4", "magi.test5"} {
		txID, err := d.BroadcastCustomJSON("vsc.reserve_vote", []string{voter}, votePayload, d.cfg.InitminerWIF)
		if err != nil {
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
			t.Fatalf("[reserve-pay] broadcasting vsc.reserve_vote from %s: %v", voter, err)
		}
		t.Logf("[reserve-pay] vote from %s tx=%s", voter, txID)
		time.Sleep(1 * time.Second)
	}
	t.Logf("[reserve-pay] 4 of 5 effective witnesses signed (test6 abstains) — minimum quorum")

	// 5) Poll until the recipient's spendable hive rises by the disbursed amount.
	payDeadline := time.Now().Add(8 * time.Minute)
	paid := false
	for time.Now().Before(payDeadline) {
		if gqlHiveSpendable(t, d.GQLEndpoint(1), "hive:"+attacker) >= wantHive {
			paid = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !paid {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		final := gqlHiveSpendable(t, d.GQLEndpoint(1), "hive:"+attacker)
		t.Fatalf("[reserve-pay] recipient spendable hive never rose to %d (final=%d, pre=%d, amount=%d) — minimum quorum did not disburse",
			wantHive, final, preRecipient, reserveAmt)
	}

	// 6) Cross-node consistency + reserve drawdown: every node must agree the
	// recipient was paid, and the reserve must carry a debit of the disbursed amount.
	// All nodes apply the payout at the SAME L1 block (the vote tally is a
	// deterministic function of L1-ordered votes), but a non-leader may process
	// that block a beat after node-1, so poll each node to convergence rather than
	// reading a single instant. A genuine divergence still fails at the deadline.
	crossDeadline := time.Now().Add(2 * time.Minute)
	for {
		laggard, laggardV := 0, int64(0)
		for n := 1; n <= cfg.Nodes; n++ {
			if v := gqlHiveSpendable(t, d.GQLEndpoint(n), "hive:"+attacker); v < wantHive {
				laggard, laggardV = n, v
				break
			}
		}
		if laggard == 0 {
			break // every node agrees
		}
		if time.Now().After(crossDeadline) {
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
			t.Fatalf("[reserve-pay] node-%d disagrees: recipient spendable hive=%d < want=%d", laggard, laggardV, wantHive)
		}
		time.Sleep(3 * time.Second)
	}
	if debits := sumReservePayoutDebits(t, d); debits > -reserveAmt {
		t.Fatalf("[reserve-pay] expected a reserve debit of <= %d, got %d", -reserveAmt, debits)
	}
	t.Logf("[reserve-pay] SUCCESS: %d disbursed from the reserve to %s on the minimum 4-of-5 quorum; consistent across all %d nodes",
		reserveAmt, attacker, cfg.Nodes)
}
