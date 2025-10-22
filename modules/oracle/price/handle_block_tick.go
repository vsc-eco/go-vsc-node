package price

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"

	blocks "github.com/ipfs/go-block-format"
)

const float64Epsilon = 1e-9

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *PriceOracle) HandleBlockTick(
	sig p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	o.logger.Debug("broadcast price block tick.")

	defer o.priceMap.Clear()

	// broadcast local average price
	localAvgPrices := o.priceMap.GetAveragePricePoints()

	msg, err := makePriceOracleMessage(averagePriceCode, localAvgPrices)
	if err != nil {
		o.logger.Error("failed to make message", "err", err)
		return
	}

	if err := p2pSpec.Broadcast(p2p.MsgPriceOracle, msg); err != nil {
		o.logger.Error("failed to broadcast local average price", "err", err)
		return
	}

	if !sig.IsWitness {
		return
	}

	// collect average prices from the network
	ctx, cancel := context.WithTimeout(o.ctx, 15*time.Second)
	defer cancel()

	priceChan := o.priceChannel.Open()

	type CollectedPricePoint struct {
		prices  []float64
		volumes []float64
	}

	collectedPricePoint := make(map[string]CollectedPricePoint)
	err = nil

avgPricePoll:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()

			// if timed out, ignore the error
			if errors.Is(err, context.DeadlineExceeded) {
				err = nil
			}

			break avgPricePoll

		case priceMap := <-priceChan:
			for symbol, pp := range priceMap {
				if math.IsNaN(pp.Volume) || math.IsNaN(pp.Volume) {
					o.logger.Debug("NaN values dropped", "price", pp.Price, "volume", pp.Volume)
					continue
				}

				symbol = strings.ToLower(symbol)

				p, ok := collectedPricePoint[symbol]
				if !ok {
					p = CollectedPricePoint{
						prices:  []float64{pp.Price},
						volumes: []float64{pp.Volume},
					}
				} else {
					p.prices = append(p.prices, pp.Price)
					p.volumes = append(p.volumes, pp.Volume)
				}

				collectedPricePoint[symbol] = p
			}
		}
	}

	o.priceChannel.Close()
	if err != nil {
		o.logger.Error("failed collect average prices from network")
		return
	}

	// get median prices
	medianPriceMap := make(map[string]api.PricePoint, len(collectedPricePoint))
	for symbol, pp := range collectedPricePoint {
		if len(pp.prices) == 0 || len(pp.volumes) == 0 {
			o.logger.Debug("skipping symbol, no prices or volumes supplied", "symbol", symbol)
			continue
		}

		medianPriceMap[symbol] = api.PricePoint{
			Price:  getMedianValue(pp.prices),
			Volume: getMedianValue(pp.volumes),
		}
	}

	// make transaction
	tx, err := makeTx(medianPriceMap)
	if err != nil {
		o.logger.Error("failed to make transaction", "err", err)
	}

	var handler interface {
		handle(blocks.Block, map[string]api.PricePoint) error
	}

	if sig.IsProducer {
		handler = &Producer{
			OracleP2PSpec:   p2pSpec,
			electionMembers: sig.ElectedMembers,
		}
	} else {
		handler = &Witness{p2pSpec}
	}

	if err := handler.handle(tx, medianPriceMap); err != nil {
		o.logger.Error(
			"failed to handle block tick",
			"is-producer", sig.IsProducer,
			"is-witness", sig.IsWitness,
			"err", err,
		)
	}
}

// block producer
type Producer struct {
	p2p.OracleP2PSpec
	electionMembers []elections.ElectionMember
}

func (p *Producer) handle(tx blocks.Block, medianPriceMap map[string]api.PricePoint) error {
	if tx == nil {
		return errors.New("nil tx")
	}

	// broadcast signature request
	sigRequestMsg := SignatureRequestMessage{
		TxCid:       tx.String(),
		MedianPrice: medianPriceMap,
	}

	msg, err := makePriceOracleMessage(signatureRequestCode, &sigRequestMsg)
	if err != nil {
		return fmt.Errorf("failed to make message: %w", err)
	}

	if err := p.Broadcast(p2p.MsgPriceOracle, msg); err != nil {
		return fmt.Errorf("failed to broadcast signature request: %w", err)
	}

	// TODO: make bls circuit

	// TODO: collect and verify signatures with bls circuit

	return nil
}

// witness
type Witness struct{ p2p.OracleP2PSpec }

func (w *Witness) handle(tx blocks.Block, medianPriceMap map[string]api.PricePoint) error {
	if tx == nil {
		return errors.New("nil tx")
	}

	return nil
}

// sort b and returns the median:
// - if b has odd elements, returns the mid value
// - if b has even elements, returns the mean of the 2 mid values
func getMedianValue(b []float64) float64 {
	slices.Sort(b)

	if len(b)&1 == 1 {
		return b[len(b)/2]
	}

	i := len(b) / 2
	return (b[i] + b[i-1]) / 2.0
}
