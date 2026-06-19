package election_proposer

import (
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

// h6Witness builds a witness with a VALID consensus BLS key + PoP (so it always
// passes the consensus arm of the H-6 gate). withGatewayPoP toggles whether it
// also carries a valid gateway-key PoP; when false it announces a gateway key
// with NO PoP — exactly the case the gateway arm of the gate must exclude.
func h6Witness(t *testing.T, account string, seedByte byte, withGatewayPoP bool) witnesses.Witness {
	t.Helper()
	var seed [32]byte
	for i := range seed {
		seed[i] = seedByte
	}
	priv := dids.BlsPrivKey{}
	priv.Deserialize(&seed)
	pub, err := ethBls.SkToPk(&priv)
	if err != nil {
		t.Fatalf("SkToPk: %v", err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		t.Fatalf("NewBlsDID: %v", err)
	}
	consPoP, err := dids.GenerateBlsPoP(&priv, account)
	if err != nil {
		t.Fatalf("GenerateBlsPoP: %v", err)
	}

	kp := hivego.KeyPairFromBytes(seed[:])
	gwPoP := ""
	if withGatewayPoP {
		gwPoP, err = dids.GenerateGatewayKeyPoP(kp, account)
		if err != nil {
			t.Fatalf("GenerateGatewayKeyPoP: %v", err)
		}
	}
	return witnesses.Witness{
		Account: account,
		Enabled: true,
		// Announce consensus version 0.2.0 so the witness survives the election
		// version-floor filter when the gate is driven by a 0.2.0 prevVersion (the
		// floor and the H-6 gate now share that version input).
		ProtocolVersion: 2,
		DidKeys: []witnesses.PostingJsonKeys{
			{CryptoType: "bls", Type: "consensus", Key: string(did), PoP: consPoP},
		},
		GatewayKey:    *kp.GetPublicKeyString(),
		GatewayKeyPoP: gwPoP,
	}
}

// TestH6GatewayPoPGate proves the gateway arm of the H-6 strict-admission gate:
// with WitnessKeyStrictActive on, a witness whose gateway key carries a valid
// PoP is admitted, and an otherwise-identical witness whose gateway key has NO
// PoP is excluded from the committee.
func TestH6GatewayPoPGate(t *testing.T) {
	good := h6Witness(t, "alice", 0x11, true)   // valid gateway PoP → kept
	noPoP := h6Witness(t, "bob", 0x22, false)   // gateway key, no PoP → excluded

	ct := test_utils.NewContractTest()
	ep := New(
		nil,
		&test_utils.MockWitnessDb{},
		&test_utils.MockElectionDb{},
		nil,
		&test_utils.MockBalanceDb{},
		ct.DataLayer,
		nil,
		nil,
		systemconfig.MocknetConfig(),
		nil,
		nil,
	)

	// prevVersion = 0.2.0 drives WitnessKeyStrictActive ON (the gate keys on the
	// prior election's version, not a height). The test witnesses announce 0.2.0
	// so they survive the version-floor filter and reach the H-6 gate.
	_, data, err := ep.GenerateFullElection([]witnesses.Witness{good, noPoP}, 0, consensusversion.V0_2_0, 100)
	if err != nil {
		t.Fatalf("GenerateFullElection: %v", err)
	}
	var members []string
	for _, m := range data.Members {
		members = append(members, m.Account)
	}
	if len(members) != 1 || members[0] != "alice" {
		t.Fatalf("members = %v, want [alice] (bob excluded for missing gateway-key PoP)", members)
	}
}
