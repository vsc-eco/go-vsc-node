package mapper

import (
	"context"
	"encoding/json"
)

func (b *Bot) callContract(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	return b.callContractL2(ctx, contractInput, action)
}
