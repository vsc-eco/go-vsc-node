package chain

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	blocks "github.com/ipfs/go-block-format"
	blsu "github.com/protolambda/bls12-381-util"
)

var (
	errInvalidBlockProducer = errors.New("invalid block producer")
	errInvalidChainHash     = errors.New("invalid chain hash")
)

type chainOracleWitness struct {
	logger            *slog.Logger
	username          string
	privateBlsKeySeed string
	sessionID         string
	chainRelayMap     map[string]chainRelay
	blockProducer     string
}

// signs off chain data and returns a signature
func (o *chainOracleWitness) witnessChainData(
	msg *chainOracleBlockProducerMessage,
) (*chainOracleWitnessMessage, error) {
	if o.blockProducer != msg.BlockProducer {
		return nil, errInvalidBlockProducer
	}

	// get blocks from btcd
	chainSymbol, startBlock, endBlock, err := parseChainSessionID(o.sessionID)
	if err != nil {
		return nil, fmt.Errorf("invalid session id: %w", err)
	}
	chainSymbol = strings.ToLower(chainSymbol)

	chain, ok := o.chainRelayMap[chainSymbol]
	if !ok {
		return nil, errInvalidChainSymbol
	}

	count := (endBlock - startBlock) + 1
	blocks, err := chain.ChainData(startBlock, count)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to fetch blocks: symbol %s startBlock %d, endBlock %d, err %w",
			chainSymbol,
			startBlock,
			endBlock,
			err,
		)
	}

	o.logger.Debug(
		"got blocks from btcd",
		"chainSymbol", chainSymbol,
		"startBlock", startBlock,
		"endBlock", endBlock,
		"blocksCount", len(blocks),
	)

	// verify blocks against block producer's hashes
	tx, err := makeChainTx(chain, blocks)
	if err != nil {
		return nil, fmt.Errorf("failed to create tx: %w", err)
	}

	chainHashMatch := tx.Cid().String() == msg.SigHash
	if !chainHashMatch {
		return nil, errInvalidChainHash
	}

	// sign and respond
	signature, err := o.signChainData(tx)
	if err != nil {
		return nil, fmt.Errorf("failed to sign producer block: %w", err)
	}

	response := chainOracleWitnessMessage{
		Signature: signature,
		Signer:    o.username,
	}

	return &response, nil
}

func (w *chainOracleWitness) signChainData(
	payload blocks.Block,
) (string, error) {
	// decode bls key seed
	blsKeyDecoded, err := hex.DecodeString(w.privateBlsKeySeed)
	if err != nil {
		return "", fmt.Errorf("failed to deserialize bsl key seed: %w", err)
	}

	if len(blsKeyDecoded) != 32 {
		return "", errors.New("bls priv seed must be 32 bytes")
	}

	var blsKeyBuf [32]byte
	copy(blsKeyBuf[:], blsKeyDecoded)

	blsSecretKey := &blsu.SecretKey{}
	if err := blsSecretKey.Deserialize(&blsKeyBuf); err != nil {
		return "", fmt.Errorf("failed to deserialize bls priv key: %w", err)
	}

	sigBytes := blsu.Sign(blsSecretKey, payload.Cid().Bytes()).Serialize()

	sig := base64.RawURLEncoding.EncodeToString(sigBytes[:])
	return sig, nil
}
