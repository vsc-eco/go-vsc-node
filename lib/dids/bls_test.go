package dids_test

import (
	"crypto/rand"
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
	blst "github.com/supranational/blst/bindings/go"
)

func genRandomBlsDIDAndBlstSecretKey() (dids.BlsDID, *blst.SecretKey, error) {
	var seed [32]byte
	_, err := rand.Read(seed[:])
	if err != nil {
		return "", nil, err
	}

	// generate a priv key and its corresponding public key using the random seed
	privKey := blst.KeyGen(seed[:])
	pubKey := new(blst.P1Affine).From(privKey)

	// generate the BlsDID from the pub key
	did, err := dids.NewBlsDID(pubKey)
	if err != nil {
		return "", nil, err
	}

	return did, privKey, nil
}

func TestFullCircuitFlow(t *testing.T) {
	// generate DIDs and their corresponding pub keys and secret keys for signing
	did1, secretKey1, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(secretKey1)
	assert.NoError(t, err)

	did2, secretKey2, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider2, err := dids.NewBlsProvider(secretKey2)
	assert.NoError(t, err)

	// create a dummy block for testing purposes
	block := blocks.NewBlock([]byte("test abc 123"))

	// initialize a new BlsCircuitGenerator with two members
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})

	// generate a partial circuit
	partialCircuit, err := generator.Generate(block)
	assert.NoError(t, err)

	sig1, err := provider1.Sign(block)
	assert.NoError(t, err)

	sig2, err := provider2.Sign(block)
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

	// verify the aggregated signature in the final circuit
	verified, err := finalCircuit.Verify()
	assert.NoError(t, err)
	assert.True(t, verified)

	// verify the pub keys of the members match the final aggregated pub key in the circuit
	didsToVerify := []dids.BlsDID{did1, did2}
	verifiedPubKeys, err := finalCircuit.VerifyDIDs(didsToVerify)
	assert.NoError(t, err)
	assert.True(t, verifiedPubKeys)
}

func TestInvalidSignature(t *testing.T) {
	did1, secretKey1, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(secretKey1)
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("hello there"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{{Account: "account1", DID: did1}})
	partialCircuit, err := generator.Generate(block)
	assert.NoError(t, err)

	// generate valid signature and tamper it to create an invalid signature
	sig, err := provider1.Sign(block)
	assert.NoError(t, err)
	invalidSig := sig[:len(sig)-1] + "X"

	// add and verify the invalid signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, invalidSig)
	assert.Error(t, err)
}

func TestWrongPublicKey(t *testing.T) {
	did1, secretKey1, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(secretKey1)
	assert.NoError(t, err)

	did2, _, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("foo bar"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{{Account: "account1", DID: did1}})
	partialCircuit, err := generator.Generate(block)
	assert.NoError(t, err)

	sig, err := provider1.Sign(block)
	assert.NoError(t, err)

	// use wrong DID for verification
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did2}, sig)
	assert.Error(t, err)
}

func TestNotAllMembersSigned(t *testing.T) {
	did1, secretKey1, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(secretKey1)
	assert.NoError(t, err)

	did2, _, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)

	block := blocks.NewBlock([]byte("hello world"))
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})
	partialCircuit, err := generator.Generate(block)
	assert.NoError(t, err)

	// add and verify only one signature
	sig1, err := provider1.Sign(block)
	assert.NoError(t, err)
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
	assert.NoError(t, err)

	// try finalizing without all signatures
	_, err = partialCircuit.Finalize()
	assert.Error(t, err)
}

func TestSerializeDeserialize(t *testing.T) {
	// gen random DID and key
	did1, secretKey1, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider1, err := dids.NewBlsProvider(secretKey1)
	assert.NoError(t, err)

	// gen another random DID without key
	did2, secretKey2, err := genRandomBlsDIDAndBlstSecretKey()
	assert.NoError(t, err)
	provider2, err := dids.NewBlsProvider(secretKey2)
	assert.NoError(t, err)

	// create a block
	block := blocks.NewBlock([]byte("test block data"))

	// create the circuit generator with two members
	generator := dids.NewBlsCircuitGenerator([]dids.Member{
		{Account: "account1", DID: did1},
		{Account: "account2", DID: did2},
	})

	// gen partial circuit
	partialCircuit, err := generator.Generate(block)
	assert.NoError(t, err)

	// sign the block with the first provider and add the signature to the partial circuit
	sig1, err := provider1.Sign(block)
	assert.NoError(t, err)

	sig2, err := provider2.Sign(block)
	assert.NoError(t, err)

	// add and verify the first signature
	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
	assert.NoError(t, err)

	err = partialCircuit.AddAndVerify(dids.Member{Account: "account2", DID: did2}, sig2)
	assert.NoError(t, err)

	// get the current circuit map
	circuitMap := partialCircuit.CircuitMap()

	// finalize the circuit
	finalCircuit, err := partialCircuit.Finalize()
	assert.NoError(t, err)

	// serialize and deserialize the circuit
	serializedCircuit, err := finalCircuit.Serialize(circuitMap)
	assert.NoError(t, err)
	deserializedCircuit, err := dids.DeserializeBlsCircuit(serializedCircuit, []string{did1.String(), did2.String()})
	assert.NoError(t, err)

	// ensure that the deserialized circuit matches the original circuit; it should pass verification
	verified, err := deserializedCircuit.Verify()
	assert.NoError(t, err)
	assert.True(t, verified)
}
