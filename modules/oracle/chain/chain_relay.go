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

	errInvalidChainData   = errors.New("invalid chain data")
	errInvalidChainSymbol = errors.New("invalid chain symbol")
)

type chainRelay interface {
	Init() error
	// Returns the ticker of the chain (ie, BTC for bitcoin).
	Symbol() string
	// Checks for (optional) latest chain state.
	GetLatestValidHeight() (chainState, error)
	GetContractState() (chainState, error)
	// Fetches chaindata and serializes to raw bytes.
	ChainData(startBlockHeight uint64, count uint64) ([]chainBlock, error)
	// Deserializes and verifies the received raw bytes of the chain data.
	// VerifyChainData(json.RawMessage) error
}

type chainBlock interface {
	Type() string //symbol of block network
	Serialize() (string, error)
	BlockHeight() uint64
}

type chainState struct {
	blockHeight uint64
}

type ChainOracle struct {
	ctx               context.Context
	logger            *slog.Logger
	signatureChannels *signatureChannels
	chainRelayers     map[string]chainRelay
	conf              common.IdentityConfig
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
) *ChainOracle {
	logger := oracleLogger.With("sub-service", "chain-relay")

	chainRelayers := make(map[string]chainRelay)
	for _, c := range _chains {
		chainRelayers[strings.ToUpper(c.Symbol())] = c
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
		if err := chainRelayer.Init(); err != nil {
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
	hiveClient.BroadcastJson([]string{c.conf.Get().HiveUsername}, []string{}, "dev_vsc.chain_oracle", string(jsonBytes), &wif)
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}

func (c *ChainOracle) fetchAllStatuses() []chainSession {
	chainSessions := make([]chainSession, 0)

	for _, chain := range c.chainRelayers {
		chainSession, err := fetchChainStatus(chain)
		if err != nil {
			c.logger.Error(
				"failed to get chain data.",
				"symbol", chain.Symbol(), "err", err,
			)
			continue
		}

		chainSessions = append(chainSessions, chainSession)
	}

	return chainSessions
}

type chainSession struct {
	symbol            string
	contractId        *string
	chainData         []chainBlock
	newBlocksToSubmit bool
}

func fetchChainStatus(chain chainRelay) (chainSession, error) {
	latestChainState, err := chain.GetLatestValidHeight()
	if err != nil {
		return chainSession{
			newBlocksToSubmit: false,
		}, fmt.Errorf("failed to check latest state: %w", err)
	}
	//

	contractState, err := chain.GetContractState()
	if err != nil {
		return chainSession{
			newBlocksToSubmit: false,
		}, err
	}

	if latestChainState.blockHeight <= contractState.blockHeight {
		return chainSession{}, nil
	}

	chainData, err := chain.ChainData(contractState.blockHeight+1, 100)
	if err != nil {
		return chainSession{}, fmt.Errorf("failed to get chain data: %w", err)
	}

	c := "vsc1BRZLx1"
	chainSession := chainSession{
		symbol:            chain.Symbol(),
		contractId:        &c,
		chainData:         chainData,
		newBlocksToSubmit: true,
	}

	return chainSession, nil
}

func makeChainSessionID(c *chainSession) (string, error) {
	if len(c.chainData) == 0 {
		return "", errors.New("chainData not supplied")
	}

	startBlock := c.chainData[0].BlockHeight()
	endBlock := c.chainData[len(c.chainData)-1].BlockHeight()

	id := fmt.Sprintf("%s-%d-%d", c.symbol, startBlock, endBlock)
	return id, nil
}

// symbol - startBlock - endBlock
func parseChainSessionID(sessionID string) (string, uint64, uint64, error) {
	var (
		chainSymbol string
		startBlock  uint64
		endBlock    uint64
	)

	_, err := fmt.Sscanf(
		sessionID,
		"%s-%d-%d",
		&chainSymbol,
		&startBlock,
		&endBlock,
	)
	if err != nil {
		return "", 0, 0, err
	}

	return chainSymbol, startBlock, endBlock, nil
}
