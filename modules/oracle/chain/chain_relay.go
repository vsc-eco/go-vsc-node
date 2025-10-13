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

	"github.com/chebyrash/promise"
	"github.com/vsc-eco/hivego"
)

var (
	// only usage is to build chain map with proper key-value for ChainOracle.chainMap
	_chains = [...]chainRelay{
		&bitcoinRelayer{},
	}

	_ aggregate.Plugin = &ChainOracle{}

	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

type ChainOracle struct {
	ctx               context.Context
	logger            *slog.Logger
	signatureChannels *signatureChannels
	chainRelayers     map[string]chainRelay
	conf              common.IdentityConfig
}

type chainRelay interface {
	Init(context.Context) error
	// Returns the (lowercase) ticker of the chain (ie, btc for bitcoin).
	Symbol() string
	// Get the deployed contract ID
	ContractID() string
	// Checks for (optional) latest chain state.
	GetLatestValidHeight() (chainState, error)
	// Get the lastest state on contract
	GetContractState() (chainState, error)
	// Fetch chaindata
	ChainData(startBlockHeight uint64, count uint64) ([]chainBlock, error)
}

type chainBlock interface {
	Type() string //symbol of block network
	Serialize() (string, error)
	BlockHeight() uint64
}

type chainState struct {
	blockHeight uint64
}

type chainSession struct {
	sessionID         string
	symbol            string
	contractId        string
	chainData         []chainBlock
	newBlocksToSubmit bool
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
) *ChainOracle {
	logger := oracleLogger.With("sub-service", "chain-relay")

	chainRelayers := make(map[string]chainRelay)
	for _, c := range _chains {
		chainRelayers[strings.ToLower(c.Symbol())] = c
	}

	return &ChainOracle{
		ctx:               ctx,
		logger:            logger,
		signatureChannels: makeSignatureChannels(),
		chainRelayers:     chainRelayers,
		conf:              conf,
	}
}

// Init implements aggregate.Plugin.
func (c *ChainOracle) Init() error {
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

	startSymbols := make(map[string]string)
	for symbol, chainRelayer := range c.chainRelayers {

		fcl, err := chainRelayer.GetLatestValidHeight()

		fmt.Println("fcl", fcl, symbol)
		if err != nil {
			startSymbols[symbol] = err.Error()
		} else {
			startSymbols[symbol] = strconv.Itoa(int(fcl.blockHeight))
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
