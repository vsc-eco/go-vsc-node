package witnesses

import (
	"context"
	a "vsc-node/modules/aggregate"

	"go.mongodb.org/mongo-driver/bson"
)

type Witnesses interface {
	a.Plugin
	StoreNodeAnnouncement(nodeId string) error
	SetWitnessUpdate(accountInfo SetWitnessUpdateType) error
	GetLastestWitnesses(options ...SearchOption) ([]Witness, error)
	GetWitnessesAtBlockHeight(bh uint64, options ...SearchOption) ([]Witness, error)
	GetWitnessesByPeerId(peerIds []string, options ...SearchOption) ([]Witness, error)
	GetWitnessAtHeight(account string, bh *uint64) (*Witness, error)
	// PruneOlderThan deletes witness records with height < cutoff. Caller must
	// pass cutoff <= currentHeight - WITNESS_EXPIRE_BLOCKS so records still
	// inside the GetWitnessesAtBlockHeight window are not removed.
	PruneOlderThan(ctx context.Context, cutoff uint64) (int64, error)
}

type SearchConfig struct {
	ExpirationBlocks uint64
	BlockHeight      uint64
	Enabled          bool
}

type SearchOption func(cfg *bson.M) error

// Only returns witnesses that are enabled
func EnabledOnly() SearchOption {
	return func(cfg *bson.M) error {
		(*cfg)["enabled"] = true
		return nil
	}
}
