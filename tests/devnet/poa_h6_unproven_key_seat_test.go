package devnet

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"

	"go.mongodb.org/mongo-driver/bson"
)

// breakGatewayPoPEverywhere writes an INVALID gateway_key_pop for an account into
// EVERY node's witnesses collection.
//
// Deliberately every node, not just magi-1 as poisonGatewayKey does. Witness
// records are consensus input: election generation is a pure function of them and
// every node must derive the identical committee/CID. Poisoning one node's view
// would manufacture an artificial divergence and the test would be measuring my
// own tampering rather than the H-6 gate.
func breakGatewayPoPEverywhere(t *testing.T, d *Devnet, ctx context.Context, account string, nodes int) int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	total := 0
	for n := 1; n <= nodes; n++ {
		coll := client.Database(d.nodeDbName(n)).Collection("witnesses")
		res, err := coll.UpdateMany(ctx,
			bson.M{"account": account},
			// Well-formed-looking but bound to nothing: VerifyGatewayKeyPoP fails.
			//
			// ★ CORRECTION (2026-08-26): an earlier version of this comment claimed
			// an EMPTY pop "reads as announced-before-gateway-PoP-support, which the
			// gate is written to tolerate". That is FALSE of the current code.
			// dids.VerifyGatewayKeyPoP returns a hard error for an empty pop AND for
			// an empty gateway key (lib/dids/gateway_pop.go:79-84), and the election
			// gate excludes on ANY non-nil error (election-proposer.go:602). Two
			// existing tests already pin the strict behaviour:
			// witnesses/gateway_pop_test.go:30 ("missing pop (legacy announce)" =>
			// must fail) and lib/dids/gateway_pop_test.go:58 ("empty PoP accepted" =>
			// t.Fatal). The tolerant reading came from the SCHEMA comment at
			// witnesses/schema.go:21-22, which describes what the FIELD may contain,
			// not what the GATE does. There is no grandfathering to remove, and none
			// should be added: it would be the "announce a stolen key, omit the PoP"
			// doorway.
			bson.M{"$set": bson.M{"gateway_key_pop": "3045022100deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0220cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"}},
		)
		if err != nil {
			t.Fatalf("breaking PoP on magi-%d: %v", n, err)
		}
		total += int(res.MatchedCount)
	}
	return total
}

// TestPoaH6DisabledSeatsUnprovenKey is scenario D15.
//
// ★★ THIS IS A CHARACTERISATION TEST. It PINS CURRENT, UNDESIRABLE BEHAVIOUR.
// When H-6 is re-enabled this test MUST BE INVERTED, not deleted. A
// characterisation test nobody inverts silently blesses the gap forever.
//
// consensusversion.WitnessKeyStrictActive is stubbed `return false`
// (feature_gates.go, commit 9801c292, 2026-06-22) after the strict PoP gate
// starved the mainnet committee below the floor at epoch 1699 and halted
// elections. The gate BODY at election-proposer.go:565 is intact; only the
// predicate is dead, so the whole exclusion block is skipped.
//
// That interacts badly with POA specifically. bootstrapPoaSeats seeds the
// PERMANENT, APPEND-ONLY seat registry from whatever the ratified election
// contained, and performs NO key validation of its own (verified: no
// Key/PoP/Verify reference anywhere in its body). So a witness whose gateway key
// carries no valid proof-of-possession — one nobody has shown they hold the
// secret for — is written into a registry that has no delete path, and its seat
// survives every future election.
//
// On a 5-witness testnet the practical risk is low because the cohort is known.
// Before MAINNET POA this must be closed, because the founding cohort there is
// whoever happens to be elected at one hand-picked height.
func TestPoaH6DisabledSeatsUnprovenKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	// Guard the premise itself: if H-6 has been re-enabled since this was
	// written, the whole test is obsolete and must be inverted rather than run.
	if consensusversion.WitnessKeyStrictActive(consensusversion.V0_7_0) {
		t.Fatal("H-6 IS NOW ENABLED (WitnessKeyStrictActive returned true). This characterisation " +
			"test pins the DISABLED behaviour and is now WRONG. INVERT IT: assert that the witness " +
			"with the invalid gateway PoP is EXCLUDED from the committee and never seated.")
	}

	cfg := tssTestConfig()
	if cfg.SysConfigOverrides.ConsensusParams == nil {
		cfg.SysConfigOverrides.ConsensusParams = &params.ConsensusParams{}
	}
	// ★ FloorEpoch MUST be non-zero.
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorMajor = 0
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorConsensus = 7
	cfg.SysConfigOverrides.ConsensusParams.ConsensusVersionFloorEpoch = 1

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	victim := d.witnessAccount(cfg.Nodes) // magi.test5

	// Break the PoP BEFORE the activation election is generated. Witness rows
	// appear during setup, so poll briefly for them.
	broke := 0
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if broke = breakGatewayPoPEverywhere(t, d, ctx, victim, cfg.Nodes); broke > 0 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if broke == 0 {
		t.Fatalf("no witness rows for %s to break — cannot establish the premise", victim)
	}
	t.Logf("set an INVALID gateway_key_pop on %d witness row(s) for %s across all %d node databases",
		broke, victim, cfg.Nodes)

	for n := 1; n <= cfg.Nodes; n++ {
		nctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		err := d.waitForElectionEpoch(nctx, n, 2, 12*time.Minute)
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
			t.Fatalf("PRECONDITION FAILED: weight[%d]=%d want flat %d — POA INERT", i, w, params.PoaSeatWeight)
		}
	}

	// ---- CHARACTERISATION: the unproven key is elected AND permanently seated ----
	inCommittee := false
	for _, m := range elec.Members {
		if m == victim || m == "hive:"+victim {
			inCommittee = true
			break
		}
	}
	seats, err := d.poaSeats(ctx, 1)
	if err != nil {
		t.Fatalf("reading poa_seats: %v", err)
	}
	seated := false
	for _, s := range seats {
		if s.Account == victim {
			seated = true
			break
		}
	}
	t.Logf("committee epoch 2: %v", elec.Members)
	t.Logf("registry: %s", seatFingerprint(seats))

	if !inCommittee && !seated {
		t.Errorf("UNEXPECTED: %s was excluded despite WitnessKeyStrictActive being false. Something "+
			"else is filtering it, so this test is no longer characterising what it claims. "+
			"Investigate before trusting either outcome.", victim)
		return
	}

	if inCommittee && seated {
		t.Logf("CHARACTERISED (this is the CURRENT, UNDESIRABLE behaviour): %s holds an INVALID "+
			"gateway-key proof-of-possession, yet it was elected AND written into the permanent, "+
			"append-only POA seat registry. There is no delete path, so this seat is now structural. "+
			"Closing H-6 (restoring WitnessKeyStrictActive) is a PRE-MAINNET-POA requirement: the "+
			"mainnet founding cohort is whoever is elected at one hand-picked height, and this is "+
			"exactly how an operator who cannot prove it holds its own keys becomes permanent.",
			victim)
		t.Logf("INVERT-WHEN-FIXED: when WitnessKeyStrictActive returns true again, change the two "+
			"assertions above to require %s is ABSENT from both the committee and the registry.", victim)
		return
	}

	t.Errorf("SPLIT STATE: %s inCommittee=%v seated=%v. The committee and the registry disagree about "+
		"an account with an invalid key, which is worse than either consistent outcome.",
		victim, inCommittee, seated)
}
