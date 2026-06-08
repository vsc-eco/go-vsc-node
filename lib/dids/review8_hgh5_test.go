package dids_test

// review8 HG-H5 — BtcDID.Verify accepted high-S (malleated) signatures.
//
// A 65-byte BIP-137 compact signature is [header | R(32) | S(32)]. RecoverCompact
// recovers the same pubkey from both S and N-S (with the recovery id flipped),
// so a canonical low-S signature and its high-S twin BOTH verify for the same
// (key, message) — two distinct valid signature strings, i.e. malleability that
// defeats any signature-string-based dedup. The fix rejects S > N/2.

import (
	"encoding/base64"
	"math/big"
	"testing"

	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/require"
)

func TestReview8_HGH5_BtcDIDRejectsHighSMalleability(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)
	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("hg-h5 malleability"))

	// Canonical low-S signature (SignCompact always produces low-S, compressed).
	lowSig := signBIP137(privKey, block.Cid().String(), true)
	ok, err := did.Verify(block, lowSig)
	require.NoError(t, err)
	require.True(t, ok, "canonical low-S signature must verify")

	// Malleate to the high-S twin: S' = N - S, recovery id flipped.
	raw, err := base64.StdEncoding.DecodeString(lowSig)
	require.NoError(t, err)
	require.Len(t, raw, 65)
	N := btcec.S256().Params().N
	S := new(big.Int).SetBytes(raw[33:65])
	require.True(t, S.Cmp(new(big.Int).Rsh(N, 1)) <= 0, "SignCompact should be low-S")
	Sneg := new(big.Int).Sub(N, S) // high-S form

	recid := (raw[0] - 27) & 3
	compressed := (raw[0] - 27) >= 4
	newHeader := byte(27 + (recid ^ 1))
	if compressed {
		newHeader += 4
	}
	mal := make([]byte, 65)
	mal[0] = newHeader
	copy(mal[1:33], raw[1:33])
	copy(mal[33:65], Sneg.FillBytes(make([]byte, 32)))
	highSig := base64.StdEncoding.EncodeToString(mal)

	require.NotEqual(t, lowSig, highSig, "malleated signature must be a distinct string")

	// The high-S twin must now be REJECTED (it was accepted pre-fix — verified
	// by temporarily removing the low-S guard).
	ok, _ = did.Verify(block, highSig)
	require.False(t, ok, "HG-H5: high-S malleated signature must be rejected")
}
