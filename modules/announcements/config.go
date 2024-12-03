package announcements

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"vsc-node/lib/dids"
	"vsc-node/modules/config"

	blst "github.com/supranational/blst/bindings/go"
)

type announcementsConfig struct {
	BlsPrivKey   dids.BlsPrivKey
	ConsensusKey ed25519.PrivateKey
}

func NewAnnouncementsConfig() *config.Config[announcementsConfig] {
	// gen a random seed
	var seed [32]byte
	_, err := rand.Read(seed[:])
	if err != nil {
		panic(fmt.Errorf("failed to generate random seed: %w", err))
	}

	// gen blst.SecretKey using the seed
	blsPrivKey := blst.KeyGen(seed[:])

	// gen Ed25519 key pair using rand reader
	_, consensusKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(fmt.Errorf("failed to generate Ed25519 key: %w", err))
	}

	// defaults now for our config if not already provided
	return config.New(announcementsConfig{
		BlsPrivKey:   *blsPrivKey,
		ConsensusKey: consensusKey,
	})
}
