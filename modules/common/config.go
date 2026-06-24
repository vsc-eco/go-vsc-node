package common

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"vsc-node/lib/dids"
	"vsc-node/modules/config"

	"github.com/libp2p/go-libp2p/core/crypto"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
)

type identityConfig struct {
	BlsPrivKeySeed string
	HiveActiveKey  string
	HiveUsername   string
	Libp2pPrivKey  string
}

func (ac *identityConfigStruct) SetUsername(username string) error {
	if username == "" {
		return fmt.Errorf("empty username")
	}
	return ac.Update(func(dc *identityConfig) {
		dc.HiveUsername = username
	})
}

func (ac *identityConfigStruct) SetActiveKey(wif string) error {
	if wif == "" {
		return fmt.Errorf("empty wif")
	}
	return ac.Update(func(dc *identityConfig) {
		dc.HiveActiveKey = wif
	})
}

// SetBlsPrivKeySeed overrides the BLS private-key seed (32-byte hex string).
// This exists ONLY for the devnet integration harness: cmd/devnet-setup uses it,
// gated behind DEVNET_DETERMINISTIC_BLS=1, to derive a DETERMINISTIC per-witness
// seed so a test can re-derive the matching gateway multisig keypair WITHOUT ever
// reading identityConfig.json. Production code paths never call this (random seed
// from NewIdentityConfig stands). Mirrors SetActiveKey/SetUsername: same Update
// pattern, same 0600 file write.
func (ac *identityConfigStruct) SetBlsPrivKeySeed(seedHex string) error {
	if len(seedHex) != 64 {
		return fmt.Errorf("bls priv seed hex must be 64 chars (32 bytes), got %d", len(seedHex))
	}
	if _, err := hex.DecodeString(seedHex); err != nil {
		return fmt.Errorf("bls priv seed hex invalid: %w", err)
	}
	return ac.Update(func(dc *identityConfig) {
		dc.BlsPrivKeySeed = seedHex
	})
}

// HasPrivateKey returns true when the Hive active key has been configured
// (not empty and not the default sentinel placeholder).
func (ac *identityConfigStruct) HasPrivateKey() bool {
	key := ac.Get().HiveActiveKey
	return key != "" && key != "ADD_YOUR_PRIVATE_WIF"
}

func (ac *identityConfigStruct) HiveActiveKeyPair() (*hivego.KeyPair, error) {
	wif := ac.Get().HiveActiveKey
	return hivego.KeyPairFromWif(wif)
}

func (ac *identityConfigStruct) Libp2pPrivateKey() (crypto.PrivKey, error) {
	wif := ac.Get().Libp2pPrivKey
	b, err := hex.DecodeString(wif)
	if err != nil {
		return nil, err
	}
	return crypto.UnmarshalEd25519PrivateKey(b)
}

func (ac *identityConfigStruct) blsPrivateKey() (*dids.BlsPrivKey, error) {
	blsPrivKey := &dids.BlsPrivKey{}
	var arr [32]byte
	blsPrivSeedHex := ac.Get().BlsPrivKeySeed
	blsPrivSeed, err := hex.DecodeString(blsPrivSeedHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode bls priv seed: %w", err)
	}
	if len(blsPrivSeed) != 32 {
		return nil, fmt.Errorf("bls priv seed must be 32 bytes")
	}

	copy(arr[:], blsPrivSeed)
	if err = blsPrivKey.Deserialize(&arr); err != nil {
		return nil, fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}

	return blsPrivKey, nil
}

func (ac *identityConfigStruct) BlsProvider() (dids.BlsProvider, error) {
	blsPrivKey, err := ac.blsPrivateKey()
	if err != nil {
		return nil, err
	}

	provider, err := dids.NewBlsProvider(blsPrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create bls provider: %w", err)
	}

	return provider, nil
}

func (ac *identityConfigStruct) BlsDID() (dids.BlsDID, error) {
	blsPrivKey, err := ac.blsPrivateKey()
	if err != nil {
		return "", err
	}

	pubKey, err := blsu.SkToPk(blsPrivKey)
	if err != nil {
		return "", fmt.Errorf("failed to get bls pub key: %w", err)
	}

	// gens the BlsDID from the pub key
	blsDid, err := dids.NewBlsDID(pubKey)
	if err != nil {
		return "", fmt.Errorf("failed to create bls did: %w", err)
	}

	return blsDid, nil
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

	// crypto.UnmarshalEd25519PrivateKey()
	privKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		panic(err)
	}

	privKeyBytes, err := privKey.Raw()
	if err != nil {
		panic(err)
	}

	// defaults now for our config if not already provided

	return &identityConfigStruct{config.New(
		identityConfig{
			BlsPrivKeySeed: hex.EncodeToString(seed[:]),
			HiveActiveKey:  "ADD_YOUR_PRIVATE_WIF",
			HiveUsername:   "ADD_YOUR_USERNAME",
			Libp2pPrivKey:  hex.EncodeToString(privKeyBytes),
		},
		dataDirPtr,
	)}
}
