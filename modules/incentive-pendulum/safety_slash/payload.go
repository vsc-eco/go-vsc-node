package safetyslash

import "strings"

// RestitutionClaimRecord is the L2 op payload carried as a
// BlockTypeRestitutionClaim entry inside a VSC block. The witness
// committee's 2/3 BLS aggregate over the carrying block is the auth
// gate; the state engine independently re-validates the harm proof
// before enqueueing.
//
// All fields are deterministically encoded (sorted-where-applicable,
// trimmed strings) so a record composed by two honest nodes byte-equals.
type RestitutionClaimRecord struct {
	// ClaimID is the deterministic identifier; replays upsert by this ID.
	// Composers should derive it from (carrying block id, tx index, victim
	// account) or another stable tuple to avoid collisions across blocks.
	ClaimID string `json:"claim_id" refmt:"claim_id"`

	// VictimAccount is the account credited when the queue allocates.
	// Stored without the "hive:" prefix; the state engine normalises on
	// apply.
	VictimAccount string `json:"victim_account" refmt:"victim_account"`

	// SlashTxID is the original on-chain Hive tx id (custom_json) that
	// produced the safety_slash_consensus row this claim is scoped to.
	// Required; the apply path verifies the row exists.
	SlashTxID string `json:"slash_tx_id" refmt:"slash_tx_id"`

	// SlashedAccount is the validator whose bond was originally debited.
	// Required so the apply path can scope its slash-row lookup. Stored
	// without "hive:" prefix; Normalize() reattaches it.
	SlashedAccount string `json:"slashed_account" refmt:"slashed_account"`

	// EvidenceKind matches the slash row's kind. Required so different
	// kinds for the same SlashTxID can each have their own claims if
	// needed (today every slash is per-kind, so this is mainly explanatory).
	EvidenceKind string `json:"evidence_kind" refmt:"evidence_kind"`

	// LossHive is the amount of liquid HIVE the claim asks to be made
	// whole for, in satoshi-HIVE. Must be > 0.
	LossHive int64 `json:"loss_hive" refmt:"loss_hive"`

	// VictimTxID is OPTIONAL: when non-empty, it points at a Hive
	// custom_json or VSC tx whose AnchoredId equals SlashTxID — i.e. the
	// victim's transaction was carried by the same bad block that
	// triggered the slash. The state engine performs this check when the
	// field is supplied. When blank, the witnesses are explicitly
	// vouching for the claim with their BLS signature alone (the trust
	// gate is the 2/3 supermajority that signed the carrying block).
	VictimTxID string `json:"victim_tx_id,omitempty" refmt:"victim_tx_id,omitempty"`

	// Memo is optional free-form text recorded on the queue row for
	// explorers. Trimmed at apply time; not consensus-relevant.
	Memo string `json:"memo,omitempty" refmt:"memo,omitempty"`
}

// Normalize trims whitespace on string fields and prepends "hive:" to the
// victim and slashed accounts when missing. Pure; safe to call before
// composing or after decoding.
func (r RestitutionClaimRecord) Normalize() RestitutionClaimRecord {
	out := r
	out.ClaimID = strings.TrimSpace(out.ClaimID)
	out.SlashTxID = strings.TrimSpace(out.SlashTxID)
	out.EvidenceKind = strings.TrimSpace(out.EvidenceKind)
	out.VictimTxID = strings.TrimSpace(out.VictimTxID)
	out.Memo = strings.TrimSpace(out.Memo)
	v := strings.TrimSpace(out.VictimAccount)
	if v != "" && !strings.HasPrefix(v, "hive:") {
		v = "hive:" + v
	}
	out.VictimAccount = v
	a := strings.TrimSpace(out.SlashedAccount)
	if a != "" && !strings.HasPrefix(a, "hive:") {
		a = "hive:" + a
	}
	out.SlashedAccount = a
	return out
}

// SafetySlashReverseAction tags the reversal kind; only the listed values
// are accepted by the apply path.
type SafetySlashReverseAction string

const (
	// ReverseActionCancel undoes a not-yet-finalized burn slice. Calls
	// LedgerSystem.CancelPendingSafetySlashBurn. No HIVE_CONSENSUS credit
	// is written by this action alone — pair with reverse_or_both to
	// re-credit the validator's bond.
	ReverseActionCancel SafetySlashReverseAction = "cancel"
	// ReverseActionReverse re-credits HIVE_CONSENSUS bond up to the
	// not-yet-burned portion of the original slash. Calls
	// LedgerSystem.ReverseSafetySlashConsensusDebit. The apply path caps
	// the amount at slashAmount - alreadyFinalizedBurn so governance
	// cannot mint past what was actually debited.
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

	// Amount is the requested HIVE_CONSENSUS re-credit in satoshis. Used
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
