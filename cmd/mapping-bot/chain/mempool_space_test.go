package chain

import "testing"

func TestIsAlreadyInChain(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "utxo set message (Core 25+)",
			body: `API returned status 400: sendrawtransaction RPC error: {"code":-27,"message":"Transaction outputs already in utxo set"}`,
			want: true,
		},
		{
			name: "block chain message (pre-25)",
			body: `sendrawtransaction RPC error: {"code":-27,"message":"Transaction already in block chain"}`,
			want: true,
		},
		{
			name: "code -27 only, unfamiliar message",
			body: `RPC error: {"code":-27,"message":"some future wording"}`,
			want: true,
		},
		{
			name: "missing inputs is not already-in-chain",
			body: `sendrawtransaction RPC error: {"code":-25,"message":"bad-txns-inputs-missingorspent"}`,
			want: false,
		},
		{
			name: "generic failure",
			body: `API returned status 500: internal error`,
			want: false,
		},
		{
			name: "empty",
			body: "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAlreadyInChain(tc.body); got != tc.want {
				t.Fatalf("isAlreadyInChain(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
