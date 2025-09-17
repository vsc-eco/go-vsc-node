package chainrelay

import (
	"log/slog"
	"sync"
	"vsc-node/modules/oracle/p2p"

	"github.com/chebyrash/promise"
)

type chainRelay interface {
	Init() error
	Stop() error
	Symbol() string
	GetBlock(blockDepth int) (*p2p.BlockRelay, error)
}

type ChainRelayer struct {
	logger        *slog.Logger
	chainRelayers []chainRelay
}

func New(logger *slog.Logger) *ChainRelayer {
	chainRelayers := []chainRelay{
		&bitcoinRelayer{},
	}
	return &ChainRelayer{logger, chainRelayers}
}

func (c *ChainRelayer) Init() error {
	wg := &sync.WaitGroup{}
	wg.Add(len(c.chainRelayers))

	for i := range c.chainRelayers {
		go func(i int) {
			defer wg.Done()
			if err := c.chainRelayers[i].Init(); err != nil {
				c.logger.Error(
					"failed to initialize chainrelayer.",
					"sym", c.chainRelayers[i].Symbol(),
					"err", err,
				)
			}
		}(i)
	}

	wg.Wait()

	return nil
}

// Runs startup and should be non blocking
func (c *ChainRelayer) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(any), _ func(error)) {
		resolve(nil)
	})
}

// Runs cleanup once the `Aggregate` is finished
func (c *ChainRelayer) Stop() error {
	wg := &sync.WaitGroup{}
	wg.Add(len(c.chainRelayers))

	for i := range c.chainRelayers {
		go func(i int) {
			defer wg.Done()
			if err := c.chainRelayers[i].Stop(); err != nil {
				c.logger.Error(
					"failed to stop chainrelayer.",
					"sym", c.chainRelayers[i].Symbol(),
					"err", err,
				)
			}
		}(i)
	}

	wg.Wait()

	return nil
}
