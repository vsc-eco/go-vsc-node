package price

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"vsc-node/modules/common"
	"vsc-node/modules/oracle/p2p"
	"vsc-node/modules/oracle/price/api"

	blsu "github.com/protolambda/bls12-381-util"
)

type Witness struct {
	p2p.OracleP2PSpec
	ctx                     context.Context
	logger                  *slog.Logger
	blockTickSignal         p2p.BlockTickSignal
	signatureRequestChannel *SignatureRequestChannel
	identity                common.IdentityConfig
}

func (w *Witness) handle(medianPriceMap map[string]api.PricePoint) error {
	w.logger.Debug(
		"witness median prices",
		"median-prices", medianPriceMap,
	)

	//  receiving median price
	receiver := w.signatureRequestChannel.Open()
	defer w.signatureRequestChannel.Close()

	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()

	var signatureRequest SignatureRequestMessage
	select {
	case <-ctx.Done():
		return ctx.Err()

	case signatureRequest = <-receiver:
		w.logger.Debug("signature request received")
	}

	w.logger.Debug("signature request received",
		"median-price", signatureRequest.MedianPrice,
		"signature-hash", signatureRequest.SigHash,
	)

	// compare broadcasted vs local median price
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

	w.logger.Debug("median price verified", "broadcasted-median-price", signatureRequest.MedianPrice)

	// make tx + verify incoming sig hash
	tx, err := makeTx(signatureRequest.MedianPrice)
	if err != nil {
		return fmt.Errorf("failed to create transaction: %w", err)
	}

	txCid := tx.Cid()
	sigHashOk := signatureRequest.SigHash == txCid.String()
	if !sigHashOk {
		return fmt.Errorf("invalid signature hash")
	}

	w.logger.Debug("signature hash verified", "sig-hash", signatureRequest.SigHash)

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

	// broadcast signature
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
	w.logger.Debug(
		"signature response broadcasted",
		"signature", signatureResponse.Signature,
	)

	return nil
}
