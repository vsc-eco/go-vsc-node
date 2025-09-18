package chainrelay

import (
	"fmt"
	"log/slog"
	"sync"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"
)

type chainRelay interface {
	Init() error
	GetBlock() (*p2p.BlockRelay, error)
}

type chainMap map[string]chainRelay

type ChainRelayer struct {
	Logger   *slog.Logger
	chainMap chainMap
}

func New(logger *slog.Logger) (*ChainRelayer, error) {
	c := &ChainRelayer{
		Logger: logger.With("sub-service", "chain-relay"),
		chainMap: map[string]chainRelay{
			"BTC": &bitcoinRelayer{},
		},
	}

	for symbol, chainRelayer := range c.chainMap {
		if err := chainRelayer.Init(); err != nil {
			return nil, fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				symbol, err,
			)
		}
	}

	return c, nil
}

// Stop implements aggregate.Plugin
// Runs cleanup once the `Aggregate` is finished
func (c *ChainRelayer) Stop() error {
	return nil
}

func (c *ChainRelayer) FetchBlocks() map[string]p2p.BlockRelay {
	chainMap := threadsafe.NewMap[string, p2p.BlockRelay]()

	wg := &sync.WaitGroup{}
	wg.Add(len(c.chainMap))

	for symbol, chain := range c.chainMap {
		go func(symbol string, chain chainRelay) {
			defer wg.Done()

			block, err := chain.GetBlock()
			if err != nil {
				c.Logger.Error(
					"failed to get chain data.",
					"symbol", symbol, "err", err,
				)
			} else {
				chainMap.Insert(symbol, *block)
			}
		}(symbol, chain)
	}

	wg.Wait()

	return chainMap.Get()
}
