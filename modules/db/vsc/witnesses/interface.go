package witnesses

import (
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
}

type SearchConfig struct {
	ExpirationBlocks uint64
	BlockHeight      uint64
	Enabled          bool
}

type SearchOption func(cfg *bson.M) error

// func SearchExpiration(blocks uint64) SearchOption {
// 	return func(cfg *bson.M) error {
// 		(*cfg)["expiration_blocks"] = blocks
// 		return nil
// 	}
// }

// func SearchHeight(bh uint64) SearchOption {
// 	return func(cfg *bson.M) error {
// 		(*cfg)["block_height"] = bh
// 		return nil
// 	}
// }

// Only returns witnesses that are enabled
func EnabledOnly() SearchOption {
	return func(cfg *bson.M) error {
		(*cfg)["enabled"] = true
		return nil
	}
}
