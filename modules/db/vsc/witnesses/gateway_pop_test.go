package witnesses

import (
	"testing"
	"vsc-node/lib/dids"

	"github.com/vsc-eco/hivego"
)

// TestWitnessVerifyGatewayKeyPoP exercises the record-level method the H-6
// election gate calls to admit/exclude a witness on its gateway-key PoP.
func TestWitnessVerifyGatewayKeyPoP(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = 0x33
	}
	kp := hivego.KeyPairFromBytes(seed)
	key := *kp.GetPublicKeyString()
	pop, err := dids.GenerateGatewayKeyPoP(kp, "alice")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	cases := []struct {
		name string
		w    Witness
		ok   bool
	}{
		{"valid", Witness{Account: "alice", GatewayKey: key, GatewayKeyPoP: pop}, true},
		{"missing pop (legacy announce)", Witness{Account: "alice", GatewayKey: key}, false},
		{"missing key", Witness{Account: "alice", GatewayKeyPoP: pop}, false},
		{"pop bound to other account", Witness{Account: "bob", GatewayKey: key, GatewayKeyPoP: pop}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.VerifyGatewayKeyPoP()
			if tc.ok && err != nil {
				t.Fatalf("expected pass, got: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected failure, got pass")
			}
		})
	}
}
