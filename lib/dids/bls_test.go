package dids_test

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"

	// blst "github.com/supranational/blst/bindings/go"

	bls "github.com/consensys/gnark-crypto/ecc/bls12-381"
	ethBls "github.com/protolambda/bls12-381-util"
)

// gens a random BlsDID and BlsPrivKey using a seed
func genRandomBlsDIDAndBlstSecretKeyWithSeed(seed [32]byte) (dids.BlsDID, *dids.BlsPrivKey, error) {
	// gens a priv key and its corresponding pub key using the provided seed
	// privKey := blst.KeyGen(seed[:])
	privKey := dids.BlsPrivKey{}
	privKey.Deserialize(&seed)
	// pubKey := new(dids.BlsPubKey).From(privKey.)
	pubKey, _ := ethBls.SkToPk(&privKey)

	// gens the BlsDID from the pub key
	did, err := dids.NewBlsDID(pubKey)
	if err != nil {
		return "", nil, err
	}

	return did, &privKey, nil
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
		did1,
		did2,
	})

	// gens a partial circuit
	partialCircuit, err := generator.Generate(block.Cid())

	fmt.Println("BROKEN", err)
	assert.NoError(t, err)

	// sign the block with both providers
	sig1, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	sig2, err := provider2.Sign(block.Cid())
	assert.NoError(t, err)

	// add and verify the first member's signature
	added, err := partialCircuit.AddAndVerify(did1, sig1)
	assert.True(t, added, "did1 should be added with sig1")
	assert.NoError(t, err)

	// add and verify the second member's signature
	added, err = partialCircuit.AddAndVerify(did2, sig2)
	assert.True(t, added, "did2 should be added with sig2")
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
	generator := dids.NewBlsCircuitGenerator([]dids.Member{did1})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// gens a valid signature and tamper it to create an invalid signature
	sig, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	invalidSig := sig[:len(sig)-1] + "SOME_INVALID_SIG_SUFFIX"

	// add and verify the invalid signature
	added, err := partialCircuit.AddAndVerify(did1, invalidSig)
	assert.False(t, added, "did1 should not be added with an invalid sig")
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
	generator := dids.NewBlsCircuitGenerator([]dids.Member{did1})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	sig, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)

	// use wrong DID for verification
	added, err := partialCircuit.AddAndVerify(did2, sig)
	assert.False(t, added, "did2 did not sign sig and can not be used to add it")
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
		did1,
		did2,
	})
	partialCircuit, err := generator.Generate(block.Cid())
	assert.NoError(t, err)

	// add and verify only one signature
	sig1, err := provider1.Sign(block.Cid())
	assert.NoError(t, err)
	added, err := partialCircuit.AddAndVerify(did1, sig1)
	assert.True(t, added, "did1 should be added with sig1")
	assert.NoError(t, err)

	// try finalizing without all signatures
	_, err = partialCircuit.Finalize()
	assert.NoError(t, err)
}

