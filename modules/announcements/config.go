package announcements

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"vsc-node/modules/config"
)

type announcementsConfig struct {
	BlsPrivKeySeed         string
	AnnouncementPrivateWif string
	Username               string
}

func (ac *AnnouncementsConfig) SetUsername(username string) error {
	if username == "" {
		return fmt.Errorf("empty username")
	}
	return ac.Update(func(dc *announcementsConfig) {
		dc.Username = username
	})
}

type AnnouncementsConfig struct {
	*config.Config[announcementsConfig]
}

func NewAnnouncementsConfig(dataDir ...string) AnnouncementsConfig {
	// gen a random seed for the BLS key
	var seed [32]byte
	_, err := rand.Read(seed[:])
	if err != nil {
		panic(fmt.Errorf("failed to generate random seed: %w", err))
	}

	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	fmt.Println("AnnouncementsConfig", *dataDirPtr)
	// defaults now for our config if not already provided

	return AnnouncementsConfig{config.New(
		announcementsConfig{
			BlsPrivKeySeed:         hex.EncodeToString(seed[:]),
			AnnouncementPrivateWif: "ADD_YOUR_PRIVATE_WIF",
			Username:               "ADD_YOUR_USERNAME",
		},
		dataDirPtr,
	)}
}
