package devnet

import (
	"os"
	"testing"
)

// TestConsensusVersionDevnetManual is a placeholder for full-stack verification against a
// running devnet (Docker). It does not start infrastructure by default.
//
// Manual checklist (operators):
//  1. Set consensusParams.recoveryMultisigAccounts and recoveryMultisigThreshold in node sysconfig.
//  2. Broadcast vsc.recovery_suspend with required_auths satisfying M-of-N.
//  3. Query GraphQL localNodeInfo: processing_suspended=true, consensus_version_display ends with "-p".
//  4. Confirm offchain VSC tx submission returns "suspended" from the transaction pool.
//  5. Broadcast vsc.recovery_require_version with the same multisig; verify processing_suspended=false
//     and adopted version matches payload.
//  6. Submit vsc.propose_consensus_version from a committee account; observe pending then adoption
//     after enough witnesses announce the target triple on posting JSON.
//
// Optional automated run (starts devnet — slow, needs Docker):
//
//	CONSENSUS_DEVNET_E2E=1 go test -v -run TestConsensusVersionDevnetManual -timeout 25m ./tests/devnet/
func TestConsensusVersionDevnetManual(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet consensus test in short mode")
	}
	if os.Getenv("CONSENSUS_DEVNET_E2E") == "" {
		t.Skip("set CONSENSUS_DEVNET_E2E=1 to run devnet-backed consensus checks (see doc comment)")
	}

	t.Log("CONSENSUS_DEVNET_E2E: add scripted Hive custom_json + GraphQL assertions here when multisig test keys are available in devnet harness")
}
