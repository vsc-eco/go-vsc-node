package btcvault

import (
	"bytes"
	"fmt"

	"github.com/tinylib/msgp/msgp"
)

// UnsignedSigHash mirrors the mapping contract's contract-interface
// UnsignedSigHash (msgp keys "i"/"hs"/"ws"), PLUS an optional Amount ("am").
// The contract at 7ecbf9f does not yet carry Amount; S3.5 adds it so the node
// can recompute the BIP143 sighash independently. Amount is decoded when
// present and left 0 when absent (forward/backward compatible — msgp is a map,
// unknown keys are skipped and missing keys stay zero-valued).
type UnsignedSigHash struct {
	Index         uint32
	SigHash       []byte
	WitnessScript []byte
	Amount        int64 // spent-input value in sats; 0 if the field is absent
	HasAmount     bool  // true only if the "am" field was present in the blob
}

// SigningData mirrors the mapping contract's contract-interface SigningData: a
// serialized BTC tx template plus one UnsignedSigHash per input.
type SigningData struct {
	Tx                []byte
	UnsignedSigHashes []UnsignedSigHash
}

// DecodeSigningData decodes the msgp-encoded SigningData stored under the
// contract "d-<txid>" state key. It is a hand-written, allocation-conservative
// reader over the msgp MAP encoding the contract's generated codec produces
// (WriteMapHeader + string keys) — deliberately NOT a lift of the generated
// types_gen.go, so the optional "am" amount field is tolerated on both the old
// (absent) and new (present) contract without a codec regen on the node side.
// Unknown keys are skipped, so any future contract-side field additions do not
// break this decoder.
func DecodeSigningData(raw []byte) (*SigningData, error) {
	r := msgp.NewReader(bytes.NewReader(raw))
	nfields, err := r.ReadMapHeader()
	if err != nil {
		return nil, fmt.Errorf("btcvault: signingdata map header: %w", err)
	}
	var sd SigningData
	for i := uint32(0); i < nfields; i++ {
		key, err := r.ReadString()
		if err != nil {
			return nil, fmt.Errorf("btcvault: signingdata key: %w", err)
		}
		switch key {
		case "tx":
			sd.Tx, err = r.ReadBytes(nil)
			if err != nil {
				return nil, fmt.Errorf("btcvault: signingdata tx: %w", err)
			}
		case "uh":
			alen, err := r.ReadArrayHeader()
			if err != nil {
				return nil, fmt.Errorf("btcvault: signingdata uh header: %w", err)
			}
			sd.UnsignedSigHashes = make([]UnsignedSigHash, alen)
			for j := uint32(0); j < alen; j++ {
				uh, err := decodeUnsignedSigHash(r)
				if err != nil {
					return nil, fmt.Errorf("btcvault: signingdata uh[%d]: %w", j, err)
				}
				sd.UnsignedSigHashes[j] = uh
			}
		default:
			if err := r.Skip(); err != nil {
				return nil, fmt.Errorf("btcvault: signingdata skip %q: %w", key, err)
			}
		}
	}
	return &sd, nil
}

func decodeUnsignedSigHash(r *msgp.Reader) (UnsignedSigHash, error) {
	var uh UnsignedSigHash
	nfields, err := r.ReadMapHeader()
	if err != nil {
		return uh, err
	}
	for i := uint32(0); i < nfields; i++ {
		key, err := r.ReadString()
		if err != nil {
			return uh, err
		}
		switch key {
		case "i":
			uh.Index, err = r.ReadUint32()
		case "hs":
			uh.SigHash, err = r.ReadBytes(nil)
		case "ws":
			uh.WitnessScript, err = r.ReadBytes(nil)
		case "am":
			uh.Amount, err = r.ReadInt64()
			uh.HasAmount = err == nil
		default:
			err = r.Skip()
		}
		if err != nil {
			return uh, err
		}
	}
	return uh, nil
}
