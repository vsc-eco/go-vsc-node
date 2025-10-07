package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"

	"github.com/chebyrash/promise"
)

var (
	// only usage is to build chain map with proper key-value for ChainOracle.chainMap
	_chains = [...]chainRelay{
		&bitcoinRelayer{},
	}

	_ aggregate.Plugin = &ChainOracle{}

	errInvalidChainData = errors.New("invalid chain data")
)

type chainRelay interface {
	Init() error
	// Returns the ticker of the chain (ie, BTC for bitcoin).
	Symbol() string
	// Checks for (optional) latest chain state.
	TickCheck() (*chainState, error)
	// Fetches chaindata and serializes to raw bytes.
	ChainData() (json.RawMessage, error)
	// Deserializes and verifies the received raw bytes of the chain data.
	// Returns errInvalidChainData to reject the received chain data, otherwise
	// it is treated as operation error.
	VerifyChainData(json.RawMessage) error
}

type chainState struct {
	blockHeight uint64
}

type ChainOracle struct {
	ctx    context.Context
	logger *slog.Logger
	//sign-btc-900000
	//sign-ltc-81239
	//sign-doge-12309245
	signatureChannels *signatureChannels
	chainRelayers     map[string]chainRelay
	conf              common.IdentityConfig
}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
) *ChainOracle {
	var (
		logger        = oracleLogger.With("sub-service", "chain-relay")
		chainRelayers = make(map[string]chainRelay)
	)

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
	// locking states

	// initializes market api's
	for symbol, chainRelayer := range c.chainRelayers {
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
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}

func (c *ChainOracle) fetchAllBlocks() []chainSession {
	chainSessionChan := make(chan chainSession, len(c.chainRelayers))
	defer close(chainSessionChan)

	wg := &sync.WaitGroup{}
	wg.Add(len(c.chainRelayers))

	for _, chain := range c.chainRelayers {
		go func(chain chainRelay) {
			defer wg.Done()

			chainSession, err := fetchChain(chain)
			if err != nil {
				c.logger.Error(
					"failed to get chain data.",
					"symbol", chain.Symbol(), "err", err,
				)
				return
			}

			if chainSession != nil {
				chainSessionChan <- *chainSession
			}
		}(chain)
	}

	wg.Wait()

	// collection chainSession
	buf := make([]chainSession, 0, len(c.chainRelayers))
	for session := range chainSessionChan {
		buf = append(buf, session)
	}

	return buf
}

type chainSession struct {
	sessionID string
	chainData []byte
}

// returns nil if no new state exists
func fetchChain(chain chainRelay) (*chainSession, error) {
	latestChainState, err := chain.TickCheck()
	if err != nil {
		return nil, fmt.Errorf("failed to check latest state: %w", err)
	}

	if latestChainState == nil {
		return nil, nil
	}

	chainData, err := chain.ChainData()
	if err != nil {
		return nil, fmt.Errorf("failed to get chain data: %w", err)
	}

	chainSession := &chainSession{
		sessionID: makeChainSession(chain, latestChainState),
		chainData: chainData,
	}

	return chainSession, nil
}
