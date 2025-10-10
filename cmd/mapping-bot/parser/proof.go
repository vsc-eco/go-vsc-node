package parser

import (
	"bytes"
	"encoding/hex"
	"errors"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func constructMerkleProof(proofHex string) ([]chainhash.Hash, error) {
	proofBytes, err := hex.DecodeString(proofHex)
	if err != nil {
		return nil, err
	}
	if len(proofBytes)%32 != 0 {
		return nil, errors.New("invalid proof format")
	}
	proof := make([]chainhash.Hash, len(proofBytes)/32)
	for i := 0; i < len(proofBytes); i += 32 {
		proof[i/32] = chainhash.Hash(proofBytes[i : i+32])
	}
	return proof, nil
}

func VerifyTransaction(req *VerificationRequest) error {
	rawHeader, err := hex.DecodeString("00000220cfb8b5e92b01d0562b1cce5debe08a9fcd8f6e4d6053000000000000000000003af0fea4a492bc80edd0b6b409e21d7c2eb469bcd4ed4f3d043bce768807833dc07ee768b4dd011779cfe4f2")
	if err != nil {
		return err
	}
	var blockHeader wire.BlockHeader
	blockHeader.BtcDecode(bytes.NewReader(rawHeader[:]), wire.ProtocolVersion, wire.LatestEncoding)

	rawTxBytes, err := hex.DecodeString(req.RawTxHex)
	if err != nil {
		return err
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(rawTxBytes)); err != nil {
		return err
	}

	merkleProof, err := constructMerkleProof(req.MerkleProofHex)
	if err != nil {
		return err
	}

	calculatedHash := tx.TxHash()

	if !verifyMerkleProof(calculatedHash, req.TxIndex, merkleProof, blockHeader.MerkleRoot) {
		return errors.New("transaction invalid")
	}
	return nil
}

func verifyMerkleProof(
	txHash chainhash.Hash,
	txIndex uint32,
	proof []chainhash.Hash,
	merkleRoot chainhash.Hash,
) bool {
	currentHash := txHash
	index := txIndex

	for _, siblingHash := range proof {
		var combined []byte
		if index%2 == 0 {
			combined = append(currentHash[:], siblingHash[:]...)
		} else {
			combined = append(siblingHash[:], currentHash[:]...)
		}

		hash := chainhash.DoubleHashH(combined)
		currentHash = hash
		index = index / 2
	}

	return currentHash.IsEqual(&merkleRoot)
}
