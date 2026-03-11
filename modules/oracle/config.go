package oracle

import "vsc-node/modules/config"

type oracleConfig struct {
	BitcoindRpcHost string `json:"BitcoindRpcHost"`
	BitcoindRpcUser string `json:"BitcoindRpcUser"`
	BitcoindRpcPass string `json:"BitcoindRpcPass"`
}

type oracleConfigStruct struct {
	*config.Config[oracleConfig]
}

type OracleConfig = *oracleConfigStruct

func NewOracleConfig(dataDir ...string) OracleConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &oracleConfigStruct{config.New(oracleConfig{
		BitcoindRpcHost: "bitcoind:48332",
		BitcoindRpcUser: "vsc-node-user",
		BitcoindRpcPass: "vsc-node-pass",
	}, dataDirPtr)}
}
