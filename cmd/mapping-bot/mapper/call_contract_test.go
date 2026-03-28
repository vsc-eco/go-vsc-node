package mapper

import (
	"context"
	"testing"
	"time"
)

func TestCallContract_Integration(t *testing.T) {
	t.Skip("integration test: requires live Hive node")
	bot := newIntegrationBot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bot.callContract(
		ctx,
		[]byte(`{"amount":8000,"recipient_vsc_address":"hive:vaultec"}`),
		"transfer",
	)
}
