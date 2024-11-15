package dids_test

import (
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
	blst "github.com/supranational/blst/bindings/go"
)

// gens a random BlsDID and BlsPrivKey using a seed
func genRandomBlsDIDAndBlstSecretKeyWithSeed(seed [32]byte) (dids.BlsDID, *dids.BlsPrivKey, error) {
	// gens a priv key and its corresponding pub key using the provided seed
	privKey := blst.KeyGen(seed[:])
	pubKey := new(dids.BlsPubKey).From(privKey)

	// gens the BlsDID from the pub key
	did, err := dids.NewBlsDID(pubKey)
	if err != nil {
		return "", nil, err
	}

	return did, privKey, nil
}

// the full flow of using a BLS circuit is:
//  1. create a circuit generator
//  2. list in the circuit generator the members of the circuit who need to sign it
//  3. generate a partial circuit once all need-to-sign members are listed
//  4. let each member sign the partial circuit with block message and add/verify their signatures to the partial circuit
//  5. once all members have signed, and their signatures are all valid and match the required list of members that had to sign,
//     finalize the circuit to create a BLS circuit that we can then verify and use
func TestFullCircuitFlow(t *testing.T) {
	// use predefined seeds for determinism
	var seed1 [32]byte
	copy(seed1[:], []byte("test_seed_1_101234567"))

	var seed2 [32]byte
	copy(seed2[:], []byte("test_seed_2_98765432876543"))

	// gen DIDs and their corresponding public keys and secret keys for signing
	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(privKey1)
	assert.NoError(t, err)

	did2, privKey2, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
	assert.NoError(t, err)
	provider2, err := dids.NewBlsProvider(privKey2)
	assert.NoError(t, err)

	// create a dummy block for testing purposes
	block := blocks.NewBlock([]byte("test abc 123"))

	// inits a new BlsCircuitGenerator with two members
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})

	// gens a partial circuit
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// sign the block with both providers
	sig1, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	sig2, err := provider2.Sign(block.Cid())
	assert.NoError(t, err)

	// add and verify the first member's signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
	assert.NoError(t, err)

	// add and verify the second member's signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account2", DID: did2}, sig2)
	assert.NoError(t, err)

	// finalize the circuit after both members have signed
	finalCircuit, err := partialCircuit.Finalize()
	assert.NoError(t, err)

	// now call Verify to ensure all sigs valid
	verified, includedDIDsVerified, err := finalCircuit.Verify()
	assert.NoError(t, err)
	assert.True(t, verified)

	// ensure included DIDs match
	assert.Equal(t, finalCircuit.IncludedDIDs(), includedDIDsVerified)
}

func TestInvalidSignature(t *testing.T) {
	// use a predefined seed
	var seed1 [32]byte
	copy(seed1[:], []byte("test_seed_3_85610963"))

	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(privKey1)
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("hello there"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{{Account: "account1", DID: did1}})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// gens a valid signature and tamper it to create an invalid signature
	sig, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	invalidSig := sig[:len(sig)-1] + "SOME_INVALID_SIG_SUFFIX"

	// add and verify the invalid signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, invalidSig)
	// should error out
	assert.Error(t, err)
}

func TestWrongPublicKey(t *testing.T) {
	// use predefined seeds
	var seed1 [32]byte
	copy(seed1[:], []byte("test_seed_4_209385723"))

	var seed2 [32]byte
	copy(seed2[:], []byte("test_seed_4_189263402963"))

	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(privKey1)
	assert.NoError(t, err)

	did2, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("foo bar"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{{Account: "account1", DID: did1}})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	sig, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)

	// use wrong DID for verification
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did2}, sig)
	// should error out
	assert.Error(t, err)
}

func TestNotAllMembersSigned(t *testing.T) {
	// use predefined seeds
	var seed1 [32]byte
	copy(seed1[:], []byte("test_seed_5_46948173"))

	var seed2 [32]byte
	copy(seed2[:], []byte("test_seed_5_5897398793"))

	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(privKey1)
	assert.NoError(t, err)

	did2, _, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("hello world"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// add and verify only one signature
	sig1, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
	assert.NoError(t, err)

	// try finalizing without all signatures
	_, err = partialCircuit.Finalize()
	assert.Error(t, err)
}
func TestSerializeDeserialize(t *testing.T) {
	// use predefined seeds
	var seed1 [32]byte
	copy(seed1[:], []byte("test_seed_6_62526935846259348"))

	var seed2 [32]byte
	copy(seed2[:], []byte("test_seed_6_800822431434"))

	// generate DIDs and keys
	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(privKey1)
	assert.NoError(t, err)

	did2, privKey2, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
	assert.NoError(t, err)
	provider2, err := dids.NewBlsProvider(privKey2)
	assert.NoError(t, err)

	// create a block
	block := blocks.NewBlock([]byte("test block data"))

	// create the circuit generator with two members
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})

	// gen partial circuit
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// sign the block with both providers
	sig1, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	sig2, err := provider2.Sign(block.Cid())
	assert.NoError(t, err)

	// add and verify the first signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
	assert.NoError(t, err)

	// add and verify the second signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account2", DID: did2}, sig2)
	assert.NoError(t, err)

	// finalize the circuit
	finalCircuit, err := partialCircuit.Finalize()
	assert.NoError(t, err)

	// serialize the circuit (includes CID and bit vector)
	serializedCircuit, err := finalCircuit.Serialize()
	assert.NoError(t, err)

	// deserialize the circuit to get the AggregateDID
	keyset := generator.CircuitMap()
	blsCircuit, err := dids.DeserializeBlsCircuit(*serializedCircuit, keyset, block.Cid())
	assert.NoError(t, err)

	// verify the aggregated signature and included DIDs
	verified, includedDIDs, err := blsCircuit.Verify()
	assert.NoError(t, err)
	assert.True(t, verified)

	// verify that the included DIDs match
	assert.Equal(t, includedDIDs, finalCircuit.IncludedDIDs())
}
