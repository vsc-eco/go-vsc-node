package election_proposer

import (
	"testing"

	"vsc-node/lib/dids"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

// v020Sconf wraps a base SystemConfig and pins Version0_2_0Height, which all the
// v0.2.0-keyed resolvers (including WitnessKeyStrictActive) delegate to. height 0
// disables the v0.2.0 batch — e.g. the H-6 strict-key gate — regardless of the
// base network's value; a positive height activates it for blocks >= it.
type v020Sconf struct {
	systemconfig.SystemConfig
	height uint64
}

func (s v020Sconf) ConsensusParams() params.ConsensusParams {
	cp := s.SystemConfig.ConsensusParams() // returned by value
	cp.Version0_2_0Height = s.height
	return cp
}

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
		v020Sconf{SystemConfig: systemconfig.MocknetConfig(), height: 1}, // H-6 gate ON
		nil,
		nil,
	)

	_, data, err := ep.GenerateFullElection([]witnesses.Witness{good, noPoP}, 0, consensusversion.Version{}, 100)
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
