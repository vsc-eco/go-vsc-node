package dids_test

import (
	"testing"

	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B13, demonstrated rather than argued: a committee holding the same BLS key at
// two seats produces an aggregate that VERIFIES, from a SINGLE signature, with
// both of that key's bits set.
//
// The mechanism is structural. aggregateSignaturesFrom walks the keyset by INDEX
// while signatures are keyed by DID, so one signature matches the duplicated DID
// at every index holding it: it enters the slice twice, both bits are set, and
// bls.Aggregate is a plain EC point sum with no duplicate check. Verification
// walks the same keyset, so the pubkeys sum to 2P against an aggregate of 2S, and
// by bilinearity e(2P, H(m)) == e(P, H(m))^2 == e(G, S)^2 == e(G, 2S).
//
// Two consequences worth stating plainly. The attacker needs neither the copied
// private key nor a second signature — only a seat carrying a copy of the public
// key. And the honest validator signs once, normally, and never learns that its
// signature was counted twice.
//
// This is why the duplicate cannot be caught at the signature check, and has to
// be caught in every weight fold instead.
func TestDuplicateKeyInKeysetForgesAVerifyingAggregate(t *testing.T) {
	var honestSeed, otherSeed [32]byte
	copy(honestSeed[:], []byte("b13_honest_validator_seed_00001"))
	copy(otherSeed[:], []byte("b13_other_validator_seed_000002"))

	honestDID, honestKey, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(honestSeed)
	require.NoError(t, err)
	otherDID, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(otherSeed)
	require.NoError(t, err)
	honestProvider, err := dids.NewBlsProvider(honestKey)
	require.NoError(t, err)

	block := blocks.NewBlock([]byte("b13 duplicate key"))

	// The committee. Seat 1 is the honest validator; seat 2 is an attacker who
	// simply copied the honest validator's PUBLIC key into their own seat.
	// Seat 0 is an unrelated member, present so the committee is not degenerate.
	circuit, err := dids.NewBlsCircuitGenerator([]dids.Member{
		otherDID,
		honestDID,
		honestDID, // the copy
	}).Generate(block.Cid())
	require.NoError(t, err)

	// Exactly ONE signature is produced, by the honest validator, over the block
	// it was always going to sign.
	sig, err := honestProvider.Sign(block.Cid())
	require.NoError(t, err)
	added, err := circuit.AddAndVerify(honestDID, sig)
	require.NoError(t, err)
	require.True(t, added, "the honest signature must be accepted")

	final, err := circuit.Finalize()
	require.NoError(t, err)

	verified, included, err := final.Verify()
	require.NoError(t, err)
	assert.True(t, verified,
		"THE FINDING: an aggregate over a keyset containing a duplicated key verifies "+
			"from a single signature — so the signature check cannot catch the duplicate")

	// Both of the duplicated key's bits are set. This is what every weight fold
	// walking the bit vector then double-counts.
	bv := final.RawBitVector()
	assert.Equal(t, uint(0), bv.Bit(0), "the non-signing member's bit must be clear")
	assert.Equal(t, uint(1), bv.Bit(1), "the honest seat's bit is set")
	assert.Equal(t, uint(1), bv.Bit(2),
		"and so is the COPY's bit — from the same single signature, which is the "+
			"whole mechanism")

	// The duplicate propagates into the reported signer list as well: IncludedDIDs
	// is derived from the bit vector, so ONE signature is reported as TWO signers
	// under the same DID. Every consumer of this list has to dedup it, which is
	// exactly what FoldSignedWeight's `counted` set does.
	assert.Len(t, included, 2,
		"the copy is reported as a second signer — pinned because callers must not "+
			"treat this list as already deduplicated")
	assert.Equal(t, included[0], included[1],
		"both entries are the same DID: one key, counted twice")

	// And the canonical fold refuses to be fooled: three seats, the duplicate
	// collapsed, one seat's weight credited out of two real seats — short of 2/3.
	memberKeys := []string{otherDID.String(), honestDID.String(), honestDID.String()}
	weights := []uint64{10, 10, 80}
	signed, total, ok := dids.FoldSignedWeight(memberKeys, weights, included)
	require.True(t, ok)
	assert.Equal(t, uint64(10), signed,
		"the duplicated key is ONE signer and gets ONE seat's weight — not the 80 "+
			"the attacker self-staked, and not 90")
	assert.Equal(t, uint64(20), total, "the phantom seat is absent from the total too")
	assert.False(t, dids.TwoThirdsMet(signed, total),
		"one of two real seats is not a two-thirds supermajority")
}
