package parser

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type VerificationRequest struct {
	BlockHeight    uint32 `json:"block_height"`
	RawTxHex       string `json:"raw_tx_hex"`
	MerkleProofHex string `json:"merkle_proof_hex"` // array of byte arrays, each of which is guaranteed 32 bytes
	TxIndex        uint32 `json:"tx_index"`         // position of the tx in the block
}

type BlockParser struct {
	targetBtcAddresses map[string]bool
	chainParams        *chaincfg.Params
}

func NewBlockParser(addresses map[string]string, params *chaincfg.Params) *BlockParser {
	btcAddrMap := make(map[string]bool, len(addresses))
	for _, btcAddr := range addresses {
		btcAddrMap[btcAddr] = true
	}

	return &BlockParser{
		targetBtcAddresses: btcAddrMap,
		chainParams:        params,
	}
}

func (bp *BlockParser) ParseBlock(rawBlockBytes []byte, blockHeight uint32) ([]VerificationRequest, error) {
	var msgBlock wire.MsgBlock
	err := msgBlock.Deserialize(bytes.NewReader(rawBlockBytes))
	if err != nil {
		return nil, err
	}

	matchedTxIndices := make(map[int]bool)

	for txIdx, tx := range msgBlock.Transactions {
		for _, txOut := range tx.TxOut {
			addresses := bp.extractAddresses(txOut.PkScript)

			for _, addr := range addresses {
				if bp.targetBtcAddresses[addr] {
					matchedTxIndices[txIdx] = true
					break
				}
			}
			if matchedTxIndices[txIdx] {
				break
			}
		}
	}

	var verificationRequests []VerificationRequest

	for txIdx := range matchedTxIndices {
		tx := msgBlock.Transactions[txIdx]

		var txBuf bytes.Buffer
		err := tx.Serialize(&txBuf)
		if err != nil {
			return nil, err
		}
		rawTxHex := hex.EncodeToString(txBuf.Bytes())

		merkleProofHex, err := bp.generateMerkleProof(&msgBlock, txIdx)
		if err != nil {
			return nil, err
		}

		verificationRequests = append(verificationRequests, VerificationRequest{
			BlockHeight:    blockHeight,
			RawTxHex:       rawTxHex,
			MerkleProofHex: merkleProofHex,
			TxIndex:        uint32(txIdx),
		})
	}

	return verificationRequests, nil
}

func (bp *BlockParser) extractAddresses(pkScript []byte) []string {
	var addresses []string

	scriptClass, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, bp.chainParams)
	if err != nil {
		// fine if error, just means no addresses to extract
		return addresses
	}

	if scriptClass != txscript.NonStandardTy {
		for _, addr := range addrs {
			addresses = append(addresses, addr.EncodeAddress())
		}
	}

	return addresses
}

func (bp *BlockParser) generateMerkleProof(block *wire.MsgBlock, txIndex int) (string, error) {
	txCount := len(block.Transactions)

	txHashes := make([]*chainhash.Hash, txCount)
	for i, tx := range block.Transactions {
		hash := tx.TxHash()
		txHashes[i] = &hash
	}

	proof := []chainhash.Hash{}
	index := txIndex

	currentLevel := txHashes

	for len(currentLevel) > 1 {
		var siblingIdx int
		if index%2 == 0 {
			// even, sibling is to the right
			siblingIdx = index + 1
		} else {
			// odd, sibling is to the left
			siblingIdx = index - 1
		}

		// add sibling to proof (handle case where is last odd node)
		if siblingIdx < len(currentLevel) {
			proof = append(proof, *currentLevel[siblingIdx])
		} else {
			// duplicate current node (btc merkle tree rule)
			proof = append(proof, *currentLevel[index])
		}

		// build next level
		nextLevel := []*chainhash.Hash{}
		for i := 0; i < len(currentLevel); i += 2 {
			var left, right *chainhash.Hash
			left = currentLevel[i]

			if i+1 < len(currentLevel) {
				right = currentLevel[i+1]
			} else {
				// duplicate if odd number of nodes
				right = currentLevel[i]
			}

			combined := append(left[:], right[:]...)
			hash := chainhash.DoubleHashH(combined)
			nextLevel = append(nextLevel, &hash)
		}

		currentLevel = nextLevel
		index = index / 2
	}

	// concatenate and encode to hex, formatted for contract
	var proofBytes []byte
	for _, hash := range proof {
		proofBytes = append(proofBytes, hash[:]...)
	}

	return hex.EncodeToString(proofBytes), nil
}
