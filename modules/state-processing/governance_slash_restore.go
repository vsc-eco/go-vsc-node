package state_engine

import (
	"encoding/json"
	"strconv"
	"strings"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	governance_db "vsc-node/modules/db/vsc/governance"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	governance "vsc-node/modules/governance"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// WireGovernanceForTest injects the governance store, election db, and system
// config into a test StateEngine so the external state_engine_test package can
// drive the vote handlers without the full plugin constellation. Production
// wires these via the constructor.
func (se *StateEngine) WireGovernanceForTest(gov governance_db.Governance, elec elections.Elections, sconf systemconfig.SystemConfig) {
	se.governanceDb = gov
	se.electionDb = elec
	se.sconf = sconf
}

// HandleSlashRestoreVoteForTest exposes handleSlashRestoreVote to external tests.
func (se *StateEngine) HandleSlashRestoreVoteForTest(payload []byte, voter, txID string, blockHeight uint64) {
	se.handleSlashRestoreVote(payload, voter, txID, blockHeight)
}

// handleSlashRestoreVote processes a vsc.slash_restore L1 custom_json: a witness
// approving the restoration of a wrongful safety slash while it is still in its
// pending window. The op is propose==vote — the first valid vote creates the
// proposal, each later distinct witness adds weight, and on crossing 2/3 of the
// (beneficiary-excluded) electorate the bond is restored via
// applySafetySlashReverse(action "both"). Payload:
//
//	{"id": "<slash_tx_id>", "account": "<slashed_account>"}
//
// The slash's evidence kind and amount are RESOLVED from the on-chain slash row
// (voters never supply them, so there is nothing to disagree on). Everything
// here is a deterministic function of L1-ordered votes + on-chain state, so
// every node reaches the same apply/expire decision (votes are tallied during
// block replay, exactly like the slash itself).
//
// Phase A is intentionally one-shot per slash: the proposal is terminal once
// applied or expired, and a failed attempt falls through to the reserve-payout
// path at maturity (the general backstop). The simple-cap window never extends
// the slash's fixed maturity.
func (se *StateEngine) handleSlashRestoreVote(payload []byte, voterAccount, txID string, blockHeight uint64) {
	if se == nil || se.governanceDb == nil {
		return
	}
	var req struct {
		Id      string `json:"id"`      // slash Hive tx id
		Account string `json:"account"` // slashed account
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Warn("vsc.slash_restore: malformed payload; ignoring", "tx", txID, "err", err)
		return
	}
	slashTxID := strings.TrimSpace(req.Id)
	slashedAcct := governance.NormalizeAccount(req.Account)
	if slashTxID == "" || slashedAcct == "" {
		log.Warn("vsc.slash_restore: missing id/account; ignoring", "tx", txID)
		return
	}
	voter := governance.NormalizeAccount(voterAccount)

	// Deterministic proposal id binds the slash tx and slashed account.
	proposalID := string(governance.ProposalSlashRestore) + ":" + slashTxID + ":" + slashedAcct

	prop, exists := se.getGovernanceProposalOrBlock(proposalID)

	// Eligibility is checked BEFORE creation so a non-member cannot anchor a
	// proposal's creation block (which would fix the expiry window + electorate
	// snapshot). The snapshot height is the creation block: this block for a new
	// proposal, the stored one for an existing proposal — stable across nodes.
	snapshotBlock := blockHeight
	if exists {
		snapshotBlock = prop.CreationBlock
	}
	electorate, ok := se.governanceElectorate(snapshotBlock)
	if !ok {
		log.Warn("vsc.slash_restore: no electorate snapshot; skipping vote", "proposal", proposalID)
		return
	}
	if !governanceIsMember(electorate, voter) {
		log.Debug("vsc.slash_restore: voter not in electorate snapshot; ignoring",
			"proposal", proposalID, "voter", voter)
		return
	}

	if !exists {
		// First eligible vote creates the proposal. Resolve the slash row to
		// learn the evidence kind and amount (beneficiary = slashed account).
		row, found := se.resolveSlashConsensusRowByTx(slashTxID, slashedAcct)
		if !found {
			log.Warn("vsc.slash_restore: no matching safety_slash_consensus row; ignoring",
				"tx", txID, "slash_tx_id", slashTxID, "account", slashedAcct)
			return
		}
		amount := -row.Amount
		if amount <= 0 {
			log.Warn("vsc.slash_restore: slash row has non-positive amount; ignoring",
				"tx", txID, "slash_tx_id", slashTxID)
			return
		}
		prop = governance_db.Proposal{
			ProposalId:     proposalID,
			Type:           string(governance.ProposalSlashRestore),
			Status:         string(governance.StatusOpen),
			CreationBlock:  blockHeight,
			Beneficiary:    slashedAcct,
			SlashTxId:      slashTxID,
			EvidenceKind:   parseEvidenceKindFromSlashRowID(row.Id),
			SlashedAccount: slashedAcct,
			Amount:         amount,
		}
		se.saveGovernanceProposalOrBlock(prop)
	}

	if prop.Status != string(governance.StatusOpen) {
		// Already applied or expired — votes are no-ops (terminal, one-shot).
		return
	}

	// Effective expiry = min(creation+expiry, slash maturity). The maturity cap
	// keeps the light path inside the pending window; it NEVER moves maturity.
	maturity := se.slashPendingMaturity(prop.SlashTxId, prop.EvidenceKind, prop.SlashedAccount)
	expiryBlocks := se.sconf.ConsensusParams().GovernanceProposalExpiry()
	if governance.IsExpired(blockHeight, prop.CreationBlock, expiryBlocks, maturity) {
		prop.Status = string(governance.StatusExpired)
		se.saveGovernanceProposalOrBlock(prop)
		log.Info("vsc.slash_restore: proposal expired without quorum",
			"proposal", proposalID, "creation", prop.CreationBlock, "height", blockHeight)
		return
	}

	se.recordGovernanceVoteOrBlock(governance_db.ProposalVote{
		ProposalId: proposalID, Voter: voter, BlockHeight: blockHeight, TxId: txID,
	})

	// Recompute the weighted tally over every recorded vote and apply on crossing.
	voters := se.governanceVoterSetOrBlock(proposalID)
	if !governance.IsApproved(electorate, prop.Beneficiary, voters) {
		return
	}

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      prop.SlashTxId,
		EvidenceKind:   prop.EvidenceKind,
		SlashedAccount: prop.SlashedAccount,
		Action:         safetyslash.ReverseActionBoth,
		Amount:         prop.Amount,
		Reason:         "governance slash_restore " + proposalID,
	}
	se.applySafetySlashReverse(rec.Normalize(), txID, blockHeight)

	prop.Status = string(governance.StatusApplied)
	prop.AppliedBlock = blockHeight
	prop.AppliedTxId = txID
	se.saveGovernanceProposalOrBlock(prop)
	log.Info("vsc.slash_restore: threshold crossed; bond restoration applied",
		"proposal", proposalID, "slashed", prop.SlashedAccount, "amount", prop.Amount, "height", blockHeight)
}