func Test(t *testing.T) {
	didList := []dids.BlsDID{
		dids.BlsDID("did:key:z3tEFFFAKtRc8B9oXCM7LiH4xYKW4zNrKcE2p1S6PzJdcz5icZBCj4akv6w5feJ6mQhopG"),
		dids.BlsDID("did:key:z3tEGiumPhsgaGjq997DbadLP8YhbNVWzvzaiKXajqqaZ9KRs1o4xmfHX9SEAZiLixPx1y"),
		dids.BlsDID("did:key:z3tEFz3LYFSg6XhjTxXvPnrtQyEtAfsx3fD6pUB1KSs434XsLeNTFkTVyPUYwz4MvpbRFf"),
		dids.BlsDID("did:key:z3tEGcSWJNbUw6GxijVdRG8nQtB32BRGbGSkEusGEFKfmPPDz2bxR4ktyyfV8Pn1WeCMCj"),
		dids.BlsDID("did:key:z3tEEt3vHLEEjab3uK8Ss4BnFLZ2LvH3RzMn5bL54eTQbHPEZhxfmVgyPW1aTL2b43xM1N"),
		dids.BlsDID("did:key:z3tEFJMGRpMsmQWB7GkxV2zYQ4YRft8jsiUuvqRvy5PG4PwxDmnqpLHC9bPJb9TxoeYXpL"),
		dids.BlsDID("did:key:z3tEGKWdYE7AwmDjT9Nuq9pJUMvZ3FXToHQ1swJv3xNgPPpisBLXHRfyymoDVRB3AvpJPu"),
		dids.BlsDID("did:key:z3tEF7PQgndGv3t9agcB5F5ELhX8yTh7iYfKJAFdKnoEqJsPf8EdnRbhVSjLZJBRAY6NKM"),
		dids.BlsDID("did:key:z3tEGL89VBKtsD8kNEJqtg96Z25v8cfMxa7gx6qKSL73Q31g8qVE6KzDgBWEFubh9iDqxJ"),
		dids.BlsDID("did:key:z3tEGgxgA2iDyw95AyefwY62d1mJmPYry7j2MaicxnmSafjpJHuXBUxGArUDcP6NcAxXY6"),
		dids.BlsDID("did:key:z3tEFVF1BeYPp5PU7erY4Bi1kHQFJVpKqrwJ83qSAgzTTStNV8SFT1QmmBxpVyPoKKRX3q"),
		dids.BlsDID("did:key:z3tEGGXL2zcKrnFh97T7heMRciQNVGRBaqe1TUqw8t3EfXmuhBcEH7gSDncDYML11ZGbQL"),
		dids.BlsDID("did:key:z3tEFXunHAWZvxGRyQbXuCutZ3xebRgVZML7N7t1kMzyppEjBwpyrFUSn8bNE3gmc1y7Qp"),
		dids.BlsDID("did:key:z3tEGYbX4TiY7rmQa6fPNH6bxXXBfc1Kz1eQytrUVMkjWEXbofnFyG5GMwJWQWP9J3E8C4"),
		dids.BlsDID("did:key:z3tEFzzA26fjP72ZY6v1NLgrs9ruErHk7nmYCE2HHLmWQhU4myLuV8GxpK6XBVBJ1udkzh"),
		dids.BlsDID("did:key:z3tEGWJ1estpynDoexoh4eFiQEro3ydxPiWMq2HNNQ6h1owvb49AQ9fEniYywyyLod8YFd"),
		dids.BlsDID("did:key:z3tEFUsQiNndov8ysxyzXEZggBe7PyBdP1DswJ2nuJcCFtxpgE9NfZ2ek61SJEszE2Nvmw"),
		dids.BlsDID("did:key:z3tEG4yj2v1vMpooaa686DNE3BcPyQumfze4v9poY4ucx2kYLGJuPYtyiWQjhayk4oU761"),
	}

	keyset := []dids.BlsDID{dids.BlsDID("did:key:z3tEFFFAKtRc8B9oXCM7LiH4xYKW4zNrKcE2p1S6PzJdcz5icZBCj4akv6w5feJ6mQhopG"),
		dids.BlsDID("did:key:z3tEGiumPhsgaGjq997DbadLP8YhbNVWzvzaiKXajqqaZ9KRs1o4xmfHX9SEAZiLixPx1y"),
		dids.BlsDID("did:key:z3tEFzX6HusCWzwjdf3LM1uAny8KX6KuereChqF8eoEkENvKX6H96DBe45avev6XrQQMMs"),
		dids.BlsDID("did:key:z3tEFz3LYFSg6XhjTxXvPnrtQyEtAfsx3fD6pUB1KSs434XsLeNTFkTVyPUYwz4MvpbRFf"),
		dids.BlsDID("did:key:z3tEGcSWJNbUw6GxijVdRG8nQtB32BRGbGSkEusGEFKfmPPDz2bxR4ktyyfV8Pn1WeCMCj"),
		dids.BlsDID("did:key:z3tEF69iEFo7Z4SznE6QA5YQnZxQg4JHBod7HpXWKYjzCMvS91xuxhNspr8jtA78nac3ua"),
		dids.BlsDID("did:key:z3tEGeiVx2CoifcjYYiFLmTbxLHWPeteZhnbN2AWuZHscagPFWxCyo5MY3g1Hf9FNe4kik"),
		dids.BlsDID("did:key:z3tEEt3vHLEEjab3uK8Ss4BnFLZ2LvH3RzMn5bL54eTQbHPEZhxfmVgyPW1aTL2b43xM1N"),
		dids.BlsDID("did:key:z3tEFJMGRpMsmQWB7GkxV2zYQ4YRft8jsiUuvqRvy5PG4PwxDmnqpLHC9bPJb9TxoeYXpL"),
		dids.BlsDID("did:key:z3tEGKWdYE7AwmDjT9Nuq9pJUMvZ3FXToHQ1swJv3xNgPPpisBLXHRfyymoDVRB3AvpJPu"),
		dids.BlsDID("did:key:z3tEF7PQgndGv3t9agcB5F5ELhX8yTh7iYfKJAFdKnoEqJsPf8EdnRbhVSjLZJBRAY6NKM"),
		dids.BlsDID("did:key:z3tEGHYuGW4bRo1fjUwTmkGWwn2Rv7GTPB2BCaygjke1H7xPughqELuiNtStEphhQrVyXG"),
		dids.BlsDID("did:key:z3tEFsGef9ZDg5SWqgdKL48nkt5mC7XZnoXHQpeDgZCCUL5d4VVQZ9aaStUPmyFYepzHnd"),
		dids.BlsDID("did:key:z3tEGL89VBKtsD8kNEJqtg96Z25v8cfMxa7gx6qKSL73Q31g8qVE6KzDgBWEFubh9iDqxJ"),
		dids.BlsDID("did:key:z3tEGgxgA2iDyw95AyefwY62d1mJmPYry7j2MaicxnmSafjpJHuXBUxGArUDcP6NcAxXY6"),
		dids.BlsDID("did:key:z3tEFVF1BeYPp5PU7erY4Bi1kHQFJVpKqrwJ83qSAgzTTStNV8SFT1QmmBxpVyPoKKRX3q"),
		dids.BlsDID("did:key:z3tEGGXL2zcKrnFh97T7heMRciQNVGRBaqe1TUqw8t3EfXmuhBcEH7gSDncDYML11ZGbQL"),
		dids.BlsDID("did:key:z3tEFXunHAWZvxGRyQbXuCutZ3xebRgVZML7N7t1kMzyppEjBwpyrFUSn8bNE3gmc1y7Qp"),
		dids.BlsDID("did:key:z3tEGCcFc3TpqREEURPt3o73q6vEW2t6tDRV9YjhdTKKYzZf7eFMeqfNZudsQEUrqVZRLv"),
		dids.BlsDID("did:key:z3tEGYbX4TiY7rmQa6fPNH6bxXXBfc1Kz1eQytrUVMkjWEXbofnFyG5GMwJWQWP9J3E8C4"),
		dids.BlsDID("did:key:z3tEFzzA26fjP72ZY6v1NLgrs9ruErHk7nmYCE2HHLmWQhU4myLuV8GxpK6XBVBJ1udkzh"),
		dids.BlsDID("did:key:z3tEGWJ1estpynDoexoh4eFiQEro3ydxPiWMq2HNNQ6h1owvb49AQ9fEniYywyyLod8YFd"),
		dids.BlsDID("did:key:z3tEGY6FyKiJcvEgBZRspUjLk8n4jUqWG7fPkaFvjUnnoxCGJT7r8Ac38HdFZKAc1NHNmv"),
		dids.BlsDID("did:key:z3tEGFbLo8toPnWJgLaAto6McP9S3XUbfZEavZsgfLsZnbC5nK7zRETJSwMjFJZVmNyjC3"),
		dids.BlsDID("did:key:z3tEFUsQiNndov8ysxyzXEZggBe7PyBdP1DswJ2nuJcCFtxpgE9NfZ2ek61SJEszE2Nvmw"),
		dids.BlsDID("did:key:z3tEG4yj2v1vMpooaa686DNE3BcPyQumfze4v9poY4ucx2kYLGJuPYtyiWQjhayk4oU761"),
	}

	sigBytes := []byte{
		163,
		109,
		222,
		124,
		11,
		42,
		190,
		28,
		243,
		92,
		164,
		103,
		54,
		231,
		142,
		255,
		171,
		76,
		124,
		83,
		183,
		33,
		178,
		33,
		8,
		184,
		113,
		175,
		1,
		180,
		75,
		251,
		51,
		140,
		251,
		111,
		32,
		159,
		188,
		33,
		69,
		124,
		230,
		74,
		139,
		239,
		217,
		247,
		5,
		19,
		122,
		196,
		188,
		219,
		64,
		252,
		35,
		99,
		44,
		158,
		124,
		119,
		143,
		123,
		70,
		15,
		22,
		173,
		212,
		91,
		32,
		122,
		63,
		192,
		19,
		203,
		215,
		139,
		66,
		152,
		188,
		244,
		55,
		14,
		125,
		109,
		138,
		168,
		70,
		121,
		158,
		145,
		12,
		236,
		163,
		97,
	}

	s := dids.SerializedCircuit{
		Signature: "o23efAsqvhzzXKRnNueO_6tMfFO3IbIhCLhxrwG0S_szjPtvIJ-8IUV85kqL79n3BRN6xLzbQPwjYyyefHePe0YPFq3UWyB6P8ATy9eLQpi89DcOfW2KqEZ5npEM7KNh",
		BitVector: "Azvnmw",
	}

	sig, err := base64.RawURLEncoding.DecodeString(s.Signature)
	assert.NoError(t, err)
	assert.Equal(t, sigBytes, sig)

	circuit, err := dids.DeserializeBlsCircuit(s, keyset, cid.MustParse("bafyreihmvmys5xp4mdy2vo73hhefydkbhzwj3tf4t5exzp6rrognjdql4q"))

	assert.NoError(t, err)
	verified, incluedDids, err := circuit.Verify()
	assert.NoError(t, err)
	assert.Equal(t, didList, incluedDids)
	assert.True(t, verified)
}

