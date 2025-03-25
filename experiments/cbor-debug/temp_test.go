package cbor_debug_test

import (
	"testing"

	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/stretchr/testify/assert"
)

func TestS(t *testing.T) {
	a := struct {
		Name string
		Cid  cid.Cid
	}{Name: "test", Cid: cid.MustParse("bafyreiggfvzabljn7izap6w252mgcaezcqdnnfemgkwth5cvtzigutxvgi")}

	res, err := cbornode.Encode(a)
	assert.Error(t, err)
	t.Log(res)
}

func TestStructToCbor(t *testing.T) {
	type A struct {
		Name string  `refmt:"name"`
		Cid  cid.Cid `refmt:"cid"`
	}
	a := A{Name: "test", Cid: cid.MustParse("bafyreiggfvzabljn7izap6w252mgcaezcqdnnfemgkwth5cvtzigutxvgi")}

	cbornode.RegisterCborType(A{})

	res, err := cbornode.Encode(a)
	assert.NoError(t, err)

	t.Log(res)

	b := A{}
	err = cbornode.DecodeInto(res, &b)
	assert.NoError(t, err)

	assert.Equal(t, a, b)
}
