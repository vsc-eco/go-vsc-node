package state_engine

import (
	"encoding/json"
	"fmt"
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
// ON THE "SELF-PROTECTING" CLAIM — precisely what it does and does not buy.
//
// TRUE, and the main point: a coalition holding fewer than ceil(2/3) of seats
// cannot admit anyone on its own. Admission requires the same supermajority that
// controls the vault, so a minority acting alone can never grow itself, and the
// set can never SHRINK (there is no removal op anywhere in this build), so
// capture-by-subtraction is closed outright.
//
// The narrow caveat, recorded so nobody over-reads the claim later: required(W)
// = ceil(2W/3) increases by 0 or 1 when a seat is added, so at set sizes where
// it increases by 0 (W = 20 -> 21 both require 14) admitting a seat does not
// raise the bar. A coalition ONE vote short can therefore convert a single
// additional approval into a durable position rather than a one-off.
//
// That is a much smaller observation than it first appears, and it is NOT a
// ladder from a minority:
//   - being one vote short of ceil(2/3) already means ~65% of vetted, KYC'd,
//     UBO-capped operators are colluding, which is the catastrophe the vetting
//     is there to prevent — not a starting position an attacker reaches cheaply;
//   - the "additional approval" is a vote to admit a candidate who must first
//     pass off-chain vetting with a distinct beneficial owner. That vetting, not
//     this arithmetic, is the load-bearing control;
//   - the churn cap admits at most one seat per election and every admission is
//     public, so any such growth is slow and visible.
//
// The real lesson is where the trust actually sits: this threshold is a
// coordination bar, and the security of admission rests on VETTING plus the
// no-shrink property. See the note below on what the chain does and does not
// verify about that vetting.
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
	if se == nil || se.governanceDb == nil || se.poaSeats == nil || se.sconf == nil {
		// sconf is required for the net-id check and the vote window; without it
		// the op cannot be evaluated deterministically, so it is not evaluated.
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
	if req.NetId != se.sconf.NetId() {
		log.Debug("vsc.admit_vote: wrong net id; ignoring", "tx", txID, "net_id", req.NetId)
		return
	}

	candidate := governance.NormalizeAccount(req.Candidate)
	uboId := canonicalUboId(req.UboId)
	if candidate == "" {
		log.Warn("vsc.admit_vote: missing candidate; ignoring", "tx", txID)
		return
	}

	// ★ CHARSET VALIDATION IS SECURITY, NOT HYGIENE.
	//
	// The proposal id is a hash of candidate || 0x00 || ubo, and that NUL
	// delimiter is only unambiguous while neither field can CONTAIN a NUL. These
	// are raw JSON strings — JSON \u0000 decodes to a real NUL byte and passes
	// straight through both normalisers — so without this check an attacker can
	// shift the field boundary and make two different (candidate, owner) pairs
	// hash to the same proposal, pooling votes cast for one pairing into the
	// admission of another.
	//
	// Hive's own account charset is the natural bound for the candidate; the
	// owner id is an opaque vetting reference, so it is bounded to printable
	// non-space ASCII and a sane length rather than given a format.
	if !isPlausibleHiveAccount(candidate) {
		log.Warn("vsc.admit_vote: candidate is not a syntactically valid Hive account; ignoring",
			"tx", txID, "candidate", candidate)
		return
	}
	if !isPlausibleUboId(uboId) {
		log.Warn("vsc.admit_vote: ubo_id contains control characters or is out of bounds; ignoring", "tx", txID)
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
	electorate, ok := se.poaSeatElectorate(blockHeight)
	if !ok {
		return
	}
	if !governanceIsMember(electorate, voter) {
		log.Debug("vsc.admit_vote: voter holds no seat; ignoring", "tx", txID, "voter", voter)
		return
	}

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
	snapshot, ok := se.poaSeatElectorate(prop.CreationBlock)
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

	// ★ SEAT FROM THE PROPOSAL, NOT FROM THIS VOTE.
	//
	// Voters approve a PROPOSAL. Seating whatever the crossing vote happens to
	// have parsed would mean the last voter — not the electorate — decides who
	// is actually admitted, whenever its locals differ from the proposal's
	// stored fields for any reason (a hash boundary shift, a future change to
	// how the id is derived, or simply a bug). Reading the stored fields makes
	// "what was approved" and "what was written" the same object by
	// construction, so no divergence between them is even expressible.
	seat := poaseats.Seat{
		Account:        prop.Candidate,
		UboId:          prop.UboId,
		AdmittedHeight: blockHeight,
		AdmittedTxId:   txID,
	}
	// Fail-stop on infra, surface deterministic refusals. An admission that
	// crossed the threshold on every node must be WRITTEN on every node, or the
	// registries diverge on a membership decision.
	var admitErr error
	blockingRetry(fmt.Sprintf("poaSeats.AdmitSeat(vote,%s,%d)", prop.Candidate, blockHeight), func() error {
		admitErr = se.poaSeats.AdmitSeat(seat)
		if admitErr != nil && isDuplicateSeatErr(admitErr) {
			return nil
		}
		return admitErr
	})
	if err := admitErr; err != nil {
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
func (se *StateEngine) poaSeatElectorate(height uint64) ([]governance.Member, bool) {
	if se.poaSeats == nil {
		return nil, false
	}
	seats, err := se.poaSeats.GetSeatsAtHeight(height)
	if err != nil {
		log.Error("poa: seat electorate read failed; refusing to tally against a partial set",
			"height", height, "err", err)
		return nil, false
	}
	if len(seats) == 0 {
		return nil, false
	}
	accounts := make([]string, 0, len(seats))
	for _, s := range seats {
		accounts = append(accounts, poaseats.NormalizeAccount(s.Account))
	}
	return governance.SeatElectorate(accounts), true
}

// PoaAdmitVoteActive reports whether the admission op is dispatched at height.
// Exposed so the dispatch site reads as one condition.
func (se *StateEngine) PoaAdmitVoteActive(blockHeight uint64) bool {
	return consensusversion.PoaAdmissionOpsActive(se.ActiveConsensusVersion(blockHeight))
}

// canonicalUboId normalises a beneficial-owner id so that "one seat per owner"
// binds on the OWNER rather than on a spelling of the owner.
//
// The uniqueness backstop is a byte-exact Mongo unique index. Without
// canonicalisation, "UBO-7F3C" and "ubo-7f3c" are two different owners as far
// as that index is concerned, and the single structural defence against one
// vetted operator holding several seats is defeated by a shift key. Case-folding
// is safe here precisely because this is an opaque internal reference minted by
// the vetting process, not a cryptographic identifier whose case is meaningful.
func canonicalUboId(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// isPlausibleHiveAccount bounds the candidate to Hive's own account charset:
// lowercase alphanumerics with '-' and '.' as internal separators, 3..16 chars.
// It is deliberately a SYNTAX check, not an existence check — whether the
// account exists is not knowable here, and is anyway the vetting process's job.
// Its security purpose is to guarantee the string cannot contain the NUL byte
// that delimits the proposal-id hash, or any other control character.
func isPlausibleHiveAccount(a string) bool {
	if len(a) < 3 || len(a) > 16 {
		return false
	}
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
			// Never leading or trailing, so an id cannot be padded into a
			// different-looking-but-equal form.
			if i == 0 || i == len(a)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isPlausibleUboId bounds the owner id to printable, non-space ASCII of a sane
// length. The point is not to impose a format on the vetting process's
// identifiers — it is to guarantee no control byte (above all NUL) can reach the
// proposal-id hash, where it would let a crafted pair shift the field boundary.
func isPlausibleUboId(u string) bool {
	if len(u) == 0 || len(u) > 128 {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] <= 0x20 || u[i] >= 0x7f {
			return false
		}
	}
	return true
}
