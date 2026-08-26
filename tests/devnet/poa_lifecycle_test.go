package devnet

import (
	"context"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
)

// admitVote broadcasts vsc.admit_vote from a witness account.
func (d *Devnet) admitVote(voterWitness int, candidate, uboId string) (string, error) {
	voter := d.witnessAccount(voterWitness)
	payload := map[string]interface{}{
		"candidate": candidate,
		"ubo_id":    uboId,
		"net_id":    d.netId(),
	}
	return d.BroadcastCustomJSON("vsc.admit_vote", []string{voter}, payload, d.cfg.InitminerWIF)
}

// TestPoaLifecycle bundles the POA behaviours that do NOT halt the chain onto a
// single devnet, because each devnet boot costs ~6 minutes. Ordering is
// deliberate and load-bearing: nothing here may halt finality, or the subtests
// after it would fail for the wrong reason. The chain-halting scenarios (D10
// flat-weight quorum, D14 uncapped departures) live in their own tests.
//
// Covers D19 (duplicate admission), D20 (admission without quorum) and D13a
// (a seated operator's consensus_unstake is held by the exit-halt).
func TestPoaLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	cfg.SkipFunding = false // D13a needs real L2 balances to attempt an unstake
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// ★ FloorEpoch MUST be non-zero: PinnedVersionFloor treats 0 as "no floor".
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 40*time.Minute)

	// POA needs TWO epochs: the registry seeds at the activation election (epoch 1)
	// but flat weight is gated on the PRIOR election's version, so it lands at
	// epoch 2. See poa_bootstrap_test.go for the full explanation.
	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 10*time.Minute)
		cancel()
		if err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}

	// ---- PRECONDITION: POA genuinely active ----
	elec, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil {
		t.Fatalf("reading election epoch 2: %v", err)
	}
	for i, w := range elec.Weights {
		if w != params.PoaSeatWeight {
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d — POA INERT, every subtest "+
				"below would be vacuous", i, w, params.PoaSeatWeight)
		}
	}
	seats0, err := d.poaSeats(ctx, 1)
	if err != nil {
		t.Fatalf("reading poa_seats: %v", err)
	}
	if len(seats0) == 0 {
		t.Fatal("PRECONDITION FAILED: registry empty while POA is active")
	}
	t.Logf("POA ACTIVE: %d flat seats, registry has %d rows", len(elec.Members), len(seats0))

	// required admit votes = ceil(2/3) of seats, beneficiary excluded
	total := uint64(len(seats0))
	required := total - total/3
	t.Logf("admission threshold: %d of %d seats", required, total)

	// ---- D19: admitting an account that is ALREADY a seat must not wedge ----
	t.Run("D19_duplicate_admission_does_not_wedge", func(t *testing.T) {
		existing := d.witnessAccount(1) // already a bootstrap seat
		for v := 1; v <= int(required); v++ {
			if _, err := d.admitVote(v, existing, "ubo-duplicate-test"); err != nil {
				t.Fatalf("admit_vote from magi-%d: %v", v, err)
			}
		}
		// ★ WHAT THIS ACTUALLY EXERCISES — corrected. An already-seated candidate is
		// rejected in the VOTE path, before a proposal is ever opened
		// (poa_admission.go: "Already-seated candidate, or a UBO that already holds
		// a seat: the vote is moot. Checked BEFORE opening a proposal so a duplicate
		// never accumulates"). It does NOT reach the seat-WRITE path where the typed
		// dup-seat sentinels guard blockingRetry, so this test must not be cited as
		// covering those. What it does cover: duplicate votes neither grow the
		// registry, nor re-stamp an existing seat, nor stall the chain.
		time.Sleep(45 * time.Second)
		after, err := d.poaSeats(ctx, 1)
		if err != nil {
			t.Fatalf("re-reading poa_seats: %v", err)
		}
		if len(after) != len(seats0) {
			t.Errorf("registry grew from %d to %d on a DUPLICATE admission: %s",
				len(seats0), len(after), seatFingerprint(after))
		}
		// Stronger than a count: the existing seat must be untouched. A duplicate
		// that silently re-stamped admitted_height would keep the count identical
		// while rewriting history, and every node's fingerprint would shift.
		if before, afterFp := seatFingerprint(seats0), seatFingerprint(after); before != afterFp {
			t.Errorf("DUPLICATE ADMISSION MUTATED AN EXISTING SEAT (count unchanged, contents "+
				"changed):\n  before: %s\n  after:  %s", before, afterFp)
		}
		grew, from, to := d.grewWithin(ctx, t, 1, 2*time.Minute)
		if !grew {
			t.Errorf("chain STALLED after a duplicate admission (height %d -> %d) — this is the "+
				"blockingRetry wedge the typed dup-seat sentinels exist to prevent", from, to)
		} else {
			t.Logf("registry unchanged at %d seats; chain still advancing %d -> %d", len(after), from, to)
		}
	})

	// ---- D20: an admission that never reaches quorum must not half-apply ----
	t.Run("D20_admission_below_quorum_never_seats", func(t *testing.T) {
		before, err := d.poaSeats(ctx, 1)
		if err != nil {
			t.Fatalf("poa_seats: %v", err)
		}
		candidate := "magi.newbie"
		votes := int(required) - 1 // deliberately one short
		if votes < 1 {
			t.Skipf("threshold %d leaves no room for a below-quorum test", required)
		}
		for v := 1; v <= votes; v++ {
			if _, err := d.admitVote(v, candidate, "ubo-newbie-001"); err != nil {
				t.Fatalf("admit_vote from magi-%d: %v", v, err)
			}
		}
		time.Sleep(45 * time.Second)
		after, err := d.poaSeats(ctx, 1)
		if err != nil {
			t.Fatalf("poa_seats: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("candidate was SEATED on %d of %d votes (threshold is %d): %s",
				votes, total, required, seatFingerprint(after))
		}
		for _, s := range after {
			if s.Account == candidate {
				t.Errorf("candidate %q present in the registry despite being one vote short", candidate)
			}
		}
		t.Logf("candidate not seated on %d/%d votes (threshold %d) — correct", votes, total, required)
	})

	// ---- D13a: a seated operator's consensus_unstake is HELD by the exit-halt ----
	t.Run("D13a_seated_unstake_is_held", func(t *testing.T) {
		acct := "hive:" + d.witnessAccount(2)
		bal, err := d.GetAccountBalance(ctx, 1, acct)
		// ★ FAIL, do not SKIP. A skipped subtest is indistinguishable from a
		// passing one in a summary, and the first run of this test skipped here
		// silently while the parent reported PASS -- the exit-halt was never
		// exercised at all. Per feedback_vacuous_pass_is_worse_than_fail, a check
		// with nothing to inspect must FAIL.
		if err != nil || bal == nil {
			t.Fatalf("cannot read balance for %s (err=%v). This account is a SEATED committee "+
				"member, so it must have a readable ledger balance; not being able to read one is "+
				"itself the finding.", acct, err)
		}
		before := bal.HiveConsensus
		if before <= 0 {
			t.Fatalf("%s reports hive_consensus=%d, but it is an ELECTED committee member and "+
				"therefore must hold at least MinStake. Either the balance read is wrong or the "+
				"election admitted an unstaked member -- both are findings, neither is a skip.",
				acct, before)
		}
		t.Logf("%s hive_consensus before unstake attempt: %d", acct, before)

		txId, err := d.ConsensusUnstake(2, "1.000")
		if err != nil {
			t.Fatalf("broadcasting consensus_unstake: %v", err)
		}
		time.Sleep(60 * time.Second)

		// ★ ASSERT THE REASON, NOT JUST THE ABSENCE OF AN EFFECT.
		// "hive_consensus did not move" is equally consistent with the tx simply
		// not having been processed yet, so on its own it is not evidence the
		// exit-halt fired. The refusal is a concrete TxResult
		// (transactions.go:887-899, Success:false with a message beginning
		// "consensus bond is locked: POA collateral exit-halt"), so require the
		// transaction to have actually been PROCESSED and to have failed.
		status, serr := d.FindTransactionStatus(ctx, 1, txId)
		if serr != nil {
			t.Errorf("could not read status for the unstake tx %s (%v). Without it, an unchanged "+
				"balance does not distinguish 'the exit-halt refused it' from 'it has not been "+
				"processed yet'.", txId, serr)
		} else {
			t.Logf("unstake tx %s status=%s", txId, status)
			if strings.EqualFold(status, "UNCONFIRMED") || status == "" {
				t.Errorf("unstake tx %s is still %q after 60s — the balance check below cannot "+
					"distinguish a working exit-halt from an unprocessed transaction. Treat this "+
					"run as INCONCLUSIVE rather than a pass.", txId, status)
			}
		}

		bal2, err := d.GetAccountBalance(ctx, 1, acct)
		if err != nil || bal2 == nil {
			t.Fatalf("re-reading balance: %v", err)
		}
		if bal2.HiveConsensus < before {
			t.Errorf("EXIT-HALT DID NOT HOLD: hive_consensus fell %d -> %d for a SEATED operator. "+
				"An unstake that drains the bond while the account is still seated is the RG-1 shape: "+
				"a slash afterwards targets a zero bond and silently no-ops.",
				before, bal2.HiveConsensus)
		} else {
			t.Logf("exit-halt held: hive_consensus unchanged at %d for a seated operator", bal2.HiveConsensus)
		}
	})
}
