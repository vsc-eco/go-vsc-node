package dids_test

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/crypto"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

// HG-H5 parity for EthDID (BTC: review8_hgh5_test.go). EthDID.Verify recovered
// the key with Ecrecover, which accepts both S and N-S, so a valid signature had
// a second valid string (its high-S twin); and it read the recovery byte without
// checking the length. Now: exactly 65 bytes, low-S only.

// Same real-wallet vector as TestVerifyExternalSigCIDKeyAndBytes (eth_test.go).
var lowSVectorMsg = []byte{
	164, 98, 116, 120, 162, 98, 111, 112, 104, 119, 105, 116,
	104, 100, 114, 97, 119, 103, 112, 97, 121, 108, 111, 97,
	100, 164, 98, 116, 107, 100, 72, 73, 86, 69, 98, 116,
	111, 109, 104, 105, 118, 101, 58, 103, 101, 111, 53, 50,
	114, 101, 121, 100, 102, 114, 111, 109, 120, 59, 100, 105,
	100, 58, 112, 107, 104, 58, 101, 105, 112, 49, 53, 53,
	58, 49, 58, 48, 120, 56, 56, 69, 66, 66, 54, 52,
	67, 50, 54, 52, 65, 70, 102, 49, 48, 49, 52, 49,
	49, 52, 57, 70, 57, 55, 55, 48, 70, 56, 68, 54,
	52, 52, 67, 57, 68, 56, 54, 67, 53, 102, 97, 109,
	111, 117, 110, 116, 1, 99, 95, 95, 116, 102, 118, 115,
	99, 45, 116, 120, 99, 95, 95, 118, 99, 48, 46, 50,
	103, 104, 101, 97, 100, 101, 114, 115, 164, 100, 116, 121,
	112, 101, 1, 101, 110, 111, 110, 99, 101, 16, 103, 105,
	110, 116, 101, 110, 116, 115, 128, 110, 114, 101, 113, 117,
	105, 114, 101, 100, 95, 97, 117, 116, 104, 115, 129, 120,
	59, 100, 105, 100, 58, 112, 107, 104, 58, 101, 105, 112,
	49, 53, 53, 58, 49, 58, 48, 120, 56, 56, 69, 66,
	66, 54, 52, 67, 50, 54, 52, 65, 70, 102, 49, 48,
	49, 52, 49, 49, 52, 57, 70, 57, 55, 55, 48, 70,
	56, 68, 54, 52, 52, 67, 57, 68, 56, 54, 67, 53,
}

const lowSVectorCID = "bafyreiglzf54t4pfhn6du6ez77oi7dseba52yii7hbq77tlpasvjkzfde4"
const lowSVectorSig = "0x1e5c6be56c5a2915958f3b663f0831331111e2b1e0ac3e9aec2f7b497229d0f810014789193da635dc35be1ae6dbd5be5250c0f020b67f9fa2e7abe387f7d88e1c"
const lowSVectorAddr = "0x88EBB64C264AFf10141149F9770F8D644C9D86C5"

func lowSVectorBlock(t *testing.T) blocks.Block {
	t.Helper()
	c, err := cid.Decode(lowSVectorCID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := blocks.NewBlockWithCid(lowSVectorMsg, c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// highSTwin returns the other valid signature for the same message: S' = N - S
// with the recovery bit flipped (V in 27/28 or 0/1 form).
func highSTwin(t *testing.T, sigHex string) string {
	t.Helper()
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHex, "0x"))
	if err != nil || len(sig) != 65 {
		t.Fatalf("bad test signature: %v", err)
	}
	n := crypto.S256().Params().N
	s := new(big.Int).SetBytes(sig[32:64])
	twin := new(big.Int).Sub(n, s).FillBytes(make([]byte, 32))
	v := sig[64] ^ 1 // 0 <-> 1
	if sig[64] >= 27 {
		v = 27 + ((sig[64] - 27) ^ 1) // 27 <-> 28
	}
	out := append(append(append([]byte{}, sig[:32]...), twin...), v)
	return "0x" + hex.EncodeToString(out)
}

func verifyNoPanic(t *testing.T, did dids.EthDID, b blocks.Block, sig string) (ok bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EthDID.Verify panicked on %d-char signature: %v", len(sig), r)
		}
	}()
	return did.Verify(b, sig)
}

func TestReview9_EthDIDRejectsHighSTwin(t *testing.T) {
	b := lowSVectorBlock(t)
	did := dids.NewEthDID(lowSVectorAddr)

	// The real wallet's signature is canonical and still verifies.
	if ok, err := verifyNoPanic(t, did, b, lowSVectorSig); err != nil || !ok {
		t.Fatalf("real-wallet signature no longer verifies: ok=%v err=%v", ok, err)
	}
	// Its high-S twin recovers the same key (so it verified before the fix) and
	// must now be refused.
	twin := highSTwin(t, lowSVectorSig)
	if ok, err := verifyNoPanic(t, did, b, twin); ok || err == nil || !strings.Contains(err.Error(), "high-S") {
		t.Fatalf("high-S twin: ok=%v err=%v, want refused as high-S", ok, err)
	}

	// A fresh signature from the bot's own signer (go-ethereum, canonical) verifies.
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := dids.NewEthProvider(key).Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	keyDID := dids.NewEthDID(crypto.PubkeyToAddress(key.PublicKey).Hex())
	if ok, err := verifyNoPanic(t, keyDID, b, fresh); err != nil || !ok {
		t.Fatalf("EthProvider signature: ok=%v err=%v", ok, err)
	}
	if ok, err := verifyNoPanic(t, keyDID, b, highSTwin(t, fresh)); ok || err == nil {
		t.Fatalf("EthProvider signature's high-S twin: ok=%v err=%v, want refused", ok, err)
	}
}

func TestReview9_EthDIDRejectsWrongLengthWithoutPanic(t *testing.T) {
	b := lowSVectorBlock(t)
	did := dids.NewEthDID(lowSVectorAddr)
	full := strings.TrimPrefix(lowSVectorSig, "0x")
	for _, s := range []string{"0x", "0x" + full[:128], "0x" + full[:2], "0x" + full + "00"} {
		if ok, err := verifyNoPanic(t, did, b, s); ok || err == nil {
			t.Fatalf("%d-byte signature: ok=%v err=%v, want an error", (len(s)-2)/2, ok, err)
		}
	}
}
