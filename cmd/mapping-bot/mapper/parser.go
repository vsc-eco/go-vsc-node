package mapper

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"vsc-node/modules/common/params"

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
	BlockHeight uint64 `json:"block_height"`
	RawTxHex    string `json:"raw_tx_hex,omitempty"`
	// RawTxB64: the transaction in base64 (with its witness when that fits, else
	// without), sent instead of raw_tx_hex when the hex payload would pass the
	// node's contract-payload cap (MAP-SIZE-1, see fitTxData).
	RawTxB64       string `json:"raw_tx_b64,omitempty"`
	MerkleProofHex string `json:"merkle_proof_hex"` // array of byte arrays, each of which is guaranteed 32 bytes
	TxIndex        uint64 `json:"tx_index"`         // position of the tx in the block
}

// payloadNodeSize is a contract-call payload's size as the node checks it against
// MAX_CONTRACT_PAYLOAD_SIZE: the node keeps the payload as a JSON string decoded
// from the call's CBOR (DecodeTxCbor), so it counts the string literal (two quotes,
// one backslash per '"' or '\'), as the EVM bot's nodePayloadSize does.
func payloadNodeSize(v any) int {
	payload, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	lit, err := json.Marshal(string(payload))
	if err != nil {
		return len(payload)
	}
	return len(lit)
}

func mapPayloadNodeSize(in *MappingInputData) int { return payloadNodeSize(in) }

// fitTxData (MAP-SIZE-1, mainnet 11-05: an Uphold deposit, 122 outputs, a 9,040-byte
// map, refused 'payload exceeds maximum size of 8192 bytes' and never credited): when
// the payload passes the node's contract-payload cap with raw_tx_hex, the transaction
// goes in base64 instead. With its witness when that fits: the contract's map log names
// the sender from the input witness. Otherwise without it: the proof (txid) and every
// check (inputs, outputs) leave the witness out. A payload that fits keeps raw_tx_hex,
// byte-identical to before. The same cap binds confirmSpend: a vault spend's witnesses
// are ~230 bytes per input in hex, so a sweep of more than ~15 deposit coins (the
// contract allows 100 per tranche) could never be confirmed in hex.
func fitTxData(td *VerificationRequest, tx *wire.MsgTx, size func() int) {
	if size() <= params.MAX_CONTRACT_PAYLOAD_SIZE {
		return
	}
	hexForm := td.RawTxHex
	var full bytes.Buffer
	if err := tx.Serialize(&full); err == nil {
		td.RawTxB64 = base64.StdEncoding.EncodeToString(full.Bytes())
		td.RawTxHex = ""
		if size() <= params.MAX_CONTRACT_PAYLOAD_SIZE {
			return
		}
	}
	var bare bytes.Buffer
	if err := tx.SerializeNoWitness(&bare); err != nil {
		td.RawTxB64, td.RawTxHex = "", hexForm
		return
	}
	td.RawTxB64 = base64.StdEncoding.EncodeToString(bare.Bytes())
	td.RawTxHex = ""
}

func fitMapPayload(in *MappingInputData, tx *wire.MsgTx) {
	fitTxData(in.TxData, tx, func() int { return mapPayloadNodeSize(in) })
}

// refitStoredPayload (MAP-SIZE-1): a map or confirmSpend recorded as failed with
// raw_tx_hex past the node's cap (by a bot without fitTxData: mainnet 11-05, the Uphold
// deposit) fails the same way when /retry resends it as stored. Re-fit it; a payload
// within the cap, or one this cannot parse, is returned unchanged.
func refitStoredPayload(action string, payload json.RawMessage) json.RawMessage {
	if action != "map" && action != "confirmSpend" {
		return payload
	}
	if payloadNodeSize(payload) <= params.MAX_CONTRACT_PAYLOAD_SIZE {
		return payload
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return payload
	}
	var td VerificationRequest
	if err := json.Unmarshal(fields["tx_data"], &td); err != nil || td.RawTxHex == "" {
		return payload
	}
	raw, err := hex.DecodeString(td.RawTxHex)
	if err != nil {
		return payload
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return payload
	}
	size := func() int {
		b, err := json.Marshal(&td)
		if err != nil {
			return 0
		}
		fields["tx_data"] = b
		return payloadNodeSize(fields)
	}
	fitTxData(&td, &tx, size)
	b, err := json.Marshal(&td)
	if err != nil {
		return payload
	}
	fields["tx_data"] = b
	out, err := json.Marshal(fields)
	if err != nil {
		return payload
	}
	return out
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

	observed, err := b.gql().FetchObservedAtHeight(ctx, blockHeight)
	if err != nil {
		b.L.Debug("error fetching observed outputs for block", "height", blockHeight, "error", err)
		return nil, err
	}

	// BOT-ADDR-2 / BOT-DEAD-1: match against every address the contract credits for
	// a known instruction (deposit_index.go). A read error holds the block.
	if err := b.refreshDepositIndex(ctx); err != nil {
		return nil, err
	}

	// map of indices to their deposit instructions
	matchedTxIndices := make(map[int][]string)

	for txIndex, tx := range msgBlock.Transactions {
		for i, txOut := range tx.TxOut {
			addresses := b.extractAddresses(txOut.PkScript)

			// this loop should never be longer than one cycle, only happens with multisig which is outdated
			for _, addr := range addresses {
				target, found, err := b.lookupDeposit(ctx, addr)
				if err != nil {
					return nil, err
				}
				if !found {
					continue
				}
				if !target.Creditable {
					// The contract would accept the map and credit nothing; report the
					// stranded coin instead of submitting it.
					b.reportUncreditableDeposit(tx.TxID(), i, txOut.Value, addr, target, blockHeight)
					continue
				}
				b.L.Debug("instruction address found", "instruction", target.Instruction, "generation", target.Generation)
				if observed.Has(tx.TxID(), i) {
					break
				}
				matchedTxIndices[txIndex] = append(matchedTxIndices[txIndex], target.Instruction)
			}
		}
	}

	b.L.Debug(
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

		in := &MappingInputData{
			TxData: &VerificationRequest{
				BlockHeight:    blockHeight,
				RawTxHex:       rawTxHex,
				MerkleProofHex: merkleProofHex,
				TxIndex:        uint64(txIdx),
			},
			Instructions: instructions,
		}
		fitMapPayload(in, tx)
		mapInputs = append(mapInputs, in)
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
