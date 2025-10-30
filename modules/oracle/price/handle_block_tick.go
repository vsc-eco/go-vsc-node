package price

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"
)

type CollectedPricePoint struct {
	prices  []float64
	volumes []float64
}

// HandleBlockTick implements oracle.BlockTickHandler.
func (o *PriceOracle) HandleBlockTick(
	sig p2p.BlockTickSignal,
	p2pSpec p2p.OracleP2PSpec,
) {
	o.logger.Debug("block tick event")

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
	o.logger.Debug(
		"average prices broadcasted",
		"avg-prices", string(msg.Payload),
	)

	if !sig.IsWitness {
		return
	}

	// collect average prices from the network + get median prices
	ctx, cancel := context.WithTimeout(o.ctx, 15*time.Second)
	defer cancel()

	collectedPricePoint, err := o.collectAveragePricePoints(ctx)
	if err != nil {
		o.logger.Error("failed collect average prices from network", "err", err)
		return
	}

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

	var handler interface {
		handle(map[string]api.PricePoint) error
	}

	if sig.IsProducer {
		handler = &Producer{
			OracleP2PSpec:    p2pSpec,
			ctx:              o.ctx,
			logger:           o.logger,
			blockTickSignal:  sig,
			signatureChannel: o.sigResponseChannel,
		}
	} else {
		handler = &Witness{
			OracleP2PSpec:           p2pSpec,
			ctx:                     o.ctx,
			logger:                  o.logger,
			blockTickSignal:         sig,
			signatureRequestChannel: o.sigRequestChannel,
			identity:                o.conf,
		}
	}

	if err := handler.handle(medianPriceMap); err != nil {
		o.logger.Error(
			"failed to handle block tick",
			"is-producer", sig.IsProducer,
			"is-witness", sig.IsWitness,
			"err", err,
		)
	}
}

func (o *PriceOracle) collectAveragePricePoints(
	ctx context.Context,
) (map[string]CollectedPricePoint, error) {
	o.logger.Debug("collecting average prices")

	out := make(map[string]CollectedPricePoint)

	priceReceiver := o.priceChannel.Open()
	defer o.priceChannel.Close()

	for {
		select {
		case <-ctx.Done():
			err := ctx.Err()

			// if timed out, ignore the error
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				return out, nil
			}

			return nil, err

		case priceMap := <-priceReceiver:
			for symbol, pp := range priceMap {
				if math.IsNaN(pp.Volume) || math.IsNaN(pp.Price) {
					o.logger.Debug(
						"NaN values, price point dropped",
						"price", pp.Price,
						"volume", pp.Volume,
					)
					continue
				}

				symbol = strings.ToLower(symbol)

				p, ok := out[symbol]
				if !ok {
					p = CollectedPricePoint{
						prices:  []float64{pp.Price},
						volumes: []float64{pp.Volume},
					}
				} else {
					p.prices = append(p.prices, pp.Price)
					p.volumes = append(p.volumes, pp.Volume)
				}

				out[symbol] = p
			}
		}
	}
}

// witness
