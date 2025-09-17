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
	Shutdown() error
	Symbol() string
	GetBlock() (*p2p.BlockRelay, error)
}

type ChainRelayer struct {
	Logger *slog.Logger
	chains []chainRelay
}

func New(logger *slog.Logger) (*ChainRelayer, error) {
	chainRelayers := []chainRelay{
		&bitcoinRelayer{},
	}

	c := &ChainRelayer{
		Logger: logger.With("sub-service", "chain-relay"),
		chains: chainRelayers,
	}

	for i := range c.chains {
		if err := c.chains[i].Init(); err != nil {
			return nil, fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				c.chains[i].Symbol(), err,
			)
		}
	}

	return c, nil
}

// Stop implements aggregate.Plugin
// Runs cleanup once the `Aggregate` is finished
func (c *ChainRelayer) Stop() error {
	for i := range c.chains {
		if err := c.chains[i].Shutdown(); err != nil {
			return fmt.Errorf(
				"failed to shutdown chainrelayer %s: %w",
				c.chains[i].Symbol(), err,
			)
		}
	}

	return nil
}

func (c *ChainRelayer) FetchBlocks() map[string]p2p.BlockRelay {
	chainMap := threadsafe.NewMap[string, p2p.BlockRelay]()

	wg := &sync.WaitGroup{}
	wg.Add(len(c.chains))

	for i := range c.chains {
		go func(chain chainRelay) {
			defer wg.Done()
			if err := getChainBlock(chainMap, chain); err != nil {
				c.Logger.Error(
					"failed to get chain",
					"symbol", c.chains[i].Symbol(),
					"err", err,
				)
			}
		}(c.chains[i])
	}

	wg.Wait()

	return chainMap.Get()
}

func getChainBlock(
	buf *threadsafe.Map[string, p2p.BlockRelay],
	chain chainRelay,
) error {
	block, err := chain.GetBlock()
	if err != nil {
		return err
	}

	buf.Insert(chain.Symbol(), *block)
	return nil
}
