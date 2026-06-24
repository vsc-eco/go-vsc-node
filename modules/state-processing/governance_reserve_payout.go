package state_engine

import (
	"encoding/json"
	"strings"

	governance_db "vsc-node/modules/db/vsc/governance"
	governance "vsc-node/modules/governance"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// handleReservePayoutCreate processes a vsc.reserve_payout L1 custom_json: a
// witness PROPOSES a disbursement from the keyless insurance reserve. Payload:
//
//	{"recipient": "<account>", "amount": <sats>, "reason": "<text>"}
//
// Unlike slash_restore (whose parameters are resolved from an on-chain slash),
// a reserve payout's parameters are CHOSEN, so the create op introduces them and
// the proposal id is derived from the content + this op's tx id. Two distinct
// creates are two distinct proposals; other witnesses approve by referencing the
// id via vsc.reserve_vote. The creator's create op counts as their first vote.
// On crossing 2/3 of the recipient-excluded electorate the payout is disbursed,
// capped at the reserve balance (it can never mint or overdraw — see ReservePayout).
func (se *StateEngine) handleReservePayoutCreate(payload []byte, creatorAccount, txID string, blockHeight uint64) {
	if se == nil || se.governanceDb == nil {
		return
	}
	var req struct {
		Recipient string `json:"recipient"`
		Amount    int64  `json:"amount"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Warn("vsc.reserve_payout: malformed payload; ignoring", "tx", txID, "err", err)
		return
	}
	recipient := governance.NormalizeAccount(req.Recipient)
	if recipient == "" || req.Amount <= 0 {
		log.Warn("vsc.reserve_payout: missing recipient or non-positive amount; ignoring", "tx", txID)
		return
	}
	creator := governance.NormalizeAccount(creatorAccount)

	// Creation eligibility: only a current electorate member may propose a reserve
	// payout (it disburses to an arbitrary account, so creation is witness-gated).
	// The snapshot is THIS block — it also becomes the proposal's stable snapshot.
	electorate, ok := se.governanceElectorate(blockHeight)
	if !ok {
		log.Warn("vsc.reserve_payout: no electorate snapshot; ignoring", "tx", txID)
		return
	}
	if !governanceIsMember(electorate, creator) {
		log.Debug("vsc.reserve_payout: creator not a witness; ignoring", "tx", txID, "creator", creator)
		return
	}

	proposalID := governance.ReservePayoutProposalID(recipient, req.Amount, req.Reason, txID)

	// Create the proposal if it doesn't already exist (replay of the same create
	// op is idempotent: same txID → same id → same row). Don't clobber an existing
	// proposal's status (a re-observed create must not reopen an applied payout).
	if _, exists := se.getGovernanceProposalOrBlock(proposalID); !exists {
		se.saveGovernanceProposalOrBlock(governance_db.Proposal{
			ProposalId:    proposalID,
			Type:          string(governance.ProposalReservePayout),
			Status:        string(governance.StatusOpen),
			CreationBlock: blockHeight,
			Beneficiary:   recipient,
			Recipient:     recipient,
			Reason:        req.Reason,
			Amount:        req.Amount,
		})
	}

	// The creator's op is their first vote. If the creator IS the recipient it is
	// excluded from the tally (no self-dealing) but the proposal still stands.
	se.castReservePayoutVote(proposalID, creator, txID, blockHeight)
}

// handleReservePayoutVote processes a vsc.reserve_vote L1 custom_json: a witness
// approves an existing reserve-payout proposal by id. Payload: {"id": "<id>"}.
func (se *StateEngine) handleReservePayoutVote(payload []byte, voterAccount, txID string, blockHeight uint64) {
	if se == nil || se.governanceDb == nil {
		return
	}
	var req struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Warn("vsc.reserve_vote: malformed payload; ignoring", "tx", txID, "err", err)
		return
	}
	proposalID := strings.TrimSpace(req.Id)
	if proposalID == "" {
		return
	}
	se.castReservePayoutVote(proposalID, governance.NormalizeAccount(voterAccount), txID, blockHeight)
}

// castReservePayoutVote records one approval on an existing reserve-payout
// proposal and disburses when the 2/3 threshold is crossed. Shared by the create
// op (creator's first vote) and the vote op. Deterministic: all inputs are
// L1-ordered + on-chain, so every node reaches the same apply decision.
func (se *StateEngine) castReservePayoutVote(proposalID, voter, txID string, blockHeight uint64) {
	prop, exists := se.getGovernanceProposalOrBlock(proposalID)
	if !exists {
		// Vote for an unknown proposal (e.g. it arrived before its create op).
		// Ignored deterministically — the voter re-votes once the create lands.
		log.Debug("vsc.reserve_vote: unknown proposal; ignoring", "proposal", proposalID, "voter", voter)
		return
	}
	if prop.Type != string(governance.ProposalReservePayout) {
		return
	}
	if prop.Status != string(governance.StatusOpen) {
		return // terminal (applied or expired)
	}

	// Reserve payouts are not bounded by a slash pending window → no maturity cap;
	// the effective expiry is simply creation+expiry.
	expiryBlocks := se.sconf.ConsensusParams().GovernanceProposalExpiry()
	if governance.IsExpired(blockHeight, prop.CreationBlock, expiryBlocks, 0) {
		prop.Status = string(governance.StatusExpired)
		se.saveGovernanceProposalOrBlock(prop)
		log.Info("vsc.reserve_payout: proposal expired without quorum", "proposal", proposalID)
		return
	}

	electorate, ok := se.governanceElectorate(prop.CreationBlock)
	if !ok {
		return
	}
	if !governanceIsMember(electorate, voter) {
		log.Debug("vsc.reserve_payout: voter not in electorate snapshot; ignoring",
			"proposal", proposalID, "voter", voter)
		return
	}

	se.recordGovernanceVoteOrBlock(governance_db.ProposalVote{
		ProposalId: proposalID, Voter: voter, BlockHeight: blockHeight, TxId: txID,
	})

	voters := se.governanceVoterSetOrBlock(proposalID)
	if !governance.IsApproved(electorate, prop.Beneficiary, voters) {
		return
	}

	res := se.LedgerSystem.ReservePayout(ledgerSystem.ReservePayoutParams{
		ProposalID:  proposalID,
		Recipient:   prop.Recipient,
		Amount:      prop.Amount,
		BlockHeight: blockHeight,
		Reason:      prop.Reason,
	})
	if !res.Ok {
		// e.g. reserve empty — leave the proposal open so a later vote can retry if
		// the reserve is funded before expiry.
		log.Warn("vsc.reserve_payout: ledger payout rejected; proposal stays open",
			"proposal", proposalID, "msg", res.Msg)
		return
	}
	prop.Status = string(governance.StatusApplied)
	prop.AppliedBlock = blockHeight
	prop.AppliedTxId = txID
	se.saveGovernanceProposalOrBlock(prop)
	log.Info("vsc.reserve_payout: threshold crossed; reserve payout applied",
		"proposal", proposalID, "recipient", prop.Recipient, "amount", prop.Amount, "height", blockHeight)
}

// HandleReservePayoutCreateForTest / HandleReservePayoutVoteForTest expose the
// reserve-payout handlers to the external state_engine_test package.
func (se *StateEngine) HandleReservePayoutCreateForTest(payload []byte, creator, txID string, blockHeight uint64) {
	se.handleReservePayoutCreate(payload, creator, txID, blockHeight)
}
func (se *StateEngine) HandleReservePayoutVoteForTest(payload []byte, voter, txID string, blockHeight uint64) {
	se.handleReservePayoutVote(payload, voter, txID, blockHeight)
}
