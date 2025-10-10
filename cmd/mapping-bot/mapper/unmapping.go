package mapper

import (
	"bytes"
	"encoding/hex"

	"github.com/btcsuite/btcd/wire"
)

type SigningData struct {
	Tx                string            `json:"tx"`
	UnsignedSigHashes []UnsignedSigHash `json:"unsigned_sig_hashes"`
}

type UnsignedSigHash struct {
	Index         uint32 `json:"index"`
	SigHash       []byte `json:"sig_hash"`
	WitnessScript []byte `json:"witness_script"`
}

func attachSignatures(signingData *SigningData, signatures map[uint32][]byte) (string, error) {
	var tx wire.MsgTx
	txBytes, err := hex.DecodeString(signingData.Tx)
	if err != nil {
		return "", err
	}
	tx.Deserialize(bytes.NewReader(txBytes))

	for _, inputData := range signingData.UnsignedSigHashes {
		signature := signatures[inputData.Index]

		witness := wire.TxWitness{
			signature,
			inputData.WitnessScript,
		}

		tx.TxIn[inputData.Index].Witness = witness
	}

	var buf bytes.Buffer
	// serialize is almost the same but with a different protocol version. Not sure if that
	// actually changes the result
	if err := tx.BtcEncode(&buf, wire.ProtocolVersion, wire.WitnessEncoding); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}
