package chain

import (
	"fmt"
	"log/slog"
	"sync"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/chebyrash/promise"
)

type chainRelay interface {
	Init() error
	GetBlock() (*p2p.BlockRelay, error)
}

type chainMap map[string]chainRelay

type ChainOracle struct {
	logger         *slog.Logger
	signedBlockBuf *threadsafe.LockedConsumer[*p2p.OracleBlock]
	newBlockBuf    *threadsafe.LockedConsumer[*p2p.OracleBlock]
	chainMap       chainMap
	conf           common.IdentityConfig
}

var _ aggregate.Plugin = &ChainOracle{}

func New(oracleLogger *slog.Logger, conf common.IdentityConfig) *ChainOracle {
	var (
		logger      = oracleLogger.With("sub-service", "chain-relay")
		blockRelay  = threadsafe.NewLockedConsumer[*p2p.OracleBlock](2)
		signedBlock = threadsafe.NewLockedConsumer[*p2p.OracleBlock](2)
		chainMap    = map[string]chainRelay{
			"BTC": &bitcoinRelayer{},
		}
	)

	return &ChainOracle{
		logger:         logger,
		signedBlockBuf: signedBlock,
		newBlockBuf:    blockRelay,
		chainMap:       chainMap,
		conf:           conf,
	}
}

// Init implements aggregate.Plugin.
func (c *ChainOracle) Init() error {
	// locking states
	c.signedBlockBuf.Lock()
	c.newBlockBuf.Lock()

	// initializes market api's
	for symbol, chainRelayer := range c.chainMap {
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
	return promise.New(func(resolve func(any), _ func(error)) { resolve(nil) })
}

// Stop implements aggregate.Plugin.
func (c *ChainOracle) Stop() error {
	return nil
}

func (c *ChainOracle) FetchBlocks() map[string]p2p.BlockRelay {
	blockMap := threadsafe.NewMap[string, p2p.BlockRelay]()

	wg := &sync.WaitGroup{}
	wg.Add(len(c.chainMap))

	for symbol, chain := range c.chainMap {
		go func(symbol string, chain chainRelay) {
			defer wg.Done()

			block, err := chain.GetBlock()
			if err != nil {
				c.logger.Error(
					"failed to get chain data.",
					"symbol", symbol, "err", err,
				)
				return
			}

			blockMap.Insert(symbol, *block)
		}(symbol, chain)
	}

	wg.Wait()

	return blockMap.Get()
}
