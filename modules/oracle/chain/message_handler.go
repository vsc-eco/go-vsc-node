package chain

import (
	"errors"
	"vsc-node/modules/oracle/httputils"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Handle implements p2p.MessageHandler.
func (c *ChainOracle) Handle(peerID peer.ID, msg p2p.Msg) (p2p.Msg, error) {
	var response p2p.Msg = nil

	switch msg.Code {
	case p2p.MsgChainRelay:
		block, err := httputils.JsonUnmarshal[p2p.OracleBlock](msg.Data)
		if err != nil {
			return nil, err
		}

		if c.signatureChannels["something"] != nil {
			ended := <-c.signatureChannels["s"]
			//do something
		}
		if err := c.newBlockBuf.Consume(block); err != nil {
			if errors.Is(err, threadsafe.ErrLockedChannel) {
				c.logger.Debug(
					"unable to collect and verify chain relay block in the current block interval.",
				)
			} else {
				c.logger.Error("failed to collect price block", "err", err)
			}
		}

	default:
		return nil, p2p.ErrInvalidMessageType
	}

	return response, nil
}
