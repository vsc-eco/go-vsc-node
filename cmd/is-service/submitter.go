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
// running L2 — production wires this to a vsc client posting to the
// dash-mapping-contract.
//
// SubmitMapInstantSend executes the L2 contract call. Returns the L2
// txID so the orchestrator can persist it on the Session and surface
// it via /status (audit finding `submitter-l2-txid-discarded-no-observability`).
//
// payload is the wire format the dash-mapping-contract's mapInstantSendV2
// entrypoint expects — MUST match the contract's
// MapInstantSendV2ParamsFull tinyjson schema (envelope: {body, agg}; all
// nested keys snake_case).
type Submitter interface {
	SubmitMapInstantSend(ctx context.Context, payload MapInstantSendPayload) (l2TxID string, err error)
}

// MapInstantSendBody mirrors the contract's MapInstantSendV2Params (tinyjson
// type in dash-mapping-contract/contract/mapping/forwarder_integration.go).
// Field names + JSON tags MUST stay byte-for-byte aligned. Drift here is
// the audit's `payload-schema-mismatch-is-vs-contract` finding.
type MapInstantSendBody struct {
	RawTxHex     string                       `json:"raw_tx_hex"`
	Instruction  string                       `json:"instruction"`
	Epoch        uint64                       `json:"epoch"`
	Attestations []MapInstantSendAttestation  `json:"attestations"`
	ChainId      string                       `json:"chain_id"`
}

// MapInstantSendAttestation mirrors the contract's ValidatorAttestation.
// PubkeyHex is the 48-byte BLS pubkey hex; the contract aggregates these
// via crypto.bls_verify_aggregate's host fn and also confirms each DID is
// in the registered validator set at the request's epoch.
type MapInstantSendAttestation struct {
	ValidatorDID string `json:"validator_did"`
	PubkeyHex    string `json:"pubkey_hex"`
	BlsSigHex    string `json:"sig_hex"`
}

// MapInstantSendAgg mirrors the contract's AggregatedSig — the OFF-CHAIN
// aggregate of every attestation's BlsSigHex via bls.Aggregate.
type MapInstantSendAgg struct {
	AggSigHex string `json:"agg_sig_hex"`
}

// MapInstantSendPayload mirrors the contract's MapInstantSendV2ParamsFull
// — the {body, agg} envelope.
type MapInstantSendPayload struct {
	Body MapInstantSendBody `json:"body"`
	Agg  MapInstantSendAgg  `json:"agg"`
}

// Marshal returns the JSON wire bytes the contract will receive as the
// L2 action payload. Used by L2 submitters + test fixtures.
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

func (SubmitterLogOnly) SubmitMapInstantSend(ctx context.Context, p MapInstantSendPayload) (string, error) {
	raw, err := p.Marshal()
	if err != nil {
		return "", err
	}
	slog.Info("mapInstantSendV2 submission (log-only mode)",
		"epoch", p.Body.Epoch,
		"attestations", len(p.Body.Attestations),
		"chainId", p.Body.ChainId,
		"payloadHex", hex.EncodeToString(raw),
	)
	return "log-only:no-l2-tx", nil
}
