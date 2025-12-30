package mapper

import (
	"context"
	"testing"
	"time"
)

func TestCallContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	callContract(
		ctx,
		"hive:milo-hpr",
		"vsc1BcS12fD42kKqL2SMLeBzaEKtd9QbBWC1dt",
		[]byte(`{"amount":8000,"recipient_vsc_address":"hive:vaultec"}`),
		"transfer",
	)
}
