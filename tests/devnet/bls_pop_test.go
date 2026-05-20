package devnet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestBlsProofOfPossessionPropagation verifies the end-to-end fix for the
// rogue-key aggregate-forgery vulnerability described in the audit's B1
// finding (deep audit CRIT) and closed by commit d47f87e5
// ("proof of posession system in BLS announce").
//
// The fix has three moving pieces:
//
//  1. The announce payload (modules/announcements/announcements.go) now
//     emits a BLS proof-of-possession (PoP) — a signature over a fixed
//     domain tag concatenated with the announced BLS pubkey and the
//     announcing Hive account.
//  2. The witness schema (modules/db/vsc/witnesses/schema.go) carries a
//     `pop` field on every consensus BLS key.
//  3. The state engine (modules/state-processing/state_engine.go) calls
//     dids.VerifyBlsPoP on ingest. Today this is warn-only — a missing or
//     invalid PoP is logged but the witness is still stored — so the
//     rollout can complete without dropping pre-PoP witnesses. A later
//     change flips this to strict reject.
//
// The unit tests in lib/dids/bls_test.go already cover the crypto layer
// (round-trip, wrong-account, wrong-key, malformed). This test fills the
// integration gap: do real announcements published to Hive flow through
// the announcement→ingest→Mongo path with a *valid* PoP attached to every
// witness, and does dids.VerifyBlsPoP confirm them?
//
// Run with:
//
//	go test -v -run TestBlsProofOfPossessionPropagation -timeout 20m ./tests/devnet/
func TestBlsProofOfPossessionPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := tssTestConfig()
	d, ctx := startDevnet(t, cfg, 15*time.Minute)

	// Wait for keygen — by the time keygen completes, every witness has
	// published its v2 announce (the one bearing the PoP) and the state
	// engine has had ample time to ingest it. If a witness has not
	// announced, keygen can't pick it.
	t.Log("waiting for keygen to gate on full witness announce ingestion...")
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	t.Logf("keygen ready at block %d (epoch %d), all %d witnesses have announced",
		keygen.BlockHeight, keygen.Epoch, cfg.Nodes)

	// Verify each node's view of the witnesses collection independently.
	// We don't rely on cross-node agreement — the ingest is deterministic,
	// but we want a per-node failure to surface as a per-node assertion
	// rather than be masked by the others.
	for n := 1; n <= cfg.Nodes; n++ {
		dbName := fmt.Sprintf("magi-%d", n)
		t.Run(dbName, func(t *testing.T) {
			assertAllWitnessesHaveValidBlsPoP(t, d.MongoURI(), dbName, cfg.Nodes)
		})
	}

	// The state engine's verifyAnnouncedBlsPoP only WARNs on failure
	// during rollout — it doesn't reject. Confirm no node has actually
	// logged the warning, i.e. every announce carried a valid PoP. (If
	// this ever starts failing, it means an announce flowed in without a
	// valid PoP — investigate before flipping the rollout to strict.)
	t.Run("no_pop_warnings_in_node_logs", func(t *testing.T) {
		// Use a stable substring from the state_engine.go warn line.
		const warnSubstr = "witness announce: BLS proof-of-possession check failed"
		for n := 1; n <= cfg.Nodes; n++ {
			logs, err := d.Logs(ctx, fmt.Sprintf("magi-%d", n))
			if err != nil {
				t.Logf("warning: could not read magi-%d logs: %v", n, err)
				continue
			}
			if strings.Contains(logs, warnSubstr) {
				// Surface the surrounding context so we can see WHY it
				// failed without dumping the entire log buffer.
				for _, line := range strings.Split(logs, "\n") {
					if strings.Contains(line, warnSubstr) {
						t.Errorf("magi-%d logged PoP failure: %s", n, line)
					}
				}
			}
		}
	})
}

