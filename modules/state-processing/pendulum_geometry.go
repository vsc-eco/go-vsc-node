package state_engine

import (
	"math/big"
	"strings"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_settlements "vsc-node/modules/db/vsc/pendulum_settlements"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/ipfs/go-cid"
)

// PoolStateKeyReader fetches a contract state value at a specific block
// height. Production binds it to the live ContractState/DataLayer pair via
// liveContractStateKeyReader; tests can stub it with an in-memory map to
// avoid standing up an IPFS node.
type PoolStateKeyReader interface {
	ReadStateKey(contractID string, blockHeight uint64, key string) ([]byte, bool)
}

// PendulumPoolReserveReaderForTest exposes the unexported
// pendulumPoolReserveReader so external test packages can drive the
// production read path without standing up the full state engine. The
// caller supplies a PoolStateKeyReader so tests can seed contract state
// directly without booting a DataLayer/IPFS instance.
func (se *StateEngine) PendulumPoolReserveReaderForTest(states PoolStateKeyReader) pendulumoracle.PoolReserveReader {
	return &pendulumPoolReserveReader{states: states}
}

// NewForGeometryTest constructs a minimum-viable StateEngine wired only with
// the dependencies the pendulum geometry / pool-reserve readers consume. Skips
// the full constructor so geometry-only tests don't need a wasm runtime, RC
// system, etc.
func NewForGeometryTest(
	ls ledgerSystem.LedgerSystem,
	electionDb elections.Elections,
	balanceDb ledgerDb.Balances,
	pendulumSettlementsDb pendulum_settlements.PendulumSettlements,
) *StateEngine {
	return &StateEngine{
		LedgerSystem:          ls,
		electionDb:            electionDb,
		balanceDb:             balanceDb,
		pendulumSettlementsDb: pendulumSettlementsDb,
	}
}

// liveContractStateKeyReader is the production PoolStateKeyReader. It pins
// to the contract's most recent ContractOutput at or before blockHeight and
// resolves the requested state key through the DataLayer/DataBin pair —
// the same path the GraphQL `getStateByKeys` resolver uses.
//
// Determinism: GetLastOutput(_, blockHeight) and DataLayer reads are pinned
// to the per-block state-merkle CID. Two honest nodes processing identical
// block streams produce identical reads at the same block height.
type liveContractStateKeyReader struct {
	contractState contracts.ContractState
	da            *DataLayer.DataLayer
}

func (r *liveContractStateKeyReader) ReadStateKey(contractID string, blockHeight uint64, key string) ([]byte, bool) {
	if r == nil || r.contractState == nil || r.da == nil {
		return nil, false
	}
	output, err := r.contractState.GetLastOutput(contractID, blockHeight)
	if err != nil || output.StateMerkle == "" {
		return nil, false
	}
	stateCid, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return nil, false
	}
	databin := DataLayer.NewDataBinFromCid(r.da, stateCid)
	keyCid, err := databin.Get(key)
	if err != nil || keyCid == nil {
		return nil, false
	}
	raw, err := r.da.GetRaw(*keyCid)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// liveGeometryReader implements pendulumwasm.GeometryReader by recomputing
// the (V, P, E, T, sBps) tuple on every swap call from on-chain inputs:
// the FeedTracker's most-recent-tick price + the GeometryComputer wired
// against committee bond and pool state-key reserves.
//
// Per-swap cost: one feed read + one committee-bond sum + one state-key
// read per whitelisted pool. All inputs are deterministic at the swap's
// block height — two honest nodes processing the same block produce
// identical outputs.
//
// `OK == false` on the returned outputs surfaces as ErrSnapshotUnavailable
// to the contract caller, matching the prior pre-warmup gate semantics.
type liveGeometryReader struct {
	computer          *pendulumoracle.GeometryComputer
	feed              *pendulumoracle.FeedTracker
	whitelist         func() []string
	effectiveStakeNum int64
	effectiveStakeDen int64
}

