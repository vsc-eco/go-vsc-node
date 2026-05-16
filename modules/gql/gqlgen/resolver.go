package gqlgen

import (
	"encoding/json"

	"vsc-node/lib/datalayer"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/incentive-pendulum/settlement"
	"vsc-node/modules/oracle/chain"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	"github.com/ipfs/go-cid"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

type Resolver struct {
	Witnesses      witnesses.Witnesses
	TxPool         *transactionpool.TransactionPool
	Balances       ledgerDb.Balances
	Ledger         ledgerDb.Ledger
	Actions        ledgerDb.BridgeActions
	InterestClaims ledgerDb.InterestClaims
	Elections      elections.Elections
	Transactions   transactions.Transactions
	Nonces         nonces.Nonces
	Rc             rcDb.RcDb
	HiveBlocks     hive_blocks.HiveBlocks
	StateEngine    *stateEngine.StateEngine
	Da             *datalayer.DataLayer
	Contracts      contracts.Contracts
	ContractsState contracts.ContractState
	TssKeys        tss_db.TssKeys
	TssCommitments tss_db.TssCommitments
	TssRequests    tss_db.TssRequests
	ChainOracle    *chain.ChainOracle
}

// electionSettlement re-hydrates the closing committee's pendulum settlement
// record from an election's `data` CID. The full SettlementRecord lives only
// in the ElectionData CBOR payload — the proposer embeds it before computing
// the CID, but elections.StoreElection deliberately drops it from the Mongo
// record (ElectionResultRecord has no Settlement field), so GetElection /
// ElectionByBlockHeight return it nil. This reads it back from the DataLayer
// the same way the state engine does when it applies the settlement.
//
// Best-effort: returns nil on any failure (empty/invalid CID, object not in
// the DataLayer, no settlement embedded, decode error) so an election query
// never fails just because the settlement leg can't be fetched. A nil result
// is also the correct answer for elections that carry no settlement (genesis,
// or any epoch where the proposer skipped composition).
func (r *Resolver) electionSettlement(dataCid string) *settlement.SettlementRecord {
	if dataCid == "" || r.Da == nil {
		return nil
	}
	parsed, err := cid.Parse(dataCid)
	if err != nil {
		return nil
	}
	node, err := r.Da.GetDag(parsed)
	if err != nil {
		return nil
	}
	jsonBytes, err := node.MarshalJSON()
	if err != nil {
		return nil
	}
	var body struct {
		Settlement *settlement.SettlementRecord `json:"settlement"`
	}
	if err := json.Unmarshal(jsonBytes, &body); err != nil {
		return nil
	}
	return body.Settlement
}
