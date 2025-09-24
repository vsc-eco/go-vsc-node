package chainrelay

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/libp2p/go-libp2p/core/peer"
)

type chainRelay interface {
	Init() error
	GetBlock() (*p2p.BlockRelay, error)
}

type chainMap map[string]chainRelay

type ChainRelayer struct {
	Logger       *slog.Logger
	SignBlockBuf *threadsafe.LockedConsumer[*p2p.OracleBlock]
	NewBlockBuf  *threadsafe.LockedConsumer[*p2p.OracleBlock]

	chainMap chainMap
}

func New(oLogger *slog.Logger) (*ChainRelayer, error) {
	var (
		logger      = oLogger.With("sub-service", "chain-relay")
		blockRelay  = threadsafe.NewLockedConsumer[*p2p.OracleBlock](2)
		signedBlock = threadsafe.NewLockedConsumer[*p2p.OracleBlock](2)
		chainMap    = map[string]chainRelay{
			"BTC": &bitcoinRelayer{},
		}
	)

	c := &ChainRelayer{
		Logger:       logger,
		SignBlockBuf: signedBlock,
		NewBlockBuf:  blockRelay,
		chainMap:     chainMap,
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

// Handle implements p2p.MessageHandler.
func (c *ChainRelayer) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	var response p2p.Msg = nil

	switch msg.Code {
	case p2p.MsgChainRelayBlock:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		if err := c.NewBlockBuf.Consume(block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				c.Logger.Debug(
					"unable to collect and verify chain relay block in the current block interval.",
				)
			} else {
				c.Logger.Error("failed to collect price block", "err", err)
			}
		}

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return response, nil
}
