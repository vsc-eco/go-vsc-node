package announcements

import (
	"crypto/rand"
	"fmt"
	"vsc-node/modules/config"
)

type announcementsConfig struct {
	BlsPrivKeySeed         string
	AnnouncementPrivateWif string
	Username               string
}

func NewAnnouncementsConfig() *config.Config[announcementsConfig] {
	// gen a random seed for the BLS key
	var seed [32]byte
	_, err := rand.Read(seed[:])
	if err != nil {
		panic(fmt.Errorf("failed to generate random seed: %w", err))
	}

	// defaults now for our config if not already provided
	return config.New(announcementsConfig{
		BlsPrivKeySeed:         string(seed[:]),
		AnnouncementPrivateWif: "ADD_YOUR_PRIVATE_WIF",
		Username:               "ADD_YOUR_USERNAME",
	})
}