// func TestSerializeDeserialize(t *testing.T) {
// 	// use predefined seeds
// 	var seed1 [32]byte
// 	copy(seed1[:], []byte("test_seed_6_62526935846259348"))

// 	var seed2 [32]byte
// 	copy(seed2[:], []byte("test_seed_6_800822431434"))

// 	// generate DIDs and keys
// 	did1, privKey1, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed1)
// 	assert.NoError(t, err)
// 	provider1, err := dids.NewBlsProvider(privKey1)
// 	assert.NoError(t, err)

// 	did2, privKey2, err := genRandomBlsDIDAndBlstSecretKeyWithSeed(seed2)
// 	assert.NoError(t, err)
// 	provider2, err := dids.NewBlsProvider(privKey2)
// 	assert.NoError(t, err)

// 	// create a block
// 	block := blocks.NewBlock([]byte("test block data"))

// 	// create the circuit generator with two members
// 	generator := dids.NewBlsCircuitGenerator([]dids.Member{
// 		{Account: "account1", DID: did1},
// 		{Account: "account2", DID: did2},
// 	})

// 	// gen partial circuit
// 	partialCircuit, err := generator.Generate(block.Cid())
// 	assert.NoError(t, err)

// 	// sign the block with both providers
// 	sig1, err := provider1.Sign(block.Cid())
// 	assert.NoError(t, err)
// 	sig2, err := provider2.Sign(block.Cid())
// 	assert.NoError(t, err)

