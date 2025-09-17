package chainrelay

import (
	"fmt"
	"log/slog"
	"sync"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/chebyrash/promise"
)

type chainRelay interface {
	Init() error
	Shutdown() error
	Symbol() string
	GetBlock() (*p2p.BlockRelay, error)
}

type ChainRelayer struct {
	chains []chainRelay
}

var _ aggregate.Plugin = &ChainRelayer{}

func New() *ChainRelayer {
	chainRelayers := []chainRelay{
		&bitcoinRelayer{},
	}
	return &ChainRelayer{chainRelayers}
}

// Init implements aggregate.Plugin
func (c *ChainRelayer) Init() error {
	for i := range c.chains {
		if err := c.chains[i].Init(); err != nil {
			return fmt.Errorf(
				"failed to initialize chainrelayer %s: %w",
				c.chains[i].Symbol(), err,
			)
		}
	}

	return nil
}

// Start implements aggregate.Plugin
// Runs startup and should be non blocking
func (c *ChainRelayer) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
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
		go getChainBlock(wg, chainMap, c.chains[i])
	}

	wg.Wait()

	return chainMap.Get()
}

func getChainBlock(
	wg *sync.WaitGroup,
	buf *threadsafe.Map[string, p2p.BlockRelay],
	chain chainRelay,
) {
	defer wg.Done()

	block, err := chain.GetBlock()
	if err != nil {
		slog.Error("failed to get block", "chain", chain.Symbol(), "err", err)
		return
	}

	buf.Insert(chain.Symbol(), *block)
}
