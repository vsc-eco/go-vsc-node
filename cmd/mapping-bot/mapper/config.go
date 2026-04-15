package mapper

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"vsc-node/lib/dids"
	"vsc-node/modules/config"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

type mappingBotConfig struct {
	ContractId           string
	ConnectedGraphQLAddr string
	HttpPort             uint16
	// SignApiKey authenticates requests to the /sign endpoint.
	// If empty, /sign is disabled for safety.
	SignApiKey string
	// BotEthPrivKey is a hex-encoded secp256k1 private key used to sign
	// VSC L2 transactions (did:pkh:eip155 caller). It is auto-generated on
	// first run; the derived DID must be funded with HBD to pay for RCs.
	BotEthPrivKey string
}

type mappingBotConfigStruct struct {
	*config.Config[mappingBotConfig]
}

type MappingBotConfig = *mappingBotConfigStruct

func NewMappingBotConfig(dataDir ...string) *mappingBotConfigStruct {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}
	return &mappingBotConfigStruct{config.New(mappingBotConfig{
		ContractId:           "ADD_MAPPING_CONTRACT_ID",
		ConnectedGraphQLAddr: "0.0.0.0:8080",
		HttpPort:             8000,
	}, dataDirPtr)}
}

func (c *mappingBotConfigStruct) ContractId() string {
	return c.Get().ContractId
}

func (c *mappingBotConfigStruct) HttpPort() uint16 {
	return c.Get().HttpPort
}

func (c *mappingBotConfigStruct) SignApiKey() string {
	return c.Get().SignApiKey
}

func (c *mappingBotConfigStruct) SetHttpPort(port uint16) {
	c.Update(func(cfg *mappingBotConfig) {
		cfg.HttpPort = port
	})
}

// BotEthKey returns the secp256k1 private key used for L2 transaction signing.
// If the config has no key, a fresh one is generated, persisted, and returned —
// so the caller still needs to fund the derived DID before L2 submissions work.
func (c *mappingBotConfigStruct) BotEthKey() (*ecdsa.PrivateKey, bool, error) {
	cfg := c.Get()
	if cfg.BotEthPrivKey != "" {
		priv, err := ethCrypto.HexToECDSA(cfg.BotEthPrivKey)
		if err != nil {
			return nil, false, fmt.Errorf("decode BotEthPrivKey: %w", err)
		}
		return priv, false, nil
	}
	priv, err := ethCrypto.GenerateKey()
	if err != nil {
		return nil, false, fmt.Errorf("generate bot eth key: %w", err)
	}
	encoded := hex.EncodeToString(ethCrypto.FromECDSA(priv))
	if err := c.Update(func(dc *mappingBotConfig) {
		dc.BotEthPrivKey = encoded
	}); err != nil {
		return nil, false, fmt.Errorf("persist bot eth key: %w", err)
	}
	return priv, true, nil
}

// BotEthDID returns the did:pkh:eip155 DID derived from the bot's eth key.
func (c *mappingBotConfigStruct) BotEthDID(priv *ecdsa.PrivateKey) dids.EthDID {
	addr := ethCrypto.PubkeyToAddress(priv.PublicKey).Hex()
	return dids.NewEthDID(addr)
}
