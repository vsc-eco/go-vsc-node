package state_engine

import (
	"strconv"
	"strings"

	"vsc-node/modules/contract/session"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
)

// pendulumPoolReserveReader reads each whitelisted pool's published HBD-side
// reserve from the contract state DB. The pool MUST publish a base-10 ASCII
// integer at PoolReserveStateKey on every swap; this reader is read-only and
// returns (0, false) if the key is missing or unparseable.
//
// Determinism: GetLastOutput pins the output to the highest block_height ≤
// requested. Two nodes with identical state DBs return identical bytes for
// the same (contractID, blockHeight). The geometry tick passes the persisted
// snapshot's TickBlockHeight, which is itself a deterministic function of
// the on-chain block stream.
type pendulumPoolReserveReader struct {
	se *StateEngine
}

func (r *pendulumPoolReserveReader) ReadPoolHBDReserve(contractID string, blockHeight uint64) (int64, bool) {
	if r == nil || r.se == nil || r.se.contractDb == nil || r.se.contractState == nil || r.se.da == nil {
		return 0, false
	}
	cs := contract_session.NewCallSession(
		r.se.da,
		r.se.contractDb,
		r.se.contractState,
		r.se.tssKeys,
		blockHeight,
		nil,
	)
	store := cs.GetStateStore(contractID)
	if store == nil {
		return 0, false
	}
	raw := store.Get(pendulumoracle.PoolReserveStateKey)
	if len(raw) == 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// pendulumCommitteeBondReader sums HIVE_CONSENSUS over the current election's
// committee at the supplied block height. Members are returned as the
// "hive:account" form so callers can correlate with slash payloads.
type pendulumCommitteeBondReader struct {
	se *StateEngine
}

func (r *pendulumCommitteeBondReader) ReadCommitteeBond(blockHeight uint64) ([]string, int64) {
	if r == nil || r.se == nil || r.se.electionDb == nil || r.se.LedgerSystem == nil {
		return nil, 0
	}
	election, err := r.se.electionDb.GetElectionByHeight(blockHeight)
	if err != nil || len(election.Members) == 0 {
		return nil, 0
	}
	members := make([]string, 0, len(election.Members))
	var total int64
	for _, m := range election.Members {
		acct := m.Account
		if !strings.HasPrefix(acct, "hive:") {
			acct = "hive:" + acct
		}
		bond := r.se.LedgerSystem.GetBalance(acct, blockHeight, "hive_consensus")
		if bond <= 0 {
			continue
		}
		members = append(members, acct)
		total += bond
	}
	return members, total
}
