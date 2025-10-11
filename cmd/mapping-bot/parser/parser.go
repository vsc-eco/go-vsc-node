package parser

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const depositInstruction = "deposit_to"

type MappingInputData struct {
	TxData *VerificationRequest `json:"tx_data"`
	// strings should be valid URL search params, to be decoded later
	Instructions []string `json:"instructions"`
}

type VerificationRequest struct {
	BlockHeight    uint32 `json:"block_height"`
	RawTxHex       string `json:"raw_tx_hex"`
	MerkleProofHex string `json:"merkle_proof_hex"` // array of byte arrays, each of which is guaranteed 32 bytes
	TxIndex        uint32 `json:"tx_index"`         // position of the tx in the block
}

type BlockParser struct {
	addressDb   *database.MappingBotDatabase
	chainParams *chaincfg.Params
}

func NewBlockParser(addressDb *database.MappingBotDatabase, params *chaincfg.Params) *BlockParser {
	return &BlockParser{
		addressDb:   addressDb,
		chainParams: params,
	}
}

func (bp *BlockParser) ParseBlock(rawBlockBytes []byte, blockHeight uint32, observedTxs map[string]bool) ([]*MappingInputData, error) {
	var msgBlock wire.MsgBlock
	err := msgBlock.Deserialize(bytes.NewReader(rawBlockBytes))
	if err != nil {
		return nil, err
	}

	// map of indices to their deposit instructions
	matchedTxIndices := make(map[int][]string)

	for txIndex, tx := range msgBlock.Transactions {
		if observedTxs[tx.TxID()] {
			continue
		}
		for _, txOut := range tx.TxOut {
			addresses := bp.extractAddresses(txOut.PkScript)

			// this loop should never be longer than one cycle, only happens with multisig which is outdated
			for _, addr := range addresses {
				if vscAddr, err := bp.addressDb.GetVscAddress(context.TODO(), addr); err == nil {
					instruction := fmt.Sprintf("%s=%s", depositInstruction, vscAddr)
					matchedTxIndices[txIndex] = append(matchedTxIndices[txIndex], instruction)
				} else if err != database.ErrAddrNotFound {
					return nil, err
				}
			}
		}
	}

	var mapInputs []*MappingInputData

	for txIdx, instructions := range matchedTxIndices {
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

		mapInputs = append(mapInputs, &MappingInputData{
			TxData: &VerificationRequest{
				BlockHeight:    blockHeight,
				RawTxHex:       rawTxHex,
				MerkleProofHex: merkleProofHex,
				TxIndex:        uint32(txIdx),
			},
			Instructions: instructions,
		})
	}

	return mapInputs, nil
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
