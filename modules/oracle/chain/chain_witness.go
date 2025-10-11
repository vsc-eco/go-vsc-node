package chain

import (
	"errors"
	"fmt"
	"strings"
)

const rawBlsSignatureLength = 96

var errInvalidBlockProducer = errors.New("invalid block producer")

type chainOracleWitness struct {
	username      string
	privateKey    string
	sessionID     string
	chainRelayMap map[string]chainRelay
	blockProducer string
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

	chain, ok := o.chainRelayMap[strings.ToUpper(chainSymbol)]
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

	// verify blocks against block producer's hashes
	ok, err = o.verifyBlockHashes(blocks, msg.BlockHash)
	if err != nil {
		return nil, err
	}

	// sign and respond
	signature, err := o.signChainData(msg.BlockHash)
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
	a []chainBlock,
	b []string,
) (bool, error) {
	if len(a) != len(b) {
		return false, nil
	}

	for i, block := range a {
		blockHash, err := block.Serialize()
		if err != nil {
			return false, fmt.Errorf(
				"failed to serialize block: symbol %s, blockHeight %d, err %w",
				block.Type(),
				block.BlockHeight(),
				err,
			)
		}

		if blockHash != b[i] {
			return false, nil
		}
	}

	return true, nil
}

// TODO: sign chain data, 96 bytes signature with base64.RawStdEncoding
func (w *chainOracleWitness) signChainData(payload []string) (string, error) {
	return "", nil
}
