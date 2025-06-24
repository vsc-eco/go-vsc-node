package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
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

func DecodeCbor(data []byte, obj interface{}) error {
	node, err := cbornode.Decode(data, multihash.SHA2_256, -1)

	if err != nil {
		return fmt.Errorf("failed to decode CBOR: %w", err)
	}
	jjson, err := node.MarshalJSON()
	if err != nil {
		return fmt.Errorf("failed to decode CBOR: %w", err)
	}

	if err := json.Unmarshal(jjson, obj); err != nil {
		return fmt.Errorf("failed to unmarshal CBOR to object: %w", err)
	}

	return nil
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
	Algo string `refmt:"alg" json:"alg"`
	Sig  string `refmt:"sig" json:"sig"`
	//Only applies to KeyID
	//Technically redundant as it's stored in Required_Auths
	Kid string `refmt:"kid" json:"kid"`
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

func SafeParseHiveFloat(amount string) (int64, error) {
	parts := strings.Split(amount, ".")
	if len(parts) != 2 {
		return 0, fmt.Errorf("must have exactly 1 decimal point")
	}

	if len(parts[1]) != 3 {
		return 0, fmt.Errorf("decimal part must have 3 decimal places")
	}

	return strconv.ParseInt(strings.Join(parts, ""), 10, 64)
}
