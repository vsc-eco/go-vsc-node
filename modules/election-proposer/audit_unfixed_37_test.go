package election_proposer

import (
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
)

// audit #37 — No max committee size cap in election proposer.
//
// GenerateFullElection (modules/election-proposer/election-proposer.go:228-326)
// builds the elected committee from every staked witness with no upper bound.
// The proposer does not consult any MaxMembers / committee-size cap from
// ConsensusParams or a config constant, so an arbitrary number of witnesses
// (e.g. 200, 500, 1000) all end up in the active committee — driving up
// per-block signature aggregation cost and BLS verification time linearly
// with witness population.
//
// Differential: today this test PASSES (RED for the audit) because committee
// size equals the full staked witness count. Post-fix, GenerateFullElection
// should clamp the returned member count to a configured constant (e.g.
// ConsensusParams.MaxMembers) and this test will need to be updated to
// reflect the cap (e.g. assert len(members) == cap when stakedCount > cap).
func TestAuditUnfixed_37_NoMaxCommitteeSizeCap(t *testing.T) {
	// A real, parseable consensus BlsDID, reused across all 200 witnesses
	// (the proposer doesn't enforce key-uniqueness, only valid parsing).
	const validConsensusDID = "did:key:z3tEFFFAKtRc8B9oXCM7LiH4xYKW4zNrKcE2p1S6PzJdcz5icZBCj4akv6w5feJ6mQhopG"

	const N = 200

	witnessList := make([]witnesses.Witness, 0, N)
	balanceDb := &test_utils.MockBalanceDb{
		BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
	}

	for i := 0; i < N; i++ {
		account := fmt.Sprintf("witness-%04d", i)
		witnessList = append(witnessList, witnesses.Witness{
			Account: account,
			Enabled: true,
			DidKeys: []witnesses.PostingJsonKeys{
				{CryptoType: "bls", Type: "consensus", Key: validConsensusDID},
			},
		})
		// Stake each witness above MinStake (mocknet MinStake=1) so they
		// all qualify for the staked weight map.
		_ = balanceDb.UpdateBalanceRecord(ledgerDb.BalanceRecord{
			Account:        "hive:" + account,
			BlockHeight:    1,
			HIVE_CONSENSUS: 100,
		})
	}

	// Reuse the contract test harness purely for its wired-up DataLayer
	// (GenerateFullElection persists the election CBOR after building the
	// member list). Mocknet config: MinMembers=3, MinStake=1.
	ct := test_utils.NewContractTest()

	ep := New(
		nil,                          // p2p
		&test_utils.MockWitnessDb{},  // witnesses (unused by GenerateFullElection)
		&test_utils.MockElectionDb{}, // elections → GetElection nil = first election
		nil,                          // vscBlocks
		balanceDb,                    // balanceDb → all 200 witnesses staked
		ct.DataLayer,                 // da
		nil,                          // txCreator
		nil,                          // conf
		systemconfig.MocknetConfig(), // sconf
		nil,                          // se
		nil,                          // hiveConsumer
	)

	_, data, err := ep.GenerateFullElection(witnessList, 0, consensusversion.Version{}, 100)
	if err != nil {
		t.Fatalf("audit #37: GenerateFullElection returned error: %v", err)
	}

	// Pre-fix expectation: every staked witness ends up in the committee
	// (no cap). Post-fix, this should clamp to a configured constant —
	// when that lands, change the assertion to len(data.Members) <= cap
	// and len(data.Weights) <= cap.
	if len(data.Members) != N {
		t.Fatalf("audit #37: committee size = %d, want %d (no cap is currently enforced; "+
			"post-fix this assertion must change to verify the clamp)",
			len(data.Members), N)
	}
	if len(data.Weights) != N {
		t.Fatalf("audit #37: weights len = %d, want %d", len(data.Weights), N)
	}
	if data.Type != "staked" {
		t.Fatalf("audit #37: election type = %q, want %q", data.Type, "staked")
	}
}
