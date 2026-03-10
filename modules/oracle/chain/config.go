package chain

import (
	"os"
	"vsc-node/modules/config"
)

type chainRpcEntry struct {
	Host string
	User string
	Pass string
}

type chainConfig struct {
	Bitcoin  chainRpcEntry
	Litecoin chainRpcEntry
}

type ChainConfig struct {
	*config.Config[chainConfig]
}

func NewChainConfig() ChainConfig {
	return ChainConfig{config.New(chainConfig{
		Bitcoin: chainRpcEntry{
			Host: "btcd:8332",
			User: "vsc-node-user",
			Pass: "vsc-node-pass",
		},
		Litecoin: chainRpcEntry{
			Host: "litecoind:9332",
			User: "vsc-node-user",
			Pass: "vsc-node-pass",
		},
	}, nil)}
}

func (cc ChainConfig) Init() error {
	if err := cc.Config.Init(); err != nil {
		return err
	}

	updated := false

	btcHost := os.Getenv("BTC_RPC_HOST")
	btcUser := os.Getenv("BTC_RPC_USER")
	btcPass := os.Getenv("BTC_RPC_PASS")
	ltcHost := os.Getenv("LTC_RPC_HOST")
	ltcUser := os.Getenv("LTC_RPC_USER")
	ltcPass := os.Getenv("LTC_RPC_PASS")

	if btcHost != "" || btcUser != "" || btcPass != "" ||
		ltcHost != "" || ltcUser != "" || ltcPass != "" {
		updated = true
	}

	if updated {
		return cc.Update(func(c *chainConfig) {
			if btcHost != "" {
				c.Bitcoin.Host = btcHost
			}
			if btcUser != "" {
				c.Bitcoin.User = btcUser
			}
			if btcPass != "" {
				c.Bitcoin.Pass = btcPass
			}
			if ltcHost != "" {
				c.Litecoin.Host = ltcHost
			}
			if ltcUser != "" {
				c.Litecoin.User = ltcUser
			}
			if ltcPass != "" {
				c.Litecoin.Pass = ltcPass
			}
		})
	}

	return nil
}
