package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"

	"vsc-node/cmd/mapping-bot/database"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type MappingInputData struct {
	TxData *VerificationRequest `json:"tx_data"`
	// strings should be valid URL search params, to be decoded later
	Instructions []string `json:"instructions"`
}

type VerificationRequest struct {
	BlockHeight    uint64 `json:"block_height"`
	RawTxHex       string `json:"raw_tx_hex"`
	MerkleProofHex string `json:"merkle_proof_hex"` // array of byte arrays, each of which is guaranteed 32 bytes
	TxIndex        uint64 `json:"tx_index"`         // position of the tx in the block
}

func (b *Bot) ParseBlock(
	ctx context.Context,
	rawBlockBytes []byte,
	blockHeight uint64,
) ([]*MappingInputData, error) {
	var msgBlock wire.MsgBlock
	err := msgBlock.Deserialize(bytes.NewReader(rawBlockBytes))
	if err != nil {
		return nil, err
	}

	// map of indices to their deposit instructions
	matchedTxIndices := make(map[int][]string)

	for txIndex, tx := range msgBlock.Transactions {
		for i, txOut := range tx.TxOut {
			addresses := b.extractAddresses(txOut.PkScript)

			// this loop should never be longer than one cycle, only happens with multisig which is outdated
			for _, addr := range addresses {
				if instruction, err := b.Db.Addresses.GetInstruction(ctx, addr); err == nil {
					slog.Debug("instruction address found", "instruction", instruction)
					exists, err := b.FetchObservedTx(ctx, tx.TxID(), i)
					if exists || err != nil {
						slog.Debug("error fetching observed tx", "exits", exists, "error", err)
						break
					}
					matchedTxIndices[txIndex] = append(matchedTxIndices[txIndex], instruction)
				} else if err != database.ErrAddrNotFound {
					return nil, err
				}
			}
		}
	}

	slog.Debug(
		"number of utxos in the block that correspond to vsc addresses",
		"len(matchedTxIndices)",
		len(matchedTxIndices),
	)

	var mapInputs []*MappingInputData

	for txIdx, instructions := range matchedTxIndices {
		tx := msgBlock.Transactions[txIdx]

		var txBuf bytes.Buffer
		err := tx.Serialize(&txBuf)
		if err != nil {
			return nil, err
		}
		rawTxHex := hex.EncodeToString(txBuf.Bytes())

		merkleProofHex, err := generateMerkleProof(&msgBlock, txIdx)
		if err != nil {
			return nil, err
		}

		mapInputs = append(mapInputs, &MappingInputData{
			TxData: &VerificationRequest{
				BlockHeight:    blockHeight,
				RawTxHex:       rawTxHex,
				MerkleProofHex: merkleProofHex,
				TxIndex:        uint64(txIdx),
			},
			Instructions: instructions,
		})
	}

	return mapInputs, nil
}

func (b *Bot) extractAddresses(pkScript []byte) []string {
	var addresses []string

	scriptClass, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript, b.ChainParams)
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

func generateMerkleProof(block *wire.MsgBlock, txIndex int) (string, error) {
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
