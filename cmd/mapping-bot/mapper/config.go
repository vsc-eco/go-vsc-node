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
	ContractId string
	// ConnectedGraphQLAddrs is the ordered list of VSC node GraphQL endpoints.
	// The first entry is tried first (typically a local/docker-internal node);
	// subsequent entries are fallbacks used when the primary is unreachable.
	ConnectedGraphQLAddrs []string
	HttpPort              uint16
	// SignApiKey authenticates requests to the /sign and /retry endpoints.
	// If empty, both endpoints are disabled for safety.
	SignApiKey string
	// RcLimit is the resource-credit limit attached to every VSC L2 transaction
	// the bot submits. The VSC node rejects the transaction if the caller's RC
	// balance is below this value, so set it high enough for your contract calls.
	RcLimit uint
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
		ContractId:            "ADD_MAPPING_CONTRACT_ID",
		ConnectedGraphQLAddrs: []string{"http://0.0.0.0:8080/api/v1/graphql"},
		HttpPort:              8000,
		RcLimit:               10000,
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

func (c *mappingBotConfigStruct) RcLimit() uint {
	if v := c.Get().RcLimit; v > 0 {
		return v
	}
	return 10000
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
