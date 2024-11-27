package announcements

import (
	"crypto/ed25519"
	"vsc-node/lib/dids"
	"vsc-node/modules/config"
)

type announcementsConfig struct {
	BlsPrivKey   dids.BlsPrivKey
	ConsensusKey ed25519.PrivateKey
}

func NewAnnouncementsConfig() *config.Config[announcementsConfig] {
	return config.New(announcementsConfig{
		// default values are "empty" keys
		BlsPrivKey:   dids.BlsPrivKey{},
		ConsensusKey: ed25519.PrivateKey{},
	})
}
