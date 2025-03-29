package common

import (
	"bytes"
	"encoding/json"
	"reflect"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"

	blocks "github.com/ipfs/go-block-format"
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

type Sig struct {
	Algo string "json:\"alg\""
	Sig  string "json:\"sig\""
	//Only applies to KeyID
	//Technically redundant as it's stored in Required_Auths
	Kid string "json:\"kid\""
}

func VerifySignatures(requiredAuths []string, blk blocks.Block, sigs []Sig) (bool, error) {
	auths, err := dids.ParseMany(requiredAuths)
	if err != nil {
		return false, err
	}

	verified, _, err := dids.VerifyMany(auths, blk, utils.Map(sigs, func(sig Sig) string {
		return sig.Sig
	}))

	return verified, err
}