func (r *liveGeometryReader) GeometryAt(blockHeight uint64) (pendulumoracle.GeometryOutputs, bool) {
	if r == nil || r.computer == nil || r.feed == nil {
		return pendulumoracle.GeometryOutputs{}, false
	}
	tick := r.feed.LastTick()
	num, den := r.effectiveStakeNum, r.effectiveStakeDen
	if num <= 0 || den <= 0 {
		num, den = 2, 3
	}
	var pools []string
	if r.whitelist != nil {
		pools = r.whitelist()
	}
	out := r.computer.Compute(pendulumoracle.GeometryInputs{
		BlockHeight:       blockHeight,
		HivePriceHBDBps:   tick.TrustedHivePriceBps,
		HivePriceOK:       tick.TrustedHiveOK,
		WhitelistedPools:  pools,
		EffectiveStakeNum: num,
		EffectiveStakeDen: den,
	})
	return out, out.OK
}

// pendulumPoolReserveReader reads each whitelisted pool's HBD-side reserve
// from the pool contract's own self-declared state, using the ABI the
// dex-contracts repo writes via setStr/setBigInt:
//
//	a0n / a1n  — asset0 / asset1 lowercase name (raw string bytes)
//	r0  / r1   — corresponding reserve as an unsigned big.Int's
//	             big-endian byte representation (the format setBigInt uses,
//	             namely string(val.Bytes())).
//
// The reader matches whichever side has name == "hbd" and decodes the
// matching reserve via new(big.Int).SetBytes(raw). Pools that don't have
// HBD on either side are rejected with (0, false). The geometry computer
// then doubles the result into V = 2P, the standard symmetry assumption
// for a balanced HBD-paired CPMM pool — see geometry.go.
//
// Why state keys instead of the ledger balance: anyone can transfer HBD to
// a contract account (the ledger balance is a public sink), which would
// inflate P, V, and ultimately push s = V/E past the s≥1 cliff that routes
// 100% of the pendulum pot to nodes. The contract's own reserves are only
// updated by addLiquidity / removeLiquidity / swap, so unsolicited deposits
// can't poison the geometry input.
type pendulumPoolReserveReader struct {
	states PoolStateKeyReader
}

func (r *pendulumPoolReserveReader) ReadPoolHBDReserve(contractID string, blockHeight uint64) (int64, bool) {
	if r == nil || r.states == nil {
		return 0, false
	}
	contractID = strings.TrimSpace(contractID)
	if contractID == "" {
		return 0, false
	}

	a0n, _ := r.states.ReadStateKey(contractID, blockHeight, "a0n")
	a1n, _ := r.states.ReadStateKey(contractID, blockHeight, "a1n")

	var reserveKey string
	switch "hbd" {
	case strings.ToLower(string(a0n)):
		reserveKey = "r0"
	case strings.ToLower(string(a1n)):
		reserveKey = "r1"
	default:
		// Pool isn't HBD-paired (or names not yet populated); not a
		// pendulum-relevant pool. Whitelist + ABI checks live elsewhere;
		// this read just refuses to contribute a P_hbd term.
		return 0, false
	}

	raw, ok := r.states.ReadStateKey(contractID, blockHeight, reserveKey)
	if !ok || len(raw) == 0 {
		return 0, false
	}

	// setBigInt stores big.Int.Bytes() — unsigned big-endian. The reserve
	// is always non-negative and fits in int64 for any plausible pool
	// (~9.2e18 base units = ~9.2e15 HBD given the 3-decimal precision on
	// Hive amounts). An overflow here means either a corrupt contract or
	// a future precision change that needs explicit handling.
	n := new(big.Int).SetBytes(raw)
	if !n.IsInt64() {
		return 0, false
	}
	val := n.Int64()
	if val <= 0 {
		return 0, false
	}
	return val, true
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
