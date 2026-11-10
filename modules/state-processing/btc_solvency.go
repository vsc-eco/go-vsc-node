package state_engine

import (
	"context"
	"encoding/json"
	"slices"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/consensus_state"
)

// N4 — the BTC solvency watch's votes (from 0.13.0), THORChain's
// MsgSolvency + processSolvencyAttestation in Magi terms. At each checkpoint
// (a multiple of params.BTC_SOLVENCY_CHECK_EVERY) a witness checks that every
// confirmed, unreserved UTXO the BTC mapping contract has registered is still
// unspent on its own bitcoind (modules/oracle/chain/solvency_watch.go). It
// votes only when what it sees differs from the current flag: "vsc.btc_solvency"
// {"c": checkpoint, "insolvent": bool, ...} from its own account, with active
// authority. Every node tallies the votes the same way: a vote counts once per
// account per checkpoint, from a member of the election at the checkpoint,
// while the block is within the interval after it. When the insolvent votes
// hold 2/3 of that election's weight the BTC vault keysigns halt
// (BtcSolvencyHalted); 2/3 solvent lifts it. Only this flag: never the
// governance vsc.tss_halt or the contract's theft flag.

// btcSolvencyVote is the vote payload. The tally reads Checkpoint and
// Insolvent; the rest is what the witness saw, kept in the op for operators.
type btcSolvencyVote struct {
	Checkpoint  uint64 `json:"c"`
	Insolvent   bool   `json:"insolvent"`
	MissingSats int64  `json:"missing_sats,omitempty"`
	Missing     int    `json:"missing,omitempty"`
	Tip         uint64 `json:"tip,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

// tallyBtcSolvencyVote applies one vote (pure). members and weights are the
// election active at the vote's checkpoint (a member without a weight weighs
// 1). It returns the new state and whether the vote was counted.
func tallyBtcSolvencyVote(st consensus_state.BtcSolvencyState, voter string, v btcSolvencyVote, height uint64,
	members []string, weights []uint64) (consensus_state.BtcSolvencyState, bool) {
	every := params.BTC_SOLVENCY_CHECK_EVERY
	if st.WatchOff || v.Checkpoint%every != 0 || v.Checkpoint > height || height-v.Checkpoint >= every {
		return st, false
	}
	if !slices.Contains(members, voter) {
		return st, false
	}
	// A vote for an earlier checkpoint than the one being counted cannot
	// arrive: blocks are processed in order and checkpoints are a whole
	// interval apart, so it is already outside its own interval (above).
	if v.Checkpoint != st.Votes.Checkpoint {
		st.Votes = consensus_state.BtcSolvencyVotes{Checkpoint: v.Checkpoint}
	}
	if slices.Contains(st.Votes.Insolvent, voter) || slices.Contains(st.Votes.Solvent, voter) {
		return st, false
	}
	if v.Insolvent {
		st.Votes.Insolvent = append(slices.Clone(st.Votes.Insolvent), voter)
	} else {
		st.Votes.Solvent = append(slices.Clone(st.Votes.Solvent), voter)
	}
	weight := func(i int) uint64 {
		if i < len(weights) {
			return weights[i]
		}
		return 1
	}
	var total, insolvent, solvent uint64
	for i, m := range members {
		w := weight(i)
		total += w
		if slices.Contains(st.Votes.Insolvent, m) {
			insolvent += w
		} else if slices.Contains(st.Votes.Solvent, m) {
			solvent += w
		}
	}
	if total == 0 {
		return st, true
	}
	if !st.Halted && insolvent*3 >= total*2 {
		st.Halted, st.HaltHeight = true, height
	} else if st.Halted && solvent*3 >= total*2 {
		st.Halted, st.HaltHeight = false, 0
	}
	return st, true
}

// btcSolvencyWatchActive reports whether the watch's ops are in force at height.
func (se *StateEngine) btcSolvencyWatchActive(height uint64) bool {
	if se.electionDb == nil {
		return false
	}
	return consensusversion.BtcSolvencyWatchActive(se.ActiveConsensusVersion(height))
}

// trustedSelfActive reports whether SELF-INJECTION-1's trusted-Self rebuild is in
// force at height. When true, every tx/op decode restores TxSelf from the
// trusted block/envelope context instead of trusting the decoded payload.
func (se *StateEngine) trustedSelfActive(height uint64) bool {
	return consensusversion.SelfInjectionTrustedSelfActive(se.ActiveConsensusVersion(height))
}

// cloneTxSelf returns a deep copy of a TxSelf whose slice fields do NOT share a
// backing array with the original. SELF-INJECTION-1 restores the trusted Self
// AFTER json.Unmarshal; but `TxX{Self: txSelf}` aliases txSelf's RequiredAuths
// slice into the decoded struct, and Unmarshal overwrites that shared array in
// place, corrupting the original. Capturing this clone BEFORE the decode gives
// the restore a pristine trusted Self to assign back.
func cloneTxSelf(s TxSelf) TxSelf {
	s.RequiredAuths = append([]string(nil), s.RequiredAuths...)
	s.RequiredPostingAuths = append([]string(nil), s.RequiredPostingAuths...)
	return s
}

// processBtcSolvencyVote handles one "vsc.btc_solvency" op.
func (se *StateEngine) processBtcSolvencyVote(voter string, raw []byte, height uint64, txId string) {
	if se.consensusState == nil || !se.btcSolvencyWatchActive(height) {
		return
	}
	var v btcSolvencyVote
	if err := json.Unmarshal(raw, &v); err != nil {
		log.Warn("vsc.btc_solvency: malformed payload; ignoring", "tx", txId, "err", err)
		return
	}
	election, err := se.electionDb.GetElectionByHeight(v.Checkpoint)
	if err != nil {
		log.Warn("vsc.btc_solvency: no election at the checkpoint; ignoring", "tx", txId, "c", v.Checkpoint, "err", err)
		return
	}
	members := make([]string, len(election.Members))
	for i, m := range election.Members {
		members[i] = m.Account
	}
	se.chainConsensusMu.RLock()
	cur := se.chainConsensusCache.BtcSolvency
	se.chainConsensusMu.RUnlock()
	next, counted := tallyBtcSolvencyVote(cur, voter, v, height, members, election.Weights)
	if !counted {
		return
	}
	if err := se.consensusState.SetBtcSolvency(context.Background(), next); err != nil {
		log.Error("vsc.btc_solvency: SetBtcSolvency FAILED — vote not recorded on this node", "tx", txId, "err", err)
		return
	}
	se.refreshChainConsensusCache()
	if next.Halted != cur.Halted {
		log.Warn("BTC solvency watch: witnesses holding 2/3 of the weight agree; BTC vault keysigns halted changed",
			"halted", next.Halted, "c", v.Checkpoint, "insolvent", next.Votes.Insolvent, "solvent", next.Votes.Solvent, "height", height)
	} else {
		log.Info("vsc.btc_solvency vote counted", "voter", voter, "c", v.Checkpoint, "insolvent", v.Insolvent,
			"missing_sats", v.MissingSats, "missing", v.Missing, "tip", v.Tip)
	}
}

// setBtcSolvencyWatch is the governance escape hatch (THORChain's
// StopSolvencyCheck): off ignores votes and lifts the halt; on resumes.
func (se *StateEngine) setBtcSolvencyWatch(on bool, txId string) {
	se.chainConsensusMu.RLock()
	st := se.chainConsensusCache.BtcSolvency
	se.chainConsensusMu.RUnlock()
	st.WatchOff = !on
	if !on {
		st.Halted, st.HaltHeight, st.Votes = false, 0, consensus_state.BtcSolvencyVotes{}
	}
	if err := se.consensusState.SetBtcSolvency(context.Background(), st); err != nil {
		log.Error("vsc.tss_halt btc_solvency: SetBtcSolvency FAILED — not applied on this node; re-broadcast the op", "on", on, "err", err)
		return
	}
	se.refreshChainConsensusCache()
	log.Warn("BTC solvency watch switched by governance", "on", on, "tx", txId)
}

// BtcSolvencyHalted reports whether the witnesses' solvency votes have halted
// BTC vault keysigns (N4). Read by the BTC sign gate through GetScheduler.
func (se *StateEngine) BtcSolvencyHalted() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.BtcSolvency.Halted
}

// BtcSolvencyWatchOff reports whether governance has turned the watch off; a
// witness does not check or vote meanwhile.
func (se *StateEngine) BtcSolvencyWatchOff() bool {
	se.chainConsensusMu.RLock()
	defer se.chainConsensusMu.RUnlock()
	return se.chainConsensusCache.BtcSolvency.WatchOff
}
