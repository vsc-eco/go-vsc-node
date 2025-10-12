package chain

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	blsu "github.com/protolambda/bls12-381-util"
)

const rawBlsSignatureLength = 96

var errInvalidBlockProducer = errors.New("invalid block producer")

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
	chainSymbol = strings.ToUpper(chainSymbol)

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
	ok, err = o.verifyBlockHashes(blocks, msg.BlockHash)
	if err != nil {
		return nil, err
	}

	// sign and respond
	signature, err := o.signChainData(
		chainSymbol,
		chain.ContractID(),
		msg.BlockHash,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to sign producer block: %w", err)
	}

	response := chainOracleWitnessMessage{
		Signature: signature,
		Signer:    o.username,
	}

	return &response, nil
}

func (w *chainOracleWitness) verifyBlockHashes(
	chainBlocks []chainBlock,
	blockHashes []string,
) (bool, error) {
	if len(chainBlocks) != len(blockHashes) {
		return false, nil
	}

	for i, block := range chainBlocks {
		blockHash, err := block.Serialize()
		if err != nil {
			return false, fmt.Errorf(
				"failed to serialize block: symbol %s, blockHeight %d, err %w",
				block.Type(),
				block.BlockHeight(),
				err,
			)
		}

		if blockHash != blockHashes[i] {
			w.logger.Debug(
				"failed to verify block",
				"block symbol", block.Type(),
				"block height", block.BlockHeight(),
			)
			return false, nil
		}
	}

	return true, nil
}

// TODO: sign chain data, 96 bytes signature with base64.RawStdEncoding
func (w *chainOracleWitness) signChainData(
	symbol string,
	contractID string,
	payload []string,
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

	// hash payload + sign the digest
	nonce := uint64(0) // TODO: pull this from contract graphql
	block, err := makeSignableBlock(symbol, contractID, payload, nonce)

	sigBytes := blsu.Sign(blsSecretKey, block.Cid().Hash()).Serialize()

	sig := base64.RawURLEncoding.EncodeToString(sigBytes[:])
	return sig, nil
}
