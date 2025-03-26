package common

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/codec/dagcbor"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/multiformats/go-multicodec"
	multihash "github.com/multiformats/go-multihash/core"
	"go.mongodb.org/mongo-driver/bson/primitive"

	codecJson "github.com/ipld/go-ipld-prime/codec/json"
)

func EncodeDagCbor(obj interface{}) ([]byte, error) {
	buf, _ := json.Marshal(obj)

	nb := basicnode.Prototype.Any.NewBuilder()

	codecJson.Decode(nb, bytes.NewBuffer(buf))

	node := nb.Build()

	var bbuf bytes.Buffer
	dagcbor.Encode(node, &bbuf)
	return bbuf.Bytes(), nil
}

func HashBytes(data []byte, mf multicodec.Code) (cid.Cid, error) {
	prefix := cid.Prefix{
		Version:  1,
		Codec:    uint64(mf),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}

	return prefix.Sum(data)
}

func ArrayToStringArray(arr interface{}) []string {
	out := make([]string, 0)
	if reflect.TypeOf(arr).String() == "primitive.A" {
		for _, v := range arr.(primitive.A) {
			out = append(out, v.(string))
		}
	} else {
		//Assume []interface{}
		for _, v := range arr.([]interface{}) {
			out = append(out, v.(string))
		}
	}

	return out
}
