package state_engine

import (
	"encoding/json"
	"strings"

	"vsc-node/modules/common/consensusversion"
	governance_db "vsc-node/modules/db/vsc/governance"
	"vsc-node/modules/db/vsc/poaseats"
	governance "vsc-node/modules/governance"
)

// POA seat admission — the vsc.admit_vote op.
//
// This is a clone of the witness-vote governance mechanism already in the tree
// (vsc.reserve_payout / vsc.reserve_vote): propose-is-first-vote, a ceil(2/3)
// beneficiary-excluded threshold, an expiry window, and one-shot terminality. It
// reuses that engine's PURE core (modules/governance) and its proposal store
// rather than re-implementing the tally — the determinism argument for those is
// already made and already reviewed.
//
// TWO THINGS DIFFER FROM THE GOVERNANCE TRIO, both deliberate:
//
//  1. THE ELECTORATE IS SEATS, NOT THE ELECTED COMMITTEE, and each seat carries
//     exactly one vote. A seat that is temporarily out of the committee (dropped
//     for a liveness fault) still votes, because it is still an admitted
//     operator; and no amount of stake buys a second vote.
//
//  2. THERE IS NO CREATE OP. Every vote for the same (candidate, ubo) converges
//     on one proposal id derived from that pair, so the first vote opens the
//     proposal. With a create op, or with a tx id folded into the id, votes
//     would scatter across per-tx proposals and the threshold could never be
//     reached.
//
// WHY THE THRESHOLD IS THE THEFT THRESHOLD: admission at ceil(2/3) of seats is
// SELF-PROTECTING. A coalition below 2/3 cannot vote accomplices in to REACH
// 2/3, and a coalition already at 2/3 gains nothing by admitting more — it
// already controls the vault. That property holds only while the seat set cannot
// SHRINK, which is why there is no removal op anywhere in this build and why the
// seat gate is placed so it can never starve the committee.
//
// WHAT THIS DOES NOT DO: it does not vet anybody. The chain enforces that a UBO
// string is present and unique. It cannot and does not verify that the string is
// TRUE. KYC/UBO vetting is an off-chain precondition to casting a vote at all,
// and treating the on-chain uniqueness check as vetting would be a serious
// misreading of what is built here.