// 	// add and verify the first signature
// 	err = partialCircuit.AddAndVerify(dids.Member{Account: "account1", DID: did1}, sig1)
// 	assert.NoError(t, err)

// 	// add and verify the second signature
// 	err = partialCircuit.AddAndVerify(dids.Member{Account: "account2", DID: did2}, sig2)
// 	assert.NoError(t, err)

// 	// finalize the circuit
// 	finalCircuit, err := partialCircuit.Finalize()
// 	assert.NoError(t, err)

// 	// serialize the circuit (includes CID and bit vector)
// 	serializedCircuit, err := finalCircuit.Serialize()
// 	assert.NoError(t, err)

// 	// deserialize the circuit to get the AggregateDID
// 	keyset := generator.CircuitMap()
// 	aggDID, err := dids.DeserializeBlsCircuit(serializedCircuit, keyset)
// 	assert.NoError(t, err)

// 	// use aggDID to verify the original block
// 	// get the aggregated signature from the circuit
// 	sig, err := finalCircuit.AggregatedSignature()
// 	assert.NoError(t, err)

// 	// verify the signature using aggDID
// 	verified, err := aggDID.AggPubKey.Verify(finalCircuit.Msg(), sig)
// 	assert.NoError(t, err)
// 	assert.True(t, verified)

