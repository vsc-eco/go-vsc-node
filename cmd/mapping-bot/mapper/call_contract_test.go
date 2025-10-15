package mapper

import "testing"

func TestCallContract(t *testing.T) {
	callContract(
		"hive:milo-hpr",
		"vsc1BcS12fD42kKqL2SMLeBzaEKtd9QbBWC1dt",
		[]byte(`{"amount":8000,"recipient_vsc_address":"hive:vaultec"}`),
		"transfer",
	)
}
