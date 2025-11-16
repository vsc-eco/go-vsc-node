package chain

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ipfs/go-cid"
	blsu "github.com/protolambda/bls12-381-util"
)

var (
	errInvalidBlockProducer = errors.New("invalid block producer")
	errInvalidChainHash     = errors.New("invalid chain hash")
)

type chainOracleWitness struct {
	logger            *slog.Logger
	chainMap          map[string]chainRelay
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

	// get blocks from chain
	chain, blocks, err := getSessionData(o.chainMap, o.sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get blocks from chain: %w", err)
	}

	o.logger.Debug(
		"got blocks",
		"blocksCount", len(blocks),
	)

	// verify blocks against block producer's hashes
	tx, err := makeChainTx(chain, blocks)
	if err != nil {
		return nil, fmt.Errorf("failed to create tx: %w", err)
	}
	txCid := tx.Cid()

	chainHashMatch := txCid.String() == msg.SigHash
	if !chainHashMatch {
		return nil, errInvalidChainHash
	}

	// sign and respond
	signature, err := o.signChainData(&txCid)
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
	txCid *cid.Cid,
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

	sigBytes := blsu.Sign(blsSecretKey, txCid.Bytes()).Serialize()
	sig := base64.RawURLEncoding.EncodeToString(sigBytes[:])

	return sig, nil
}
