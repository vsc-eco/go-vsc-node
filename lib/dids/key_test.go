package dids_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
)

type dummyBlock struct {
	data []byte
}

// raw data, to fulfil interface
func (b *dummyBlock) RawData() []byte {
	return b.data
}

// cid, to fulfil interface
func (b *dummyBlock) Cid() cid.Cid {
	hash := sha256.Sum256(b.data)
	multihash, _ := mh.Encode(hash[:], mh.SHA2_256)
	return cid.NewCidV0(multihash)
}

// create a dummy block
func createDummyBlock(data []byte) blocks.Block {
	return &dummyBlock{data: data}
}

// loggable, to fulfil interface
func (b *dummyBlock) Loggable() map[string]interface{} {
	return map[string]interface{}{
		"cid": b.Cid().String(),
	}
}

// string, to fulfil interface
func (b *dummyBlock) String() string {
	return b.Cid().String()
}

func TestSignVerify(t *testing.T) {
	// gen a keypair
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	assert.NotNil(t, pubKey)

	provider := dids.NewKeyProvider(privKey)

	// create original block
	block := createDummyBlock([]byte("dummy data"))
	assert.NotNil(t, block)

	jws1, err := provider.Sign(block)
	assert.Nil(t, err)

	// create DID from the pub key
	did, err := dids.NewKeyDID(pubKey)
	assert.Nil(t, err)

	// verify the original block with its sig
	valid, err := did.Verify(block, jws1)
	assert.Nil(t, err)
	assert.True(t, valid)

	// create modified/incorrect block with different content
	modifiedBlock := createDummyBlock([]byte("modified data 123 456"))
	valid, err = did.Verify(modifiedBlock, jws1)
	assert.NotNil(t, err)
	assert.False(t, valid)
}
