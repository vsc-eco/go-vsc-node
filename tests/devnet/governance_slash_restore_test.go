package devnet

import (
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// TestGovernanceSlashRestoreVote proves the witness-vote restoration path
// end-to-end on a real multi-node devnet: a wrongful safety slash is undone NOT
// by a gateway-multisig op but by a quorum of witnesses each broadcasting a cheap
// vsc.slash_restore custom_json FROM THEIR OWN WITNESS ACCOUNT. The state engine
// tallies those L1 votes deterministically and, on crossing 2/3 of the
// beneficiary-excluded electorate, runs applySafetySlashReverse to RESTORE
// node-3's slashed HIVE_CONSENSUS bond. No gateway key is ever materialized —
// restoration is pure L2 ledger state driven by the on-chain vote tally.
//
// Setup mirrors TestSafetySlashGovernanceReverse: a single forced double-sign by
// node-3 fires the slash, and a LONG burn delay (VSC_SLASH_BURN_DELAY=600) keeps
// the residual pending so the light cancel+reverse path has full headroom.
//
// Run manually (long-running, excluded from `make test`):
//
//	go test ./tests/devnet/ -run TestGovernanceSlashRestoreVote -timeout 45m -v
func TestGovernanceSlashRestoreVote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	const (
		attacker = "magi.test3" // forced double-signer → wrongfully slashed → the beneficiary (excluded from voting)
		honest   = "magi.test1"
	)

	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"VSC_DOUBLE_SIGN_ACCOUNT": attacker,
		"VSC_DOUBLE_SIGN_ONCE":    "1",
		"VSC_SLASH_BURN_DELAY":    "600", // long: residual stays pending so slash_restore (cancel+reverse) has full headroom
		// Match the proven-working reverse test: devnet-setup derives deterministic
		// per-witness BLS/consensus keys that line up with the running nodes, so the
		// genesis-elector finds a valid committee (avoids panic("No members found")).
		// (The vote path itself signs each witness vote with the initminer key, so it
		// does not depend on this — it is purely for reliable genesis startup.)
		"DEVNET_DETERMINISTIC_BLS": "1",
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	// Short election interval so the 6-witness electorate forms quickly.
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{ElectionInterval: 20},
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	// True post-genesis staking baseline (== node-3's pre-slash bond, since every
	// witness staked equally). node-3's bond must RETURN to this after restoration.
	baseH := waitForStableBond(t, d, "hive:"+honest, 8*time.Minute)
	head, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("[restore-vote] head=%d baseline(stable honest)=%d", head, baseH)

	// 1) Wait for the double-sign incident to slash node-3's bond below baseline.
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
		t.Fatalf("[restore-vote] node-3 bond never dropped below baseline (baseH=%d cur=%d) — slash did not fire", baseH, cur3)
	}
	slashedTotal := sumSlashDebits(t, d, "hive:"+attacker)
	t.Logf("[restore-vote] node-3 slashed: bond now=%d (baseline=%d); ledger slash-debit total=%d", cur3, baseH, slashedTotal)
	if slashedTotal <= 0 {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[restore-vote] no safety_slash_consensus debit rows for node-3")
	}

	// 2) Pick ONE slash row to restore. vsc.slash_restore needs only the row's
	// tx_id + the slashed account — the handler resolves the evidence kind and
	// amount from the row itself (voters never supply them). A double-sign trips
	// multiple detectors with DIFFERENT triggering txs, so we choose one row and
	// the handler will cancel+reverse exactly that (tx_id, kind).
	var slashRow ledgerRecord
	rowDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(rowDeadline) {
		rows := findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus"})
		var best ledgerRecord
		for _, r := range rows {
			if r.Amount >= 0 || r.Asset != "hive_consensus" {
				continue
			}
			if evidenceKindFromSlashRowID(r.Id) == "" {
				continue
			}
			// Prefer the largest single debit; deterministic tiebreak on row id so
			// this picks the SAME row the handler's resolver picks (smallest id).
			if best.TxId == "" || -r.Amount > -best.Amount || (-r.Amount == -best.Amount && r.Id < best.Id) {
				best = r
			}
		}
		if best.TxId != "" {
			slashRow = best
			break
		}
		time.Sleep(3 * time.Second)
	}
	if slashRow.TxId == "" {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		t.Fatalf("[restore-vote] no safety_slash_consensus row found for %s", attacker)
	}
	slashTxID := slashRow.TxId
	rowSlashedAmt := -slashRow.Amount // amount this one incident debited; the bond must rise by this
	t.Logf("[restore-vote] target slash row: id=%s tx_id=%s amount=%d kind=%s",
		slashRow.Id, slashTxID, slashRow.Amount, evidenceKindFromSlashRowID(slashRow.Id))

	preVote := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
	wantBond := preVote + rowSlashedAmt
	t.Logf("[restore-vote] pre-vote bond=%d; expect rise by %d -> %d", preVote, rowSlashedAmt, wantBond)

	// 3) Each NON-attacker witness votes to restore by broadcasting vsc.slash_restore
	// from its OWN account. Payload is just {id, account}. The slashed account
	// (magi.test3) is the beneficiary and is excluded from the tally, so the
	// effective electorate is the other 5 witnesses (threshold ceil(2*5/3)=4); we
	// vote from all 5 to comfortably cross it. Every devnet witness account is
	// controlled by the initminer key (so the test signs each vote with it) — in
	// production each operator signs with the key that controls their own account.
	payload := map[string]interface{}{"id": slashTxID, "account": attacker}
	voteCount := 0
	for n := 1; n <= cfg.Nodes; n++ {
		voter := d.witnessAccount(n)
		if voter == attacker {
			continue // beneficiary cannot vote for its own restoration
		}
		txID, err := d.BroadcastCustomJSON("vsc.slash_restore", []string{voter}, payload, d.cfg.InitminerWIF)
		if err != nil {
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
			t.Fatalf("[restore-vote] broadcasting vsc.slash_restore from %s: %v", voter, err)
		}
		voteCount++
		t.Logf("[restore-vote] vote %d/%d from %s tx=%s", voteCount, cfg.Nodes-1, voter, txID)
		time.Sleep(1 * time.Second) // small spacing so votes land in distinct/ordered blocks
	}

	// 4) Poll node-3's bond on ALL nodes until it RISES by the restored incident's
	// amount. The votes must be observed on L1, tallied during block replay, and
	// on crossing 2/3 applySafetySlashReverse re-credits the bond. Allow several
	// leader rotations.
	restoreDeadline := time.Now().Add(8 * time.Minute)
	restored := false
	for time.Now().Before(restoreDeadline) {
		b := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		if b >= wantBond {
			restored = true
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !restored {
		dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
		final := gqlConsensus(t, d.GQLEndpoint(1), "hive:"+attacker)
		t.Fatalf("[restore-vote] node-3 bond never rose to %d after votes (final=%d, preVote=%d, restoredAmt=%d)",
			wantBond, final, preVote, rowSlashedAmt)
	}

	// 5) Cross-node consistency: every node must agree the bond was restored (the
	// reverse landed in a 2/3-ratified block, so all nodes apply it identically),
	// and a safety_slash_consensus_reverse credit row must exist for the account.
	for n := 1; n <= cfg.Nodes; n++ {
		b := gqlConsensus(t, d.GQLEndpoint(n), "hive:"+attacker)
		if b < wantBond {
			dumpNodeLogs(t, d, ctx, cfg.Nodes, 40)
			t.Fatalf("[restore-vote] node-%d disagrees: bond=%d < want=%d (restoration not consistent across nodes)", n, b, wantBond)
		}
	}
	reverseRows := findLedgerTXs(t, d.GQLEndpoint(1), "hive:"+attacker, []string{"safety_slash_consensus_reverse"})
	var reverseTotal int64
	for _, r := range reverseRows {
		if r.Amount > 0 && r.Asset == "hive_consensus" {
			reverseTotal += r.Amount
		}
	}
	if reverseTotal < rowSlashedAmt {
		t.Fatalf("[restore-vote] expected a consensus reverse credit of >= %d, got %d", rowSlashedAmt, reverseTotal)
	}
	t.Logf("[restore-vote] SUCCESS: node-3 bond restored to >= %d on all %d nodes via %d witness votes; reverse credit total=%d",
		wantBond, cfg.Nodes, voteCount, reverseTotal)
}
