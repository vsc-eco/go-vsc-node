package witnesses

import (
	"context"
	a "vsc-node/modules/aggregate"
)

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(ctx context.Context, nodeId string) error
	SetWitnessUpdate(ctx context.Context, accountInfo SetWitnessUpdateType) error
	GetLastestWitnesses(ctx context.Context, options ...SearchOption) ([]Witness, error)
	GetWitnessesAtBlockHeight(ctx context.Context, bh uint64, options ...SearchOption) ([]Witness, error)
	GetWitnessesByPeerId(ctx context.Context, peerIds []string, options ...SearchOption) ([]Witness, error)
	GetWitnessAtHeight(ctx context.Context, account string, bh *uint64) (*Witness, error)
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
