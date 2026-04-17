package mapper

// Mapping-bot pagination for oversized map / confirmSpend submissions.
//
// The VSC L2 transaction pool caps a single tx at `MAX_TX_SIZE = 16384` bytes
// (see `modules/transaction-pool/transaction-pool.go`). Some BTC proofs (large
// `raw_tx_hex` + Merkle proofs) serialize to a JSON payload beyond this cap.
//
// This file implements the deterministic chunk-commit protocol that is the
// counterpart to the contract-side `SubmitPage` in `utxo-mapping/btc-mapping-
// contract/contract/mapping/pagination.go`. The design is backed by the Lean
// proofs in `magi-lean/MagiLean/Security/MappingBot.lean`:
//
//   - `beyond_l2_requires_pagination`        → `mustPaginate` decision
//   - `PagePlanFitsL2` / `pagination_plan_each_page_fits_l2` → per-page size bound
//   - `pagination_reconstructs_original`     → content-hash parentId binding
//   - `contractSubmit_idempotent`             → duplicate page resubmit is safe
//
// Payload encoding is the UTF-8 JSON bytes of the full MapParams /
// ConfirmSpendParams object; splitting happens at byte boundaries (JSON is
// not required to be well-formed per page — reassembly is byte-exact).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	transactionpool "vsc-node/modules/transaction-pool"
)

// maxPagePayloadBytes is the conservative per-page payload byte cap, in
// BYTES OF THE BASE64 WIRE FORM.
//
// The L2 MAX_TX_SIZE is 16384 bytes, but the signed tx envelope wraps the
// payload with `{parent_id, page_idx, total_pages, payload}` JSON (~120 B
// overhead), a `VscContractCall` op header (~300 B with action + contract +
// caller + nonce + rc_limit), plus a CBOR outer wrapper and signature. We
// budget 2 KB for envelope + signature headroom, leaving 14 000 bytes for
// the page `payload` string.
//
// Because the wire form is base64 (URL alphabet, no padding) of the raw JSON
// payload, every byte in the page string is plain ASCII and maps 1:1 into
// the JSON string literal — no `\uXXXX` escape blowup can push a page over
// the budget. The effective raw-payload capacity per page is therefore
// roughly `maxPagePayloadBytes * 3 / 4` ≈ 10 500 bytes.
const maxPagePayloadBytes = 14_000

// paginationActionSuffix tags the paginated variants of a base action.
// The contract-side WASM export names are `<action>Page`; e.g. `map` ->
// `mapPage`, `confirmSpend` -> `confirmSpendPage`.
const paginationActionSuffix = "Page"

// pageJob captures a single chunk of a paginated payload to be submitted as
// its own L2 transaction.
type pageJob struct {
	ParentID   string          `json:"parent_id"`
	PageIdx    uint32          `json:"page_idx"`
	TotalPages uint32          `json:"total_pages"`
	Payload    json.RawMessage `json:"payload"`
}

// pagePayload is the on-wire shape of a single page body. It matches
// `mapping.MapPageParams` / `mapping.ConfirmSpendPageParams` exactly on the
// contract side. Kept as a local type so the mapping-bot does not depend on
// the WASM contract Go package.
type pagePayload struct {
	ParentID   string `json:"parent_id"`
	PageIdx    uint32 `json:"page_idx"`
	TotalPages uint32 `json:"total_pages"`
	Payload    string `json:"payload"`
}

