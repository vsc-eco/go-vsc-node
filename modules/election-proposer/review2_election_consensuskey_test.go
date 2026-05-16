package election_proposer

import (
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"
)

// review2 MEDIUM #66 — GenerateFullElection built its member list with
// utils.Map + panic(err) on w.ConsensusKey() failure. A witness consensus
// key is untrusted L1 account data; a single witness with a missing or
// malformed consensus key panicked the election proposer on every node,
// so no election could ever be produced — a network-wide liveness break
// triggerable by any account that can register as a witness.
//
// Differential: on the #170 baseline the bad-key witness panics
// GenerateFullElection (RED); on fix/review2 it is deterministically
// skipped and a valid election with the remaining members is returned
// (GREEN). All nodes read identical witness records, so the skip is
// consensus-consistent.
func TestReview2GenerateFullElectionBadConsensusKey(t *testing.T) {
	// A real, parseable consensus BlsDID (verified to ParseBlsDID cleanly).
	const validConsensusDID = "did:key:z3tEFFFAKtRc8B9oXCM7LiH4xYKW4zNrKcE2p1S6PzJdcz5icZBCj4akv6w5feJ6mQhopG"

	good := witnesses.Witness{
		Account: "good-witness",
		Enabled: true,
		DidKeys: []witnesses.PostingJsonKeys{
			{CryptoType: "bls", Type: "consensus", Key: validConsensusDID},
		},
	}
	// No "consensus" entry → ConsensusKey() returns an error.
	bad := witnesses.Witness{
		Account: "bad-witness",
		Enabled: true,
		DidKeys: []witnesses.PostingJsonKeys{
			{CryptoType: "bls", Type: "posting", Key: "irrelevant"},
		},
	}

	// Reuse the contract test harness purely for its wired-up DataLayer
	// (GenerateFullElection persists the election CBOR after building
	// members; the rest is exercised with controlled mocks).
	ct := test_utils.NewContractTest()

	ep := New(
		nil,                          // p2p
		&test_utils.MockWitnessDb{},  // witnesses (unused by GenerateFullElection)
		&test_utils.MockElectionDb{}, // elections → GetElection nil = first election
		nil,                          // vscBlocks
		&test_utils.MockBalanceDb{},  // balanceDb → no stake records
		ct.DataLayer,                 // da
		nil,                          // txCreator
		nil,                          // conf
		systemconfig.MocknetConfig(), // sconf
		nil,                          // se
		nil,                          // hiveConsumer
	)

	var (
		members []string
		callErr error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("review2 #66: GenerateFullElection panicked on a bad consensus key: %v", r)
			}
		}()
		_, data, err := ep.GenerateFullElection([]witnesses.Witness{bad, good}, 0, 0, 100)
		callErr = err
		for _, m := range data.Members {
			members = append(members, m.Account)
		}
	}()

	if callErr != nil {
		t.Fatalf("review2 #66: GenerateFullElection returned error: %v", callErr)
	}
	if len(members) != 1 || members[0] != "good-witness" {
		t.Fatalf("review2 #66: members = %v, want [good-witness] (bad-key witness skipped)", members)
	}
}
