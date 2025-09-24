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
	Logger       *slog.Logger
	SignBlockBuf *threadsafe.Slice[p2p.OracleBlock]
	NewBlockBuf  *threadsafe.Slice[p2p.OracleBlock]

	chainMap chainMap
}

func New(logger *slog.Logger) (*ChainRelayer, error) {
	c := &ChainRelayer{
		Logger:       logger.With("sub-service", "chain-relay"),
		SignBlockBuf: threadsafe.NewSlice[p2p.OracleBlock](2),
		NewBlockBuf:  threadsafe.NewSlice[p2p.OracleBlock](2),
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

	c.SignBlockBuf.Lock()
	c.NewBlockBuf.Lock()

	return c, nil
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
				return
			}

			chainMap.Insert(symbol, *block)
		}(symbol, chain)
	}

	wg.Wait()

	return chainMap.Get()
}
