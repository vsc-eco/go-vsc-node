package state_engine

import (
	"strings"

	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulum_settlements "vsc-node/modules/db/vsc/pendulum_settlements"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// PendulumPoolReserveReaderForTest exposes the unexported
// pendulumPoolReserveReader so external test packages can drive the
// production read path without standing up the full state engine.
func (se *StateEngine) PendulumPoolReserveReaderForTest() pendulumoracle.PoolReserveReader {
	return &pendulumPoolReserveReader{se: se}
}

// NewForGeometryTest constructs a minimum-viable StateEngine wired only with
// the dependencies the pendulum geometry / pool-reserve readers consume. Skips
// the full constructor so geometry-only tests don't need a wasm runtime, RC
// system, etc.
func NewForGeometryTest(
	ls ledgerSystem.LedgerSystem,
	electionDb elections.Elections,
	balanceDb ledgerDb.Balances,
	pendulumOracleDb pendulum_oracle.PendulumOracleSnapshots,
	pendulumSettlementsDb pendulum_settlements.PendulumSettlements,
) *StateEngine {
	return &StateEngine{
		LedgerSystem:          ls,
		electionDb:            electionDb,
		balanceDb:             balanceDb,
		pendulumOracleDb:      pendulumOracleDb,
		pendulumSettlementsDb: pendulumSettlementsDb,
	}
}

// pendulumPoolReserveReader reads each whitelisted pool's HBD-side reserve
// directly from the ledger as the contract account's HBD balance.
//
// Why direct balance and not contract state: every CLP pool already holds
// its HBD reserves in the contract's HBD ledger account ("contract:<id>",
// asset "hbd") — that's where users deposit when adding liquidity and
// where swaps source their output. The only HBD a pool holds that ISN'T
// liquidity is the network's claimable protocol-fee accumulation; that is
// collected regularly via the pool's claim flow, so balance ≈ live
// reserves with bounded drift. Reading the balance avoids forcing every
// pool contract author to publish a state key in lockstep with their swap
// handler.
//
// Determinism: LedgerSystem.GetBalance pins to a snapshot at blockHeight
// from the balance DB plus in-flight unstake/deposit ops. Two nodes
// processing identical block streams produce identical balance snapshots
// at the same block height — the same determinism guarantee the W5
// settlement reader relies on.
type pendulumPoolReserveReader struct {
	se *StateEngine
}

func (r *pendulumPoolReserveReader) ReadPoolHBDReserve(contractID string, blockHeight uint64) (int64, bool) {
	if r == nil || r.se == nil || r.se.LedgerSystem == nil {
		return 0, false
	}
	contractID = strings.TrimSpace(contractID)
	if contractID == "" {
		return 0, false
	}
	// Contract-owned ledger accounts are stored under the "contract:" prefix
	// (see execution-context.go SendBalance / PullBalance).
	bal := r.se.LedgerSystem.GetBalance("contract:"+contractID, blockHeight, "hbd")
	if bal <= 0 {
		return 0, false
	}
	return bal, true
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
