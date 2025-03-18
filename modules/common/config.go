package common

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"vsc-node/modules/config"

	"github.com/vsc-eco/hivego"
)

type identityConfig struct {
	BlsPrivKeySeed string
	HiveActiveKey  string
	HiveUsername   string
}

func (ac *identityConfigStruct) SetUsername(username string) error {
	if username == "" {
		return fmt.Errorf("empty username")
	}
	return ac.Update(func(dc *identityConfig) {
		dc.HiveUsername = username
	})
}

func (ac *identityConfigStruct) HiveActiveKeyPair() (*hivego.KeyPair, error) {
	wif := ac.Get().HiveActiveKey
	return hivego.KeyPairFromWif(wif)
}

type identityConfigStruct struct {
	*config.Config[identityConfig]
}

type IdentityConfig = *identityConfigStruct

func NewIdentityConfig(dataDir ...string) IdentityConfig {
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

	// defaults now for our config if not already provided

	return &identityConfigStruct{config.New(
		identityConfig{
			BlsPrivKeySeed: hex.EncodeToString(seed[:]),
			HiveActiveKey:  "ADD_YOUR_PRIVATE_WIF",
			HiveUsername:   "ADD_YOUR_USERNAME",
		},
		dataDirPtr,
	)}
}
