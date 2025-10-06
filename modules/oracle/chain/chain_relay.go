package chain

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"

	"github.com/chebyrash/promise"
)

type chainRelay interface {
	Init() error
	Symbol() string
	// checks for (optional) latest  chain state
	TickCheck() (*chainState, error)
	// fetches chaindata and serializes to raw bytes
	ChainData() ([]byte, error)
	// deserializes raw bytes and verify chain data
	VerifyChainData([]byte) error
}

type chainState struct {
	blockHeight uint64
}

type chainMap map[string]chainRelay

type ChainOracle struct {
	ctx    context.Context
	logger *slog.Logger
	//sign-btc-900000
	//sign-ltc-81239
	//sign-doge-12309245
	signatureChannels *signatureChannels
	chainRelayers     []chainRelay
	conf              common.IdentityConfig
}

var _ aggregate.Plugin = &ChainOracle{}

func New(
	ctx context.Context,
	oracleLogger *slog.Logger,
	conf common.IdentityConfig,
) *ChainOracle {
	var (
		logger = oracleLogger.With("sub-service", "chain-relay")

		chainRelayers = []chainRelay{
			&bitcoinRelayer{},
		}
	)

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

	sessionID := fmt.Sprintf(
		"%s-%d",
		strings.ToLower(chain.Symbol()),
		latestChainState.blockHeight,
	)

	chainSession := &chainSession{
		sessionID: sessionID,
		chainData: chainData,
	}

	return chainSession, nil
}