// assertAllWitnessesHaveValidBlsPoP queries the named node's witnesses
// collection, requires exactly `wantWitnesses` records, and for each one
// extracts the DID-BLS / consensus key and runs the same VerifyBlsPoP
// check the state engine runs at ingest time. This catches drift between
// the announcer (lib/dids GenerateBlsPoP) and the verifier (state_engine
// + lib/dids VerifyBlsPoP), and confirms the on-disk schema field round-
// trips through BSON intact.
func assertAllWitnessesHaveValidBlsPoP(t *testing.T, mongoURI, dbName string, wantWitnesses int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, _ := connectMongo(t, mongoURI)
	defer client.Disconnect(ctx)

	coll := client.Database(dbName).Collection("witnesses")

	// The `witnesses` collection is append-style: one row per announce, keyed by
	// (account, height). A witness that re-announces (e.g. property change)
	// leaves multiple `enabled:true` rows. We only care about the *latest*
	// announce per account — that's the one the state engine's
	// verifyAnnouncedBlsPoP ran on most recently and the one downstream
	// consumers (elections, BLS circuit) read.
	cursor, err := coll.Find(ctx, bson.M{"enabled": true},
		options.Find().SetSort(bson.D{
			{Key: "account", Value: 1},
			{Key: "height", Value: -1},
		}))
	if err != nil {
		t.Fatalf("find witnesses in %s: %v", dbName, err)
	}
	defer cursor.Close(ctx)

	checkedAccounts := make(map[string]struct{}, wantWitnesses)
	for cursor.Next(ctx) {
		var w witnesses.Witness
		if err := cursor.Decode(&w); err != nil {
			t.Fatalf("decode witness in %s: %v", dbName, err)
		}
		// height-desc sort means the first row we see per account is the
		// latest announce; skip older rows for the same account.
		if _, already := checkedAccounts[w.Account]; already {
			continue
		}
		checkedAccounts[w.Account] = struct{}{}

		// Find the DID-BLS / consensus key — the exact one the state
		// engine's verifyAnnouncedBlsPoP looks up.
		var blsKey *witnesses.PostingJsonKeys
		for i := range w.DidKeys {
			k := &w.DidKeys[i]
			if k.CryptoType == "DID-BLS" && k.Type == "consensus" {
				blsKey = k
				break
			}
		}
		if blsKey == nil {
			t.Errorf("%s: witness %q has no DID-BLS/consensus key in did_keys",
				dbName, w.Account)
			continue
		}

		// Three independent sub-checks so a failure points cleanly at
		// the layer that broke (announce / schema / verify).
		if blsKey.PoP == "" {
			t.Errorf("%s: witness %q has empty pop field (announce or schema regression)",
				dbName, w.Account)
			continue
		}
		if err := dids.VerifyBlsPoP(dids.BlsDID(blsKey.Key), w.Account, blsKey.PoP); err != nil {
			t.Errorf("%s: witness %q PoP fails VerifyBlsPoP: %v (account binding, key, or domain tag drift)",
				dbName, w.Account, err)
			continue
		}
		t.Logf("%s: witness %q has valid PoP at height %d (key=%s)",
			dbName, w.Account, w.Height, truncateMid(string(blsKey.Key), 16))
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("cursor err in %s: %v", dbName, err)
	}

	if got := len(checkedAccounts); got != wantWitnesses {
		t.Errorf("%s: expected %d distinct enabled witness accounts, found %d", dbName, wantWitnesses, got)
	}
}

// truncateMid keeps the first and last few chars of a long DID string for
// readable log output; the full DID is ~75 chars of base64 noise.
func truncateMid(s string, keep int) string {
	if len(s) <= keep*2+3 {
		return s
	}
	return s[:keep] + "..." + s[len(s)-keep:]
}

// TestBlsPoPRejectsRogueKeyAnnounce documents the property the warn-only
// rollout is preparing for: an announce that carries a PoP signed by the
// wrong key (the rogue-key attack precondition) MUST fail
// dids.VerifyBlsPoP, so when the rollout flips to strict mode the rogue
// announce will be rejected by the state engine.
//
// This is the unit-level equivalent of TestBlsPoPWrongKeyFails in
// lib/dids/bls_test.go, but framed against the ingest-path verifier so a
// future reader of the devnet test directory can find the
// rogue-key-rejection guarantee at the layer where it'll matter on
// mainnet.
//
// Once the warn-only rollout flips to strict, the body should also assert
// (via a devnet harness change) that a witness ingested with an invalid
// PoP is NOT written to the witnesses collection. Until then, this stays
// at the crypto-layer assertion only.
func TestBlsPoPRejectsRogueKeyAnnounce(t *testing.T) {
	did1, priv1 := freshBlsKey(t, "rogue_key_seed_1_aaaaaaaa")
	_, priv2 := freshBlsKey(t, "rogue_key_seed_2_bbbbbbbb")

	// Real PoP from key1 verifies for did1/alice — sanity check.
	pop1, err := dids.GenerateBlsPoP(priv1, "alice")
	if err != nil {
		t.Fatalf("generate pop1: %v", err)
	}
	if err := dids.VerifyBlsPoP(did1, "alice", pop1); err != nil {
		t.Fatalf("sanity: own PoP must verify, got err: %v", err)
	}

	// PoP from key2 presented under did1 (the rogue-key precondition) MUST fail.
	pop2, err := dids.GenerateBlsPoP(priv2, "alice")
	if err != nil {
		t.Fatalf("generate pop2: %v", err)
	}
	if err := dids.VerifyBlsPoP(did1, "alice", pop2); err == nil {
		t.Fatal("verifier accepted a PoP signed by the wrong key — rogue-key defense is broken")
	}

	// Same key, wrong account — PoP must fail (account binding).
	if err := dids.VerifyBlsPoP(did1, "mallory", pop1); err == nil {
		t.Fatal("verifier accepted a PoP under a different account — account binding is broken")
	}
}

// freshBlsKey deterministically derives a BLS keypair from a short
// human-readable seed (padded to 32 bytes) and returns the matching DID.
// Mirrors the helper in lib/dids/bls_test.go so this devnet test can
// stand alone without the test_utils import cycle.
func freshBlsKey(t *testing.T, seedStr string) (dids.BlsDID, *dids.BlsPrivKey) {
	t.Helper()
	var seed [32]byte
	copy(seed[:], []byte(seedStr))

	privKey := dids.BlsPrivKey{}
	privKey.Deserialize(&seed)

	pubKey, err := ethBls.SkToPk(&privKey)
	if err != nil {
		t.Fatalf("derive pubkey from seed %q: %v", seedStr, err)
	}
	did, err := dids.NewBlsDID(pubKey)
	if err != nil {
		t.Fatalf("derive DID from seed %q: %v", seedStr, err)
	}
	return did, &privKey
}
