package gqlgen

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	tss_db "vsc-node/modules/db/vsc/tss"
	stateEngine "vsc-node/modules/state-processing"
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
			opts := stateEngine.ContractCallGatesFor(true, cp, nil, 2000, tc.active)
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

// SIM-TRY-1 (2026-11-17): the dry run built its execution context without
// try/catch and without the TSS key allowlist, which the chain has run with
// since 0.2.0 and 0.14.0. A dry run of a deposit whose swap the router refuses
// aborted, where the real call catches the refusal and credits the deposit; the
// bots size their calls from dry runs. The dry run now takes its gated options
// from the list the chain uses (stateEngine.ContractCallGates); this pins each
// gate of that list at the version it starts.
func TestSimulateRunsWithTheChainsGates(t *testing.T) {
	v := func(c uint64) consensusversion.Version { return consensusversion.Version{Consensus: c} }
	type gates struct{ try, sp1, big, keyListed bool }
	const listed, unlisted = "vsc1TheBridge", "vsc1SomeoneElse"
	for _, tc := range []struct {
		name   string
		active consensusversion.Version
		want   gates
	}{
		{"0.1.0: nothing yet", v(1), gates{}},
		{"0.2.0: try/catch", v(2), gates{try: true}},
		{"0.10.0: SP1 priced by work", v(10), gates{try: true, sp1: true}},
		{"0.13.0: big swap amounts", v(13), gates{try: true, sp1: true, big: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := stateEngine.ContractCallGatesFor(true, params.ConsensusParams{}, nil, 2000, tc.active)
			ctx := contract_execution_context.New(contract_execution_context.Environment{ContractId: unlisted}, 1000, 100000, 100000, nil,
				contract_session.NewCallSession(nil, nil, nil, &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}, &test_utils.MockTssCommitmentsDb{}, 0, nil), 0, opts...)
			got := gates{try: ctx.TryCatchActive(), sp1: ctx.Sp1WorkPricingActive(), big: ctx.BigSwapAmountsActive()}
			if got != tc.want {
				t.Fatalf("gates %+v, want %+v", got, tc.want)
			}
		})
	}
	// The allowlist the chain resolves reaches the dry run's context: a contract
	// that is not on it is refused, as on chain.
	opts := stateEngine.ContractCallGatesFor(true, params.ConsensusParams{}, []string{listed}, 2000, v(13))
	ctx := contract_execution_context.New(contract_execution_context.Environment{ContractId: unlisted}, 1000, 100000, 100000, nil,
		contract_session.NewCallSession(nil, nil, nil, &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{}}, &test_utils.MockTssCommitmentsDb{}, 0, nil), 0, opts...)
	if res := ctx.TssCreateKey("main", "ecdsa", 0); !res.IsErr() {
		t.Fatalf("an unlisted contract's create_key passed in a dry-run context: %q", res.Unwrap())
	}
}

// The chain's contract call and the dry run must not set a version-gated option
// by hand again: that is how they drifted (KeyReshared 2026-10-13, try/catch and
// the key allowlist 2026-11-17). Both take them from
// stateEngine.ContractCallGates; the only option either may set itself is the
// pendulum applier, which is not a gate.
func TestContractContextBuildersTakeTheirGatesFromOneList(t *testing.T) {
	direct := regexp.MustCompile(`contract_execution_context\.With([A-Za-z0-9]+)\(`)
	for _, f := range []string{"schema.resolvers.go", "../../state-processing/transactions.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "ContractCallGates(") {
			t.Errorf("%s does not call ContractCallGates", f)
		}
		for _, m := range direct.FindAllStringSubmatch(string(src), -1) {
			if m[1] != "PendulumApplier" {
				t.Errorf("%s sets With%s itself: put it in stateEngine.ContractCallGatesFor so the chain and the dry run both get it", f, m[1])
			}
		}
	}
}
