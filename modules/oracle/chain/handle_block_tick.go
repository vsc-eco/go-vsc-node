package chain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/threadsafe"
)

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *ChainOracle) HandleBlockTick(
	signal p2p.BlockTickSignal,
	p2pSpec p2p.OracleVscSpec,
) {
	// NOTE: when the node is the not the producer, it is not a witness. The
	// action of witness is triggered for incoming p2p message.
	if !signal.IsProducer {
		return
	}
	// ## Process end to end ##
	// - Node is witness & producer
	// - Using the contract state of latest block in combination with latest
	//   block on Bitcoin mainnet
	// - Assuming this is true.
	// - That means the latest block on BTC mainnet is atleast 3 confirmations
	//   higher
	// - Create the transaction structure with the block headers to submit
	// - Ask P2P channels for signatures for the transaction
	// - Receiving node will receive request to create transaction on VSC mainnet
	// - Receiving node will do the same check as above, aka verify that new
	//   blocks must be submitted
	// - If true, then sign the exact same transaction and return signature
	// - Producer node will receive signatures through the p2p channel
	// - Producer node will aggregate those signatures into a single BLS circuit
	//   for submission on mainnet

	for _, chainSession := range o.fetchAllBlocks() {
		signatureRequest, err := makeSignatureRequestMessage(
			signatureRequest,
			chainSession.sessionID,
			chainSession.chainData,
		)

		if err != nil {
			fmt.Println(err) // TODO: log this
			continue
		}

		// make a channel to collect signatures

		if err := p2pSpec.Broadcast(p2p.MsgChainRelay, signatureRequest); err != nil {
			fmt.Println(err) // TODO: log this
		}
	}

	var handler chainRelayHandler
	if signal.IsProducer {
		handler = &chainRelayProducer{o, p2pSpec}
	} else {
		handler = &chainRelayWitness{o, p2pSpec}
	}

	if err := handler.handle(signal, chainSessions); err != nil {
		o.logger.Error(
			"failed to process chain relay tick interval",
			"is-producer", signal.IsProducer,
			"is-witness", signal.IsWitness,
		)
	}
}

// chain relay producer

type chainRelayProducer struct {
	*ChainOracle
	p2p.OracleVscSpec
}

// handle implements chainRelayHandler.
func (c *chainRelayProducer) handle(
	sig p2p.BlockTickSignal,
	blockMap map[string]p2p.BlockRelay,
) error {

	// endnlsadflkdsjflksjdlf
	// broadcast block
	oracleBlock, err := p2p.MakeOracleBlock(
		c.conf.Get().HiveUsername,
		c.conf.Get().HiveActiveKey,
		blockMap,
	)
	if err != nil {
		return fmt.Errorf("failed to create oracle block")
	}

	if err := c.Broadcast(p2p.MsgChainRelay, oracleBlock); err != nil {
		return fmt.Errorf("failed to broadcast oracle block: %w", err)
	}

	// collect and verify signatures
	c.logger.Debug("collecting signature", "block-id", oracleBlock.ID)

	sigThreshold := int(math.Ceil(float64(len(sig.ElectedMembers)) * 2.0 / 3.0))
	oracleBlock.Signatures = make([]string, 0, sigThreshold)
	collector := c.signatureCollector(oracleBlock, sigThreshold)

	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = c.signatureBuf.Collect(ctx, collector); err != nil {
		return errors.New("failed to meet signature threshold")
	}

	return c.SubmitToContract(oracleBlock)
}

func (c *chainRelayProducer) signatureCollector(
	oracleBlock *p2p.OracleBlock,
	sigThreshold int,
) threadsafe.CollectFunc[*p2p.OracleBlock] {
	const earlyReturn = false

	return func(ob *p2p.OracleBlock) bool {
		if len(ob.Signatures) != 1 {
			return earlyReturn
		}

		sig := ob.Signatures[0]
		if err := c.ValidateSignature(oracleBlock, sig); err != nil {
			c.logger.Error(
				"failed to validate signature",
				"block-id", oracleBlock.ID,
				"err", err, "signature", sig,
			)
			return earlyReturn
		}

		oracleBlock.Signatures = append(oracleBlock.Signatures, sig)

		return len(oracleBlock.Signatures) >= sigThreshold
	}
}

// chain relay witness

type chainRelayWitness struct {
	*ChainOracle
	p2p.OracleVscSpec
}

// handle implements chainRelayHandler.
func (c *chainRelayWitness) handle(
	sig p2p.BlockTickSignal,
	localBlockMap map[string]p2p.BlockRelay,
) error {
	// room for network latency
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c.newBlockBuf.Collect(ctx, func(ob *p2p.OracleBlock) bool {
		return false
	})

	return nil
}

// Fetches state from smart contract or other source to see if there are any actions required
type ChainStateFetcher interface {
	//blockHash, blockHeight uint64
	LastBlock() (string, uint64)
	//Returns contract ID of the chain relay contract
	ContractId() string
}