// 	// verify that the included DIDs match
// 	assert.Equal(t, aggDID.IncludedDIDs, finalCircuit.IncludedDIDs())
// }

func TestBlsGnark(t *testing.T) {
	cidt := cid.MustParse("bafyreihmvmys5xp4mdy2vo73hhefydkbhzwj3tf4t5exzp6rrognjdql4q")
	affine, err := bls.HashToG2(cidt.Bytes(), []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_"))

	bbytes := affine.Bytes()
	fmt.Println(hex.EncodeToString(bbytes[:]), err)
	pubKey := bls.G1Affine{}
	bytesPubKey, _ := hex.DecodeString("95e0ef0109d414147401fd670ba99c6c8490bd982bfe8ad18323195869452dce9c612b5e3a28c1ea4cede3007e354d05")
	fmt.Println(pubKey.Unmarshal(bytesPubKey))

	sigBytes, _ := base64.URLEncoding.DecodeString("o23efAsqvhzzXKRnNueO_6tMfFO3IbIhCLhxrwG0S_szjPtvIJ-8IUV85kqL79n3BRN6xLzbQPwjYyyefHePe0YPFq3UWyB6P8ATy9eLQpi89DcOfW2KqEZ5npEM7KNh")

	e1, _ := bls.Pair([]bls.G1Affine{*pubKey.Neg(&pubKey)}, []bls.G2Affine{affine})
	_, _, g1Gen, _ := bls.Generators()

	g2Sig := bls.G2Affine{}
	fmt.Println(g2Sig.Unmarshal(sigBytes))
	e2, _ := bls.Pair([]bls.G1Affine{g1Gen}, []bls.G2Affine{g2Sig})

	fmt.Println(e2)
	fmt.Println("Verified", e1.Equal(&e2))
	pk := ethBls.Pubkey{}
	pk.Deserialize((*[48]byte)(bytesPubKey))
	sig := ethBls.Signature{}
	sig.Deserialize((*[96]byte)(sigBytes))

	// blstSig := blst.P2Affine{}
	// blstPub := blst.P1Affine{}
	// blstPub.Deserialize(bytesPubKey)
	// blstSig.Deserialize(sigBytes)

	// fmt.Println("verified blst", blstSig.Verify(true, &blstPub, true, cidt.Bytes(), []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_")))
	fmt.Println("verified", ethBls.Verify(&pk, cidt.Bytes(), &sig))
}
