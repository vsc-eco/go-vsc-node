package chain

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// BTCBlockParser implements BlockParser for Bitcoin (and forks like LTC/DASH
// that share the same block wire format).
type BTCBlockParser struct {
	Params *chaincfg.Params
}

func (p *BTCBlockParser) ParseBlock(rawBlock []byte, knownAddresses []string, blockHeight uint64) ([]MappingInput, error) {
	var msgBlock wire.MsgBlock
	if err := msgBlock.Deserialize(bytes.NewReader(rawBlock)); err != nil {
		return nil, err
	}

	// Build a set for O(1) address lookups
	addrSet := make(map[string]bool, len(knownAddresses))
	for _, addr := range knownAddresses {
		addrSet[addr] = true
	}

	// Find transactions with outputs to known addresses
	matchedIndices := make(map[int]bool)
	for txIdx, tx := range msgBlock.Transactions {
		for _, txOut := range tx.TxOut {
			for _, addr := range extractBTCAddresses(txOut.PkScript, p.Params) {
				if addrSet[addr] {
					matchedIndices[txIdx] = true
				}
			}
		}
	}

	var results []MappingInput
	for txIdx := range matchedIndices {
		tx := msgBlock.Transactions[txIdx]

		var txBuf bytes.Buffer
		if err := tx.Serialize(&txBuf); err != nil {
			return nil, err
		}

		merkleProofHex, err := generateBTCMerkleProof(&msgBlock, txIdx)
		if err != nil {
			return nil, err
		}

		results = append(results, MappingInput{
			RawTxHex:       hex.EncodeToString(txBuf.Bytes()),
			MerkleProofHex: merkleProofHex,
			TxIndex:        uint32(txIdx),
			BlockHeight:    uint32(blockHeight),
		})
	}

	return results, nil
}

func extractBTCAddresses(pkScript []byte, params *chaincfg.Params) []string {
	scriptClass, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, params)
	if err != nil || scriptClass == txscript.NonStandardTy {
		return nil
	}
	result := make([]string, len(addrs))
	for i, addr := range addrs {
		result[i] = addr.EncodeAddress()
	}
	return result
}

func generateBTCMerkleProof(block *wire.MsgBlock, txIndex int) (string, error) {
	txHashes := make([]*chainhash.Hash, len(block.Transactions))
	for i, tx := range block.Transactions {
		hash := tx.TxHash()
		txHashes[i] = &hash
	}

	var proof []chainhash.Hash
	index := txIndex
	currentLevel := txHashes

	for len(currentLevel) > 1 {
		var siblingIdx int
		if index%2 == 0 {
			siblingIdx = index + 1
		} else {
			siblingIdx = index - 1
		}

		if siblingIdx < len(currentLevel) {
			proof = append(proof, *currentLevel[siblingIdx])
		} else {
			proof = append(proof, *currentLevel[index])
		}

		var nextLevel []*chainhash.Hash
		for i := 0; i < len(currentLevel); i += 2 {
			left := currentLevel[i]
			right := left
			if i+1 < len(currentLevel) {
				right = currentLevel[i+1]
			}
			combined := append(left[:], right[:]...)
			hash := chainhash.DoubleHashH(combined)
			nextLevel = append(nextLevel, &hash)
		}

		currentLevel = nextLevel
		index = index / 2
	}

	var proofBytes []byte
	for _, hash := range proof {
		proofBytes = append(proofBytes, hash[:]...)
	}
	return hex.EncodeToString(proofBytes), nil
}
