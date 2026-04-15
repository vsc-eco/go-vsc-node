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

// hiveCustomJsonMaxSize is the Hive custom_json operation byte limit (HF20).
// Payloads whose wrapped envelope exceeds this must be routed through the
// VSC L2 transaction pool (which caps at 16384 bytes via MAX_TX_SIZE).
const hiveCustomJsonMaxSize = 8192

// hiveEnvelopeThreshold is the envelope-size ceiling at which we switch to the
// L2 path. Set below hiveCustomJsonMaxSize to leave headroom for minor size
// variations between estimation and the actual broadcast.
const hiveEnvelopeThreshold = 7500

// errBotEthKeyMissing is returned when the bot is asked to submit an L2 tx but
// no signing key is configured.
var errBotEthKeyMissing = errors.New("L2 signing key not configured for this bot")

// estimateHiveEnvelopeSize approximates the on-wire size of the Hive
// custom_json operation that callContract would build for the given payload.
// Used only to decide whether the call needs the L2 fallback — not a
// security-critical bound.
func estimateHiveEnvelopeSize(action string, netId, hiveUsername, contractId string, payload json.RawMessage) int {
	// Wrapper is {"net_id":"...","caller":"hive:...","contract_id":"...",
	// "action":"...","payload":<inlined>,"rc_limit":10000,"intents":[]}.
	// The non-payload JSON keys + punctuation + fixed fields total ~120 bytes.
	const wrapperOverhead = 120
	return wrapperOverhead + len(netId) + len("hive:") + len(hiveUsername) +
		len(contractId) + len(action) + len(payload)
}

// callContractL2 submits a vsc.call contract invocation through the VSC L2
// transaction pool using the bot's did:pkh:eip155 identity. This path bypasses
// the Hive custom_json 8192-byte cap and supports payloads up to 16384 bytes.
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

	nonce, err := b.gql().FetchAccountNonce(ctx, did.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce: %w", err)
	}

	call := &transactionpool.VscContractCall{
		ContractId: b.BotConfig.ContractId(),
		Action:     action,
		Payload:    string(contractInput),
		RcLimit:    10000,
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
		RcLimit: 10000,
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(b.botEthKey),
		Did:      did,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
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
