package contracts

import wasm_runtime "vsc-node/modules/wasm/runtime"

type Contract struct {
	Id             string               `bson:"id,omitempty"`
	Code           string               `bson:"code"`
	Name           string               `bson:"name"`
	Description    string               `bson:"description"`
	Creator        string               `bson:"creator"`
	Owner          string               `bson:"owner"`
	TxId           string               `bson:"tx_id"`
	CreationHeight uint64               `bson:"creation_height"`
	CreationTs     *string              `bson:"creation_ts,omitempty"`
	Runtime        wasm_runtime.Runtime `bson:"runtime"`
	Latest         bool                 `bson:"latest,omitempty"`

	// ActivationHeight is the Hive L1 height at/after which this version becomes
	// the active code. For immediate writes (contract creation, metadata updates
	// on networks with no timelock) it equals CreationHeight. For a timelocked
	// update it is CreationHeight + the network timelock. ContractById returns the
	// newest version whose ActivationHeight <= the query height, so queued code
	// does not run until it matures. RegisterContract guarantees it is never less
	// than CreationHeight.
	ActivationHeight uint64 `bson:"activation_height"`
	// Proposer is the account that submitted this version (creator on creation,
	// the signing owner on an update). The "by who" of a pending update.
	Proposer string `bson:"proposer,omitempty"`

	// Cancellation (audit-preserved): a vsc.cancel_contract_update marks a still-
	// pending version cancelled instead of deleting it. Cancelled versions are
	// ignored by ContractById/FindActiveContracts/FindPendingUpdates.
	Cancelled       bool   `bson:"cancelled,omitempty"`
	CancelledHeight uint64 `bson:"cancelled_height,omitempty"`
	CancelledTx     string `bson:"cancelled_tx,omitempty"`
}

type Intent struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args"`
}
