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

	// Per-action: the vault-rotation ops need a far higher ceiling than the bot's
	// configured default, which every other action keeps unchanged (see rcLimitFor).
	rcLimit := b.rcLimitFor(action)

	// VR2-04: refuse before submitting rather than stalling mid-cycle.
	//
	// A rotation costs roughly 28,000 RC across map, topUpFeeReserve, migrateVault
	// and confirmSpend, and there was no pre-flight at all. Running out partway
	// leaves a generation half-swept with an in-flight spend that the same account
	// can no longer finish -- a testnet confirmSpend aborted at RC 83 exactly this
	// way. Failing before the first op is recoverable; failing between them is the
	// state that needs manual recovery.
	//
	// Fails OPEN on an unreadable RC balance: a monitoring outage must not become a
	// rotation outage, and the node will still reject the op itself if the credits
	// really are missing.
	if available, known, rcErr := b.gql().FetchAccountRC(ctx, did.String()); rcErr != nil {
		b.L.Warn("RC pre-flight skipped: could not read available credits",
			"action", action, "err", rcErr)
	} else if known && available < int64(rcLimit) {
		return "", fmt.Errorf(
			"insufficient RC for %s: %d available, %d needed for this op alone; "+
				"fund the bot account before starting a rotation (a mid-cycle stall "+
				"leaves an in-flight spend this account cannot finish)",
			action, available, rcLimit)
	}
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