// handleAdmitVote processes a vsc.admit_vote L1 custom_json. Payload:
//
//	{"candidate": "<account>", "ubo_id": "<beneficial owner id>", "net_id": "<net>"}
//
// Every input is L1-ordered and on-chain, so every node reaches the same
// admit/expire decision during replay.
func (se *StateEngine) handleAdmitVote(payload []byte, voterAccount, txID string, blockHeight uint64) {
	if se == nil || se.governanceDb == nil || se.poaSeats == nil {
		return
	}

	var req struct {
		Candidate string `json:"candidate"`
		UboId     string `json:"ubo_id"`
		NetId     string `json:"net_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Warn("vsc.admit_vote: malformed payload; ignoring", "tx", txID, "err", err)
		return
	}
	if se.sconf != nil && req.NetId != se.sconf.NetId() {
		log.Debug("vsc.admit_vote: wrong net id; ignoring", "tx", txID, "net_id", req.NetId)
		return
	}

	candidate := governance.NormalizeAccount(req.Candidate)
	uboId := strings.TrimSpace(req.UboId)
	if candidate == "" {
		log.Warn("vsc.admit_vote: missing candidate; ignoring", "tx", txID)
		return
	}
	if uboId == "" {
		// A blank UBO is refused rather than defaulted: it is the ONLY thing
		// standing between "one operator, one seat" and an operator quietly
		// holding several. An empty id would collide with every other empty id
		// under the sparse unique index, so the cap would silently stop binding.
		log.Warn("vsc.admit_vote: missing ubo_id; ignoring (one-seat-per-owner cannot be enforced without it)", "tx", txID)
		return
	}

	voter := governance.NormalizeAccount(voterAccount)

	// Electorate: the seats as of THIS block. Also the eligibility check — only a
	// seated operator votes on admission.
	electorate, seatAccounts, ok := se.poaSeatElectorate(blockHeight)
	if !ok {
		return
	}
	if !governanceIsMember(electorate, voter) {
		log.Debug("vsc.admit_vote: voter holds no seat; ignoring", "tx", txID, "voter", voter)
		return
	}
	_ = seatAccounts

	// Already-seated candidate, or a UBO that already holds a seat: the vote is
	// moot. Checked BEFORE opening a proposal so a duplicate never accumulates
	// votes that could not be applied.
	if _, exists, err := se.poaSeats.GetSeat(candidate); err != nil {
		log.Error("vsc.admit_vote: seat read failed; ignoring this vote (it can be recast)", "tx", txID, "err", err)
		return
	} else if exists {
		log.Debug("vsc.admit_vote: candidate already holds a seat; ignoring", "tx", txID, "candidate", candidate)
		return
	}
	if held, exists, err := se.poaSeats.GetSeatByUbo(uboId); err != nil {
		log.Error("vsc.admit_vote: ubo read failed; ignoring this vote (it can be recast)", "tx", txID, "err", err)
		return
	} else if exists {
		log.Warn("vsc.admit_vote: beneficial owner already holds a seat; ignoring",
			"tx", txID, "candidate", candidate, "held_by", held.Account)
		return
	}

	proposalID := governance.AdmitSeatProposalID(candidate, uboId)

	prop, exists := se.getGovernanceProposalOrBlock(proposalID)
	if !exists {
		// First vote opens the proposal. Beneficiary is the candidate, so the
		// shared engine excludes it from the electorate — a no-op here (a
		// candidate holds no seat by definition, checked above), but it keeps the
		// invariant that a beneficiary never votes on its own proposal true for
		// this proposal type as well.
		prop = governance_db.Proposal{
			ProposalId:    proposalID,
			Type:          string(governance.ProposalAdmitSeat),
			Status:        string(governance.StatusOpen),
			CreationBlock: blockHeight,
			Beneficiary:   candidate,
			Candidate:     candidate,
			UboId:         uboId,
		}
		se.saveGovernanceProposalOrBlock(prop)
		log.Info("vsc.admit_vote: admission proposal opened",
			"proposal", proposalID, "candidate", candidate, "ubo", uboId,
			"opened_by", voter, "height", blockHeight)
	}

	if prop.Type != string(governance.ProposalAdmitSeat) {
		return
	}
	if prop.Status != string(governance.StatusOpen) {
		return // terminal: applied or expired
	}

	// Admission has no maturity cap (nothing external bounds the window), so the
	// effective expiry is simply open+window.
	window := se.sconf.ConsensusParams().EffectivePoaAdmitVoteWindow()
	if governance.IsExpired(blockHeight, prop.CreationBlock, window, 0) {
		prop.Status = string(governance.StatusExpired)
		se.saveGovernanceProposalOrBlock(prop)
		log.Info("vsc.admit_vote: proposal expired without a 2/3 seat majority",
			"proposal", proposalID, "candidate", prop.Candidate)
		return
	}

	// The electorate SNAPSHOT is taken at the proposal's creation block, not at
	// each vote. Otherwise the denominator moves under a proposal while it is
	// open: seats admitted mid-window would raise the bar for a vote already
	// cast, and — worse — a shrinking committee would lower it. Same discipline
	// as the reserve-payout path.
	snapshot, _, ok := se.poaSeatElectorate(prop.CreationBlock)
	if !ok {
		return
	}
	if !governanceIsMember(snapshot, voter) {
		log.Debug("vsc.admit_vote: voter not in the proposal's seat snapshot; ignoring",
			"proposal", proposalID, "voter", voter)
		return
	}

	// One row per (proposal, voter): a re-vote upserts rather than double-counts.
	se.recordGovernanceVoteOrBlock(governance_db.ProposalVote{
		ProposalId: proposalID, Voter: voter, BlockHeight: blockHeight, TxId: txID,
	})

	voters := se.governanceVoterSetOrBlock(proposalID)
	if !governance.IsApproved(snapshot, prop.Beneficiary, voters) {
		log.Debug("vsc.admit_vote: vote recorded, threshold not yet met",
			"proposal", proposalID, "voted", governance.Tally(snapshot, prop.Beneficiary, voters),
			"required", governance.RequiredWeight(snapshot, prop.Beneficiary))
		return
	}

	seat := poaseats.Seat{
		Account:        candidate,
		UboId:          uboId,
		AdmittedHeight: blockHeight,
		AdmittedTxId:   txID,
	}
	if err := se.poaSeats.AdmitSeat(seat); err != nil {
		// The registry refused (a race against another admission for the same
		// account or owner). Leave the proposal OPEN rather than marking it
		// applied: nothing was seated, and marking it applied would record an
		// admission that did not happen.
		log.Error("vsc.admit_vote: threshold met but the seat write was refused; proposal stays open",
			"proposal", proposalID, "candidate", candidate, "err", err)
		return
	}

	prop.Status = string(governance.StatusApplied)
	prop.AppliedBlock = blockHeight
	prop.AppliedTxId = txID
	se.saveGovernanceProposalOrBlock(prop)

	log.Info("vsc.admit_vote: SEAT ADMITTED by a 2/3 majority of seats",
		"proposal", proposalID, "candidate", candidate, "ubo", uboId,
		"seats", len(snapshot), "required", governance.RequiredWeight(snapshot, prop.Beneficiary),
		"height", blockHeight, "tx", txID)
}

// poaSeatElectorate builds the one-vote-per-seat electorate as of height.
//
// ok=false means "could not resolve" and every caller ABORTS on it rather than
// proceeding with a partial electorate: a short electorate lowers the ceil(2/3)
// bar, so a transient read failure could otherwise let a minority admit a seat.
func (se *StateEngine) poaSeatElectorate(height uint64) ([]governance.Member, []string, bool) {
	if se.poaSeats == nil {
		return nil, nil, false
	}
	seats, err := se.poaSeats.GetSeatsAtHeight(height)
	if err != nil {
		log.Error("poa: seat electorate read failed; refusing to tally against a partial set",
			"height", height, "err", err)
		return nil, nil, false
	}
	if len(seats) == 0 {
		return nil, nil, false
	}
	accounts := make([]string, 0, len(seats))
	for _, s := range seats {
		accounts = append(accounts, poaseats.NormalizeAccount(s.Account))
	}
	return governance.SeatElectorate(accounts), accounts, true
}

// PoaAdmitVoteActive reports whether the admission op is dispatched at height.
// Exposed so the dispatch site reads as one condition.
func (se *StateEngine) PoaAdmitVoteActive(blockHeight uint64) bool {
	return consensusversion.PoaAdmissionOpsActive(se.ActiveConsensusVersion(blockHeight))
}
