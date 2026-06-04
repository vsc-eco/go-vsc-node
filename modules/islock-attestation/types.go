// Package islock_attestation implements the lazy-attestation protocol that
// lets Magi validators collectively notarise a Dash InstantSend lock for the
// dash-mapping-contract's fast-path (`mapInstantSend`).
//
// Design context: docs/dash-is-login/0-scoping-spike-decision.md
// Spec ref:       magi/testnet/docs/superpowers/specs/2026-05-14-dash-instantsend-login-design.md §5.6
//
// The protocol is request-driven (not push):
//
//  1. The IS Service sees a Dash IS-lock for a deposit address one of
//     its sessions is waiting on (via its dashd-RPC watcher).
//  2. IS Service broadcasts an IsLockAttestationRequest over a Magi p2p
//     gossip topic.
//  3. Each Magi validator's DashdPoller has also seen the same tx
//     (validators poll their own dashd via getrawmempool every 2s and
//     admit txids that report instantlock=true). The validator checks
//     its bounded in-memory cache and, on a hit, signs an
//     IsLockAttestationResponse with its consensus BLS key + domain
//     prefix. Validators that never saw the lock ignore the request
//     silently.
//  4. IS Service collects N-of-M responses and submits one L2 tx
//     (mapInstantSend) carrying the bundle.
//  5. The dash-mapping-contract verifies the BLS aggregate against the
//     active validator set at the request's epoch.
//
// Wire types live in this file; memory store + signing in sibling
// files; p2p wiring + validator-side observation in dashd_poller.go
// (Audit R15-CONS-06: the original "ZMQ" wording predated the
// production-shape RPC poller in commit 4a8a4fd8).
package islock_attestation

// IsLockAttestationRequest is a broadcast from the IS Service (or any
// relayer) asking validators to attest that they saw a specific Dash
// InstantSend lock and that it matches the encoded deposit address.
//
// The validator does NOT trust any field in this request blindly. It
// independently:
//   - Verifies it has seen this txid via its own dashd RPC poller
//     (modules/islock-attestation/dashd_poller.go).
//   - Confirms instantlock=true on the tx (defense-in-depth; the
//     poller already filters on this before populating IsLockMemory).
//   - Confirms the destination address in rawTxHex matches the address
//     derived from (primaryPubkey, backupPubkey, instruction).
//
// Then signs the canonical message and replies.
type IsLockAttestationRequest struct {
	// TxId is the Dash txid being attested. Hex-encoded.
	TxId string `json:"txid"`
	// RawTxHashHex is hex(sha256d(rawTxBytes)). Validators verify this
	// matches what they observed via their own dashd RPC poller before
	// signing.
	RawTxHashHex string `json:"rawTxHash"`
	// InstructionHashHex is hex(sha256(instruction_bytes)). Binds the
	// attestation to the specific deposit instruction the user paid for.
	// Without this, a malicious requester could collect attestations for
	// a tx and reuse them with a different instruction.
	InstructionHashHex string `json:"instructionHash"`
	// Epoch identifies the active Magi validator-set epoch the attester
	// must sign against. The contract verifies signatures against the
	// at-epoch validator set (with a small grace window for rotations).
	Epoch uint64 `json:"epoch"`
	// ChainId is the Magi network identifier ("vsc-mainnet", "vsc-testnet")
	// — included in the signed message to prevent cross-network replay.
	ChainId string `json:"chainId"`
}

// IsLockAttestationResponse is a validator's signed attestation that it
// observed the requested IS-lock and that the proposed instruction binding
// is consistent with what it saw.
type IsLockAttestationResponse struct {
	// TxId echoed back so the requester can correlate response → request.
	TxId string `json:"txid"`
	// ValidatorDID identifies the signing validator. For Magi this is
	// "did:key:..." (BlsDID). The contract looks up the corresponding
	// pubkey from the active validator set at the request's epoch and
	// confirms it matches PubkeyHex.
	ValidatorDID string `json:"validatorDid"`
	// PubkeyHex is the validator's 48-byte BLS pubkey (96 hex chars).
	// Included in the response so the IS-service-side aggregator
	// doesn't need a separate validator-set query — the contract is the
	// authority and rejects responses whose PubkeyHex doesn't match the
	// registered pubkey for the claimed ValidatorDID.
	PubkeyHex string `json:"pubkey"`
	// Epoch the validator signed at. MUST equal the request's Epoch
	// (validators reject requests for epochs they're not active in).
	Epoch uint64 `json:"epoch"`
	// BlsSigHex is hex(96 bytes) — the BLS12-381 signature over the
	// canonical message (CanonicalSigningMessage). Verifiable against
	// the validator's BlsDID-derived pubkey.
	BlsSigHex string `json:"sig"`
}

// MaxIsLockMemoryEntries is the upper bound on a validator's in-memory
// cache of seen IS-locks. Each entry is small (~1 KB tx + metadata) so
// 100K is ~100 MB — generous for any production rate. Eviction is FIFO
// by observedAt timestamp (NOT LRU) so attackers can't extend lifetime
// of attacker-controlled entries via repeated attestation requests.
const MaxIsLockMemoryEntries = 100_000

// IsLockMemoryTTL is how long an observed IS-lock stays in a validator's
// memory before it's evicted. Long enough that legitimate fast-path
// flows always succeed (typical session window is <60s), short enough
// that memory doesn't bloat under sustained load.
const IsLockMemoryTTLSeconds = 600 // 10 minutes
