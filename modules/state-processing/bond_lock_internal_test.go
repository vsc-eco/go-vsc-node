package state_engine

import (
	"errors"
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/vaultrotation"

	"go.mongodb.org/mongo-driver/mongo"
)

// TestIsTransientReadErr — #11 council F2 fail-stop classification. This is the
// load-bearing halt-safety + fork-safety invariant: a per-node infra error must be
// treated as TRANSIENT (block/retry, so nodes converge → no fork), while a
// mongo.ErrNoDocuments is a DETERMINISTIC absence that must NOT be transient — else
// blockingRetry loops forever on a genuinely-absent generation and halts the network.
func TestIsTransientReadErr(t *testing.T) {
	if isTransientReadErr(nil) {
		t.Fatal("nil error is not transient")
	}
	if isTransientReadErr(mongo.ErrNoDocuments) {
		t.Fatal("mongo.ErrNoDocuments is a DETERMINISTIC absence, NOT transient — classifying it transient would infinite-block (network halt)")
	}
	if isTransientReadErr(fmt.Errorf("ctx: %w", mongo.ErrNoDocuments)) {
		t.Fatal("a WRAPPED ErrNoDocuments must still be deterministic absence (errors.Is unwraps)")
	}
	if !isTransientReadErr(errors.New("connection refused")) {
		t.Fatal("a generic infra error MUST be transient (block/retry to avoid a per-node fork)")
	}
	if !isTransientReadErr(fmt.Errorf("wrapped: %w", errors.New("timeout"))) {
		t.Fatal("a non-ErrNoDocuments wrapped error must be transient")
	}
}

// TestBondLockMatches_StripsHivePrefix — #11 council F1 regression guard. A
// consensus_unstake's tx.From is "hive:<account>" but the retiring-committee set is
// keyed by BARE election account names; the gate must strip the prefix before the
// lookup or it is dead code (matches nothing, every witness unstakes freely). This
// is the positive-path coverage the original commit lacked (its only test exercised
// the flag-off inert path, which returns false before the lookup and so could not
// tell "inert" from "matches nothing").
func TestBondLockMatches_StripsHivePrefix(t *testing.T) {
	// A set keyed by the BARE account name, exactly as ComputeRetiringSignerSet
	// produces (elections.ElectionMember.Account is bare).
	set := vaultrotation.RetiringSignerSet{
		SignerElection: map[string]elections.ElectionResult{"alice": {}},
		KeyIds:         map[string]bool{},
	}

	if !bondLockMatches(set, "hive:alice") {
		t.Fatal("hive:alice must MATCH the bare-keyed 'alice' after the prefix strip (the dead-gate bug)")
	}
	if !bondLockMatches(set, "alice") {
		t.Fatal("bare 'alice' must also match (defensive)")
	}
	if bondLockMatches(set, "hive:bob") {
		t.Fatal("hive:bob is not a committee member; must NOT be locked")
	}
	empty := vaultrotation.RetiringSignerSet{SignerElection: map[string]elections.ElectionResult{}}
	if bondLockMatches(empty, "hive:alice") {
		t.Fatal("an empty committee set must never lock anyone")
	}
}
