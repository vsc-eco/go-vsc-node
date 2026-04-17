package mapper

import (
	"context"
	"encoding/json"
	"fmt"
)

// callContract is the single entry point for the bot to invoke a contract
// action. For payloads that fit a single L2 transaction it defers directly
// to `callContractL2`. For oversized payloads it falls back to the
// deterministic chunk-commit pagination protocol (one L2 tx per page, each
// targeting `<action>Page`).
//
// The returned string is the tx id of the *final* submission that actually
// triggers contract dispatch (the last page in a paginated run, or the
// single L2 tx in the non-paginated path).
func (b *Bot) callContract(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	if !mustPaginate(len(contractInput)) {
		return b.callContractL2(ctx, contractInput, action)
	}
	return b.callContractPaginated(ctx, contractInput, action)
}

// callContractPaginated implements the client-side page-commit protocol:
// splits the input into ≤ `maxPagePayloadBytes` chunks, submits each as its
// own L2 tx targeting the contract's paginated variant of `action`, and
// returns the tx id of the final-page submission.
//
// Safety properties (mirror the Lean-proven guarantees in
// `MagiLean.Security.MappingBot`):
//   - Each page is bounded to fit MAX_TX_SIZE (assertPagePlanFitsL2).
//   - The content-addressed parent id binds every page to the same payload;
//     tampering with any page is rejected at assembly time by the contract.
//   - Re-submitting any previously-accepted page is a no-op at the contract
//     layer (`SubmitPage` bitmap dedupe), so duplicate-page retries by the
//     bot are safe.
func (b *Bot) callContractPaginated(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	if err := requirePaginatedAction(action); err != nil {
		return "", err
	}

	plan, parentID, err := buildPagePlan([]byte(contractInput))
	if err != nil {
		return "", fmt.Errorf("pagination plan: %w", err)
	}
	if err := assertPagePlanFitsL2(plan); err != nil {
		return "", fmt.Errorf("pagination plan invariant: %w", err)
	}

	pageAction := pageActionFor(action)
	b.L.Info("paginating oversized contract call",
		"base_action", action,
		"page_action", pageAction,
		"parent_id", parentID,
		"total_pages", len(plan),
		"payload_bytes", len(contractInput),
	)

	var lastTxID string
	for _, job := range plan {
		body, err := encodePageSubmission(job)
		if err != nil {
			return "", fmt.Errorf("encode page %d: %w", job.PageIdx, err)
		}

		txID, err := b.callContractL2(ctx, body, pageAction)
		if err != nil {
			return "", fmt.Errorf("submit page %d/%d (parent=%s): %w",
				job.PageIdx+1, job.TotalPages, parentID, err)
		}
		b.L.Info("page submitted",
			"parent_id", parentID,
			"page_idx", job.PageIdx,
			"total_pages", job.TotalPages,
			"tx_id", txID,
		)
		lastTxID = txID
	}
	return lastTxID, nil
}
