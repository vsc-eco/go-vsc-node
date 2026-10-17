package gqlgen

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// A dry run must read TssGetKey exactly as the chain does at that height. The
// devnet (2026-10-13) showed simulateContractCalls refusing migrateVault as "not
// reshared" with the key at epoch 38 and its keygen at epoch 7, while the real
// call passed that check: the preview never got the 0.11.0 fifth field, so the
// mapping bot, which dry-runs migrateVault first, would have waited forever.
func TestSimulateTssKeyOptsMatchTheChain(t *testing.T) {
	const contractId, keyName = "vsc1Vault", "mainv1"
	const fullKey = contractId + "-" + keyName
	pub := "02aa"
	keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{fullKey: {
		Id: fullKey, Status: "active", PublicKey: pub, Algo: tss_db.EcdsaType, Epoch: 38, SignatureVerified: true,
	}}}
	commitments := &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{
		"0": {Type: "keygen", KeyId: fullKey, Epoch: 7, BlockHeight: 420, PublicKey: &pub},
		"1": {Type: "reshare", KeyId: fullKey, Epoch: 38, BlockHeight: 1980},
	}}
	v := func(c uint64) consensusversion.Version { return consensusversion.Version{Consensus: c} }

	cases := []struct {
		name   string
		pin    uint64
		active consensusversion.Version
		want   string
	}{
		{"0.3.0, no pin: legacy three fields", 0, v(3), "active,02aa,ecdsa"},
		{"0.10.0: check-signature field only", 0, v(10), "active,02aa,ecdsa,1"},
		{"0.11.0: the reshared field too", 0, v(11), "active,02aa,ecdsa,1,1"},
		{"v2 height pin (ephemeral network): both", 400, v(3), "active,02aa,ecdsa,1,1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := params.ConsensusParams{VaultRotationV2ActivationHeight: tc.pin}
			opts := simulateTssKeyOpts(cp, 2000, tc.active)
			ctx := contract_execution_context.New(contract_execution_context.Environment{ContractId: contractId}, 1000, 100000, 100000, nil,
				contract_session.NewCallSession(nil, nil, nil, keys, commitments, 0, nil), 0, opts...)
			res := ctx.TssGetKey(keyName)
			if res.IsErr() {
				t.Fatalf("TssGetKey: %v", res.UnwrapErr())
			}
			if got := res.Unwrap(); got != tc.want {
				t.Fatalf("simulated TssGetKey = %q, want %q", got, tc.want)
			}
		})
	}
}
