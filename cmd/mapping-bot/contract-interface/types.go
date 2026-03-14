package contractinterface

//go:generate msgp
type SigningData struct {
	Tx                []byte            `msg:"tx"`
	UnsignedSigHashes []UnsignedSigHash `msg:"uh"`
}

type UnsignedSigHash struct {
	Index         uint32 `msg:"i"`
	SigHash       []byte `msg:"hs"`
	WitnessScript []byte `msg:"ws"`
}

// TxSpendsRegistry is a list of transaction IDs (hex-encoded 32-byte txids).
type TxSpendsRegistry []string
