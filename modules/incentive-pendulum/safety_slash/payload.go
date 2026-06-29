package safetyslash

import "strings"

// SafetySlashReverseAction tags the reversal kind; only the listed values
// are accepted by the apply path.
type SafetySlashReverseAction string

const (
	// ReverseActionCancel undoes a not-yet-finalized burn slice. Calls
	// LedgerSystem.CancelPendingSafetySlashBurn. No HIVE_CONSENSUS credit
	// is written by this action alone — pair with reverse_or_both to
	// re-credit the validator's bond.
	ReverseActionCancel SafetySlashReverseAction = "cancel"
	// ReverseActionReverse re-credits HIVE_CONSENSUS bond up to the portion
	// of the slash NOT yet committed to the reserve. Calls
	// LedgerSystem.ReverseSafetySlashConsensusDebit. The apply path caps the
	// amount at slashAmount - committedToReserve - alreadyReversed so
	// governance cannot mint past what is still reversible.
	// NOTE: reverse-only does NOT cancel an in-flight pending residual, so
	// during the challenge window it yields ~0 headroom (the pending residual
	// counts as committed); use ReverseActionBoth to undo a wrongful slash.
	ReverseActionReverse SafetySlashReverseAction = "reverse"
	// ReverseActionBoth performs cancel + reverse atomically (with
	// both ledger writes happening in the same StateEngine apply). Most
	// reversals will use this since the typical case is "the slash was
	// wrong, undo everything".
	ReverseActionBoth SafetySlashReverseAction = "both"
)

// SafetySlashReverseRecord is the L2 op payload carried as a
// BlockTypeSafetySlashReverse entry inside a VSC block. The 2/3 BLS
// aggregate over the carrying block is the supermajority auth gate
// (witnesses vote for inclusion); the state engine validates ledger
// consistency before applying.
type SafetySlashReverseRecord struct {
	// SlashTxID is the original Hive custom_json tx id that produced
	// the safety_slash_consensus row being reversed. Required.
	SlashTxID string `json:"slash_tx_id" refmt:"slash_tx_id"`

	// EvidenceKind of the original slash. Required.
	EvidenceKind string `json:"evidence_kind" refmt:"evidence_kind"`

	// SlashedAccount is the validator whose bond was originally debited.
	// Stored without "hive:" prefix; the apply path normalises.
	SlashedAccount string `json:"slashed_account" refmt:"slashed_account"`

	// Action selects cancel-only / reverse-only / both. See the
	// constants above.
	Action SafetySlashReverseAction `json:"action" refmt:"action"`

	// Amount is the requested HIVE_CONSENSUS re-credit in base units. Used
	// only by reverse / both. The apply path caps it at the original
	// slash amount minus any already-finalized burn so the resulting
	// ledger state never re-credits more than was debited.
	Amount int64 `json:"amount,omitempty" refmt:"amount,omitempty"`

	// Reason is free-form text recorded in the cancellation/credit row
	// for explorer attribution. Trimmed at apply time.
	Reason string `json:"reason,omitempty" refmt:"reason,omitempty"`
}

// Normalize trims string fields; safe before composing or after decoding.
func (r SafetySlashReverseRecord) Normalize() SafetySlashReverseRecord {
	out := r
	out.SlashTxID = strings.TrimSpace(out.SlashTxID)
	out.EvidenceKind = strings.TrimSpace(out.EvidenceKind)
	out.Reason = strings.TrimSpace(out.Reason)
	a := strings.TrimSpace(out.SlashedAccount)
	if a != "" && !strings.HasPrefix(a, "hive:") {
		a = "hive:" + a
	}
	out.SlashedAccount = a
	out.Action = SafetySlashReverseAction(strings.TrimSpace(string(out.Action)))
	return out
}
