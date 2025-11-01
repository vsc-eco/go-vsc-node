package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/chain/api"

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

var (
	// only usage is to build chain map with proper key-value for ChainOracle.chainMap
	_chains = [...]api.ChainRelay{
		&api.Bitcoin{},
		&api.Ethereum{},
	}

	_ aggregate.Plugin = &ChainOracle{}

	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

type ChainOracle struct {
	ctx               context.Context
	logger            *slog.Logger
	signatureChannels *signatureChannels
	chainRelayers     map[string]api.ChainRelay
	conf              common.IdentityConfig
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
) *ChainOracle {

	chainRelayers := make(map[string]api.ChainRelay)
	for _, c := range _chains {
		chainRelayers[strings.ToLower(c.Symbol())] = c
	}

	return &ChainOracle{
		ctx:               ctx,
		logger:            oracleLogger,
		signatureChannels: makeSignatureChannels(),
		chainRelayers:     chainRelayers,
		conf:              conf,
	}
}

// Init implements aggregate.Plugin.
func (c *ChainOracle) Init() error {
	c.logger = c.logger.With("service", "chain-oracle")
	// initializes market api's
	for symbol, chainRelayer := range c.chainRelayers {

		c.logger.Debug("initializing chain relay: " + symbol)
		if err := chainRelayer.Init(c.ctx); err != nil {
			return fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				symbol, err,
			)
		}
	}

	return nil
}

// Start implements aggregate.Plugin.
func (c *ChainOracle) Start() *promise.Promise[any] {
	c.logger = c.logger.With("sub-service", "chain-relay")

	startSymbols := make(map[string]string)
	for symbol, chainRelayer := range c.chainRelayers {

		fcl, err := chainRelayer.GetLatestValidHeight()

		fmt.Println("fcl", fcl, symbol)
		if err != nil {
			startSymbols[symbol] = err.Error()
		} else {
			startSymbols[symbol] = strconv.Itoa(int(fcl.BlockHeight))
		}
	}
	hiveClient := hivego.NewHiveRpc("https://api.hive.blog")

	jsonBytes, _ := json.Marshal(startSymbols)
	wif := c.conf.Get().HiveActiveKey
	hiveClient.BroadcastJson(
		[]string{c.conf.Get().HiveUsername},
		[]string{},
		"dev_vsc.chain_oracle",
		string(jsonBytes),
		&wif,
	)
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}
