package mapper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"
)

// errBotEthKeyMissing is returned when the bot is asked to submit an L2 tx but
// no signing key is configured.
var errBotEthKeyMissing = errors.New("L2 signing key not configured for this bot")

// callContractL2 submits a vsc.call contract invocation through the VSC L2
// transaction pool using the bot's did:pkh:eip155 identity.
//
// The bot's DID must have HBD balance to cover the RC cost — it has no free
// allotment (unlike hive: accounts). A funding error surfaces as
// "not enough RCS available" from the node.
func (b *Bot) callContractL2(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	if b.botEthKey == nil {
		return "", errBotEthKeyMissing
	}

	did := b.botEthDID

	// Serialize concurrent L2 submissions — see Bot.l2SubmitMu.
	b.l2SubmitMu.Lock()
	defer b.l2SubmitMu.Unlock()

	nonce, err := b.gql().FetchAccountNonce(ctx, did.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce: %w", err)
	}

	rcLimit := b.BotConfig.RcLimit()
	call := &transactionpool.VscContractCall{
		ContractId: b.BotConfig.ContractId(),
		Action:     action,
		Payload:    string(contractInput),
		RcLimit:    rcLimit,
		Intents:    []contracts.Intent{},
		Caller:     did.String(),
		NetId:      b.SystemConfig.NetId(),
	}
	op, err := call.SerializeVSC()
	if err != nil {
		return "", fmt.Errorf("serialize L2 op: %w", err)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   b.SystemConfig.NetId(),
		RcLimit: uint64(rcLimit),
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(b.botEthKey),
		Did:      did,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
	}

	if len(sTx.Tx) > transactionpool.MAX_TX_SIZE {
		b.L.Error("L2 transaction exceeds maximum size — cannot submit",
			"action", action,
			"cbor_size", len(sTx.Tx),
			"limit", transactionpool.MAX_TX_SIZE,
		)
		return "", fmt.Errorf("L2 tx too large: %d bytes (limit %d)", len(sTx.Tx), transactionpool.MAX_TX_SIZE)
	}

	txID, err := b.gql().SubmitTransactionV1(
		ctx,
		base64.URLEncoding.EncodeToString(sTx.Tx),
		base64.URLEncoding.EncodeToString(sTx.Sig),
	)
	if err != nil {
		return "", fmt.Errorf("broadcast L2 tx: %w", err)
	}

	b.L.Info("L2 tx broadcast",
		"id", txID,
		"action", action,
		"nonce", nonce,
		"cbor_size", len(sTx.Tx),
		"did", did.String(),
	)
	return txID, nil
}
