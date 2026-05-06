package witnesses

import (
	a "vsc-node/modules/aggregate"
)

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(nodeId string) error
	SetWitnessUpdate(accountInfo SetWitnessUpdateType) error
	GetLastestWitnesses(options ...SearchOption) ([]Witness, error)
	GetWitnessesAtBlockHeight(bh uint64, options ...SearchOption) ([]Witness, error)
	GetWitnessesByPeerId(peerIds []string, options ...SearchOption) ([]Witness, error)
	GetWitnessAtHeight(account string, bh *uint64) (*Witness, error)
}

type SearchConfig struct {
	ExpirationBlocks uint64
	BlockHeight      uint64
	Enabled          bool
}

// SearchOption filters witnesses post-dedupe. It must be a predicate over the
// latest record per account, not a query-time filter — applying enabled=true
// at the Mongo query level would surface an older enabled=true record for an
// account whose most-recent announcement is enabled=false.
type SearchOption func(w Witness) bool

// Only returns witnesses that are enabled
func EnabledOnly() SearchOption {
	return func(w Witness) bool {
		return w.Enabled
	}
}
