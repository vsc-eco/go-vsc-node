package oracle

import "vsc-node/modules/config"

// ChainRpcConfig holds RPC connection details for a single chain node.
type ChainRpcConfig struct {
	RpcHost string `json:"RpcHost"`
	RpcUser string `json:"RpcUser"`
	RpcPass string `json:"RpcPass"`
}

// oracleConfig is persisted as JSON in the node's data directory.
// To add a new chain, add an entry to the Chains map — no code changes needed.
//
// Example config JSON:
//
//	{
//	  "Chains": {
//	    "BTC":  { "RpcHost": "bitcoind:48332",  "RpcUser": "user", "RpcPass": "pass" },
//	    "DASH": { "RpcHost": "dashd:9998",      "RpcUser": "user", "RpcPass": "pass" },
//	    "LTC":  { "RpcHost": "litecoind:9332",  "RpcUser": "user", "RpcPass": "pass" },
//	    "ETH":  { "RpcHost": "http://geth:8545" }
//	  }
//	}
type oracleConfig struct {
	// Chains maps chain symbols (e.g. "BTC", "DASH") to their RPC config.
	// Each registered chainRelay looks up its RPC details from this map.
	Chains map[string]ChainRpcConfig `json:"Chains"`

	// Deprecated: use Chains["BTC"] instead. Kept for backwards compatibility
	// with existing config files that use the flat BitcoindRpc* fields.
	BitcoindRpcHost string `json:"BitcoindRpcHost,omitempty"`
	BitcoindRpcUser string `json:"BitcoindRpcUser,omitempty"`
	BitcoindRpcPass string `json:"BitcoindRpcPass,omitempty"`
}

// ChainRpc returns the RPC config for a given chain symbol.
// Falls back to the legacy Bitcoind fields for BTC.
func (c oracleConfig) ChainRpc(symbol string) (ChainRpcConfig, bool) {
	if c.Chains != nil {
		if rpc, ok := c.Chains[symbol]; ok {
			return rpc, true
		}
	}
	if symbol == "BTC" && c.BitcoindRpcHost != "" {
		return ChainRpcConfig{
			RpcHost: c.BitcoindRpcHost,
			RpcUser: c.BitcoindRpcUser,
			RpcPass: c.BitcoindRpcPass,
		}, true
	}
	return ChainRpcConfig{}, false
}

type oracleConfigStruct struct {
	*config.Config[oracleConfig]
}

type OracleConfig = *oracleConfigStruct

// NewOracleConfig creates a new oracle config with sensible defaults.
// The defaults only include BTC; additional chains are added by the
// node operator in their config JSON file.
func NewOracleConfig(dataDir ...string) OracleConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &oracleConfigStruct{config.New(oracleConfig{
		Chains: map[string]ChainRpcConfig{
			"BTC": {
				RpcHost: "bitcoind:48332",
				RpcUser: "vsc-node-user",
				RpcPass: "vsc-node-pass",
			},
			// "DASH": {
			// 	RpcHost: "dashd:9998",
			// 	RpcUser: "vsc-node-user",
			// 	RpcPass: "vsc-node-pass",
			// },
			// "LTC": {
			// 	RpcHost: "litecoind:9332",
			// 	RpcUser: "vsc-node-user",
			// 	RpcPass: "vsc-node-pass",
			// },
			// "ETH": {
			// 	RpcHost: "http://geth:8545",
			// },
		},
	}, dataDirPtr)}
}