// ── deterministic store helpers (fail-stop on infra error, like blockingRetry) ──

func (se *StateEngine) getGovernanceProposalOrBlock(proposalID string) (governance_db.Proposal, bool) {
	var p governance_db.Proposal
	var found bool
	blockingRetry("GetGovernanceProposal", func() error {
		var err error
		p, found, err = se.governanceDb.GetProposal(proposalID)
		return err
	})
	return p, found
}

func (se *StateEngine) saveGovernanceProposalOrBlock(p governance_db.Proposal) {
	blockingRetry("SaveGovernanceProposal", func() error {
		return se.governanceDb.SaveProposal(p)
	})
}

func (se *StateEngine) recordGovernanceVoteOrBlock(v governance_db.ProposalVote) {
	blockingRetry("RecordGovernanceVote", func() error {
		return se.governanceDb.RecordVote(v)
	})
}

func (se *StateEngine) governanceVoterSetOrBlock(proposalID string) map[string]bool {
	var votes []governance_db.ProposalVote
	blockingRetry("GetGovernanceVotes", func() error {
		var err error
		votes, err = se.governanceDb.GetVotes(proposalID)
		return err
	})
	out := make(map[string]bool, len(votes))
	for _, v := range votes {
		out[v.Voter] = true
	}
	return out
}

