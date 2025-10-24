package price

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	blsu "github.com/protolambda/bls12-381-util"
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

	// make transaction
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
			ctx:                     ctx,
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
					o.logger.Debug("NaN values dropped", "price", pp.Price, "volume", pp.Volume)
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

// block producer
type Producer struct {
	p2p.OracleP2PSpec
	ctx              context.Context
	logger           *slog.Logger
	blockTickSignal  p2p.BlockTickSignal
	signatureChannel *SignatureResponseChannel
}

func (p *Producer) handle(medianPriceMap map[string]api.PricePoint) error {
	tx, err := makeTx(medianPriceMap)
	if err != nil {
		return fmt.Errorf("failed to create tx: %w", err)
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

	// make bls circuit
	txCid := tx.Cid()

	witnessDIDs := utils.Map(
		p.blockTickSignal.ElectedMembers,
		func(w elections.ElectionMember) dids.Member {
			return dids.BlsDID(w.Key)
		},
	)

	circuit, err := dids.NewBlsCircuitGenerator(witnessDIDs).Generate(txCid)
	if err != nil {
		return fmt.Errorf("failed to generate bls circuit: %w", err)
	}

	// collect and verify signatures with bls circuit
	signedWeight := uint64(0)
	weightThreshold := (p.blockTickSignal.TotalElectionWeight * 2) / 3

	receiver := p.signatureChannel.Open()
	defer p.signatureChannel.Close()

	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()

	for signedWeight < weightThreshold {
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("context error: %w", err)
			} else {
				return errors.New("signature collection timed out")
			}

		case msg := <-receiver:
			memberIndex := slices.IndexFunc(
				p.blockTickSignal.ElectedMembers,
				func(e elections.ElectionMember) bool {
					return e.Account == msg.Signer
				},
			)

			if memberIndex == -1 {
				p.logger.Debug(
					"invalid witness signature, dropping.",
					"signer", msg.Signer,
				)
				continue
			}

			member := &p.blockTickSignal.ElectedMembers[memberIndex]
			memberDID := dids.BlsDID(member.Key)

			added, err := circuit.AddAndVerify(memberDID, msg.Signature)
			if err != nil {
				p.logger.Error("failed to add member to circuit.", "err", err)
				continue
			}

			if !added {
				p.logger.Debug(
					"invalid member, signature not added to circuit",
				)
				continue
			}

			signedWeight += p.blockTickSignal.WeightMap[memberIndex]

			p.logger.Debug(
				"received witness signature, appending to witnessSigned",
				"signer", member.Account, "signature", msg.Signature,
			)
		}
	}

	// make transaction + submit to contract
	blsCircuit, err := circuit.Finalize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	serializedCircuit, err := blsCircuit.Serialize()
	if err != nil {
		return fmt.Errorf("failed to finalize circuit: %w", err)
	}

	sigPackage := stateEngine.TransactionSig{
		Type: "vsc-sig",
		Sigs: []common.Sig{
			{
				Algo: "bls-agg",
				Sig:  serializedCircuit.Signature,
				Bv:   serializedCircuit.BitVector,
				Kid:  "",
			},
		},
	}

	sigBytes, err := common.EncodeDagCbor(sigPackage)
	if err != nil {
		return fmt.Errorf("failed encode DagCbor: %w", err)
	}

	// submit contract
	vscTx := transactionpool.SerializedVSCTransaction{
		Tx:  txCid.Bytes(),
		Sig: sigBytes,
	}

	if err := p.submitToContract(vscTx); err != nil {
		return fmt.Errorf("failed to submit to contract: %w", err)
	}

	return nil
}

// TODO: implement this function
func (p *Producer) submitToContract(transactionpool.SerializedVSCTransaction) error {
	return nil
}

// witness
type Witness struct {
	p2p.OracleP2PSpec
	ctx                     context.Context
	logger                  *slog.Logger
	blockTickSignal         p2p.BlockTickSignal
	signatureRequestChannel *SignatureRequestChannel
	identity                common.IdentityConfig
}

func (w *Witness) handle(medianPriceMap map[string]api.PricePoint) error {
	var signatureRequest SignatureRequestMessage

	receiver := w.signatureRequestChannel.Open()
	defer w.signatureRequestChannel.Close()

	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case signatureRequest = <-receiver:
	}

	for sym, pricePoint := range signatureRequest.MedianPrice {
		localPricePoint, ok := medianPriceMap[sym]
		if !ok {
			return fmt.Errorf("failed to verify median price point for ticker %s", sym)
		}

		priceOk := floatEqual(pricePoint.Price, localPricePoint.Price)
		if !priceOk {
			return fmt.Errorf("failed to verify median price for ticker %s", sym)
		}

		volumeOk := floatEqual(pricePoint.Volume, localPricePoint.Volume)
		if !volumeOk {
			return fmt.Errorf("failed to verify median volume for ticker %s", sym)
		}
	}

	// make tx + verify incoming sig hash
	tx, err := makeTx(signatureRequest.MedianPrice)
	if err != nil {
		return fmt.Errorf("failed to create transaction: %w", err)
	}

	txCid := tx.Cid()
	sigHashOk := signatureRequest.TxCid == txCid.String()
	if !sigHashOk {
		return fmt.Errorf("invalid signature hash")
	}

	// sign data
	blsKeyDecoded, err := hex.DecodeString(w.identity.Get().BlsPrivKeySeed)
	if err != nil {
		return fmt.Errorf("failed to decode BLS private key seed: %w", err)
	}

	if len(blsKeyDecoded) != 32 {
		return errors.New("bls priv seed must be 32 bytes")
	}

	var blsKeyBuf [32]byte
	copy(blsKeyBuf[:], blsKeyDecoded)

	blsSecretKey := &blsu.SecretKey{}
	if err := blsSecretKey.Deserialize(&blsKeyBuf); err != nil {
		return fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}

	sigBytes := blsu.Sign(blsSecretKey, txCid.Bytes()).Serialize()

	signatureResponse := &SignatureResponseMessage{
		Signer:    w.identity.Get().HiveUsername,
		Signature: base64.RawURLEncoding.EncodeToString(sigBytes[:]),
	}

	msg, err := makePriceOracleMessage(signatureResponseCode, signatureResponse)
	if err != nil {
		return fmt.Errorf("failed to make price oracle message: %w", err)
	}

	if err := w.Broadcast(p2p.MsgPriceOracle, msg); err != nil {
		return fmt.Errorf("failed to broadcast signature response: %w", err)
	}

	return nil
}

func floatEqual(a, b float64) bool {
	const epsilon = 1e-9
	return math.Abs(a-b) < epsilon
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
