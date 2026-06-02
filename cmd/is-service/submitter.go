package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	islock "vsc-node/modules/islock-attestation"
)

// Submitter sends the assembled mapInstantSendV2 L2 transaction to the
// Magi network. Abstracted so the IS service can be tested without a
// running L2 — production wires this to a hive_io client posting to the
// dash-mapping-contract.
//
// SubmitMapInstantSend executes the L2 contract call and blocks until
// the result is observable (transaction landed in a block). For v1 the
// spec doesn't require landing-block confirmation; the caller still
// transitions to ON_CHAIN once submission returns nil error.
//
// payload is the wire format the dash-mapping-contract's mapInstantSendV2
// entrypoint expects (see modules/islock-attestation + dash-mapping-contract
// MapInstantSendV2Params).
type Submitter interface {
	SubmitMapInstantSend(ctx context.Context, payload MapInstantSendPayload) error
}

// MapInstantSendPayload is the IS-service-side view of the contract call.
// The contract decodes this from JSON into MapInstantSendV2ParamsFull.
type MapInstantSendPayload struct {
	TxId               string                              `json:"txid"`
	RawTxHex           string                              `json:"rawTxHex"`
	InstructionRaw     string                              `json:"instruction"`
	InstructionHashHex string                              `json:"instructionHash"`
	Epoch              uint64                              `json:"epoch"`
	ChainId            string                              `json:"chainId"`
	Attestations       []islock.IsLockAttestationResponse  `json:"attestations"`
}

// Marshal returns the JSON wire bytes the contract will receive as
// `args` (base64-encoded by the L2 client). Convenience for tests +
// production submitters.
func (p MapInstantSendPayload) Marshal() ([]byte, error) {
	return json.Marshal(p)
}

// noopBroadcaster is the default no-op p2p Broadcaster: logs the
// would-be broadcast and returns nil. Used when the IS service is run
// without a wired libp2p host — the production main passes a real
// islock_attestation.Service whose .Start has bound a PubSubService.
type noopBroadcaster struct{}

func (noopBroadcaster) BroadcastRequest(ctx context.Context, req islock.IsLockAttestationRequest) error {
	slog.Info("attestation broadcast (no-op mode)",
		"txid", req.TxId,
		"epoch", req.Epoch,
		"chainId", req.ChainId,
	)
	return nil
}

// SubmitterLogOnly is the default no-op Submitter: logs the would-be
// submission and returns success. Used when the IS service is run
// without a wired L2 client (e.g. for staging environments where the
// L2 tx is posted by a separate tool, or in dry-run debugging).
//
// Logs the payload's txid + a hex of its hash so operators can trace
// what would have been submitted.
type SubmitterLogOnly struct{}

func (SubmitterLogOnly) SubmitMapInstantSend(ctx context.Context, p MapInstantSendPayload) error {
	raw, err := p.Marshal()
	if err != nil {
		return err
	}
	slog.Info("mapInstantSendV2 submission (log-only mode)",
		"txid", p.TxId,
		"epoch", p.Epoch,
		"attestations", len(p.Attestations),
		"payloadHex", hex.EncodeToString(raw),
	)
	return nil
}