// governanceElectorate builds the weighted member set from the election snapshot
// at height. found=false when no election covers the height (handled as
// "skip", not retried forever — see GetElectionInfoOrBlock).
func (se *StateEngine) governanceElectorate(height uint64) ([]governance.Member, bool) {
	if se.electionDb == nil {
		return nil, false
	}
	elec, ok := se.GetElectionInfoOrBlock(height)
	if !ok {
		return nil, false
	}
	members := make([]governance.Member, 0, len(elec.Members))
	for i, m := range elec.Members {
		w := uint64(1)
		if len(elec.Weights) == len(elec.Members) {
			w = elec.Weights[i]
		}
		members = append(members, governance.Member{Account: m.Account, Weight: w})
	}
	return members, true
}

func governanceIsMember(electorate []governance.Member, normalizedAcct string) bool {
	for _, m := range electorate {
		if governance.NormalizeAccount(m.Account) == normalizedAcct {
			return true
		}
	}
	return false
}

// resolveSlashConsensusRowByTx finds the principal-debit row for a slash given
// only its Hive tx id and the slashed account (the evidence kind is parsed from
// the row id afterwards). The ledger is account-indexed, so the account is
// required; one safety_slash_consensus row exists per slash tx.
func (se *StateEngine) resolveSlashConsensusRowByTx(slashTxID, normalizedAcct string) (ledgerDb.LedgerRecord, bool) {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return ledgerDb.LedgerRecord{}, false
	}
	owner := normalizeHiveAccount(normalizedAcct)
	var recs *[]ledgerDb.LedgerRecord
	blockingRetry("resolveSlashConsensusRowByTx", func() error {
		var err error
		recs, err = se.LedgerState.LedgerDb.GetLedgerRange(
			owner, 0, maxLedgerScanHeight, "hive_consensus",
			ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashConsensus}},
		)
		return err
	})
	if recs == nil {
		return ledgerDb.LedgerRecord{}, false
	}
	for _, r := range *recs {
		if r.TxId == slashTxID {
			return r, true
		}
	}
	return ledgerDb.LedgerRecord{}, false
}

// parseEvidenceKindFromSlashRowID extracts <kind> from a principal-debit row id
// of the form "<txID>#safety_slash#<kind>#consensus_debit#<acct>".
func parseEvidenceKindFromSlashRowID(id string) string {
	parts := strings.Split(id, "#")
	if len(parts) >= 3 && parts[1] == "safety_slash" {
		return parts[2]
	}
	return ""
}

// slashPendingMaturity returns the stored maturity height of this slash's pending
// burn row (its From field). 0 if no pending row exists (already matured to the
// reserve, cancelled, or slashed with no delay) — meaning no maturity cap.
func (se *StateEngine) slashPendingMaturity(slashTxID, evidenceKind, normalizedAcct string) uint64 {
	if se == nil || se.LedgerState == nil || se.LedgerState.LedgerDb == nil {
		return 0
	}
	var recs *[]ledgerDb.LedgerRecord
	blockingRetry("slashPendingMaturity", func() error {
		var err error
		recs, err = se.LedgerState.LedgerDb.GetLedgerRange(
			params.ProtocolSlashPendingBurnAccount, 0, maxLedgerScanHeight, "hive",
			ledgerDb.LedgerOptions{OpType: []string{ledgerSystem.LedgerTypeSafetySlashHiveBurnPending}},
		)
		return err
	})
	if recs == nil {
		return 0
	}
	prefix := slashTxID + "#safety_slash#" + evidenceKind + "#"
	for _, r := range *recs {
		if strings.HasPrefix(r.Id, prefix) {
			if m, err := strconv.ParseUint(strings.TrimSpace(r.From), 10, 64); err == nil {
				return m
			}
		}
	}
	return 0
}