// computeParentID is the client-side counterpart to
// `mapping.ComputeParentId`. It MUST produce the same 64-char lower-hex
// sha256 digest the contract computes on reassembly, otherwise the contract
// rejects the plan as tampered.
func computeParentID(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// mustPaginate reports whether a payload is too large for a single L2 tx
// and therefore requires the page-commit protocol. This is the executable
// counterpart of Lean's `beyond_l2_requires_pagination`.
//
// The decision is made against the raw payload length (what a direct L2
// submission would put in `VscContractCall.Payload`). Below this threshold
// the non-paginated path is safe; above it, the bot switches to the
// base64-chunk protocol even though the base64 wire form would itself be a
// bit larger — avoiding a no-op pagination attempt on payloads that fit raw.
func mustPaginate(payloadLen int) bool {
	return payloadLen > maxPagePayloadBytes
}

// encodeForWire returns the base64 (URL alphabet, no padding) wire form of
// a raw payload. Pages always carry substrings of this wire form, and the
// contract's `SubmitPage` decodes the reassembled concatenation back to the
// original bytes before hashing.
func encodeForWire(payload []byte) []byte {
	wire := make([]byte, base64.RawURLEncoding.EncodedLen(len(payload)))
	base64.RawURLEncoding.Encode(wire, payload)
	return wire
}

// splitIntoPages partitions the BASE64 wire form of `payload` into a
// deterministic list of page chunks, each sized ≤ maxPagePayloadBytes. The
// resulting slice, when concatenated in page-index order, reconstructs the
// base64 wire form exactly; base64-decoding that concatenation reproduces
// the original input.
//
// Empty input returns a single empty page (rather than zero pages) so that
// total_pages is always ≥ 1 and the contract still receives a final-page
// signal. An oversized input whose total pages would exceed 127 is rejected
// because the contract caps `MaxPagesPerParent` at 128.
func splitIntoPages(payload []byte) ([][]byte, error) {
	wire := encodeForWire(payload)
	if len(wire) == 0 {
		return [][]byte{{}}, nil
	}
	total := (len(wire) + maxPagePayloadBytes - 1) / maxPagePayloadBytes
	if total > 127 {
		return nil, fmt.Errorf("payload requires %d pages which exceeds contract MaxPagesPerParent (128)", total)
	}
	pages := make([][]byte, 0, total)
	for i := 0; i < len(wire); i += maxPagePayloadBytes {
		end := i + maxPagePayloadBytes
		if end > len(wire) {
			end = len(wire)
		}
		chunk := make([]byte, end-i)
		copy(chunk, wire[i:end])
		pages = append(pages, chunk)
	}
	return pages, nil
}

// buildPagePlan produces the full list of `pageJob` descriptors for a
// payload, bound to the content-addressed parent id. The plan is order-
// preserving: pages[i].PageIdx == i for all i.
func buildPagePlan(payload []byte) ([]pageJob, string, error) {
	chunks, err := splitIntoPages(payload)
	if err != nil {
		return nil, "", err
	}
	parentID := computeParentID(payload)
	total := uint32(len(chunks))
	plan := make([]pageJob, 0, total)
	for i, c := range chunks {
		raw, err := json.Marshal(string(c))
		if err != nil {
			return nil, "", fmt.Errorf("marshal page %d payload: %w", i, err)
		}
		plan = append(plan, pageJob{
			ParentID:   parentID,
			PageIdx:    uint32(i),
			TotalPages: total,
			Payload:    raw,
		})
	}
	return plan, parentID, nil
}

// pageActionFor returns the paginated WASM-export action name for a base
// action, e.g. `map` -> `mapPage`, `confirmSpend` -> `confirmSpendPage`.
func pageActionFor(base string) string {
	if base == "" {
		return ""
	}
	return base + paginationActionSuffix
}

// encodePageSubmission serializes a pageJob into the JSON body expected by
// the contract's page-handling exports.
func encodePageSubmission(job pageJob) (json.RawMessage, error) {
	// Payload is a JSON-encoded string (the raw bytes of this page as a JSON
	// string literal). We unmarshal -> re-marshal through pagePayload to keep
	// field ordering and type stable.
	var payloadStr string
	if err := json.Unmarshal(job.Payload, &payloadStr); err != nil {
		return nil, fmt.Errorf("re-decode page payload: %w", err)
	}
	body, err := json.Marshal(pagePayload{
		ParentID:   job.ParentID,
		PageIdx:    job.PageIdx,
		TotalPages: job.TotalPages,
		Payload:    payloadStr,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal page submission: %w", err)
	}
	return body, nil
}

// assertPagePlanFitsL2 is a defensive sanity check: each page's marshalled
// submission body must fit the L2 tx cap with reasonable envelope headroom.
// This is a client-side witness for the Lean `PagePlanFitsL2` predicate.
func assertPagePlanFitsL2(plan []pageJob) error {
	for _, job := range plan {
		body, err := encodePageSubmission(job)
		if err != nil {
			return err
		}
		if len(body) > transactionpool.MAX_TX_SIZE {
			return fmt.Errorf(
				"paginated page %d serialized to %d bytes, exceeds L2 cap %d",
				job.PageIdx, len(body), transactionpool.MAX_TX_SIZE,
			)
		}
	}
	return nil
}

// errPageActionUnsupported is returned when a caller tries to paginate an
// action for which the contract does not expose a `Page` variant.
var errPageActionUnsupported = errors.New("action has no page-commit variant")

// supportedPaginatedActions enumerates the base actions that have
// `{action}Page` counterparts on the WASM contract side.
var supportedPaginatedActions = map[string]bool{
	"map":          true,
	"confirmSpend": true,
}

// requirePaginatedAction verifies the contract exposes a page variant.
func requirePaginatedAction(base string) error {
	if !supportedPaginatedActions[base] {
		return fmt.Errorf("%w: %s", errPageActionUnsupported, base)
	}
	return nil
}
