package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/vaultrotation"
)

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
