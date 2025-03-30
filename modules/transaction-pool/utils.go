package transactionpool

import (
	"vsc-node/modules/common"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

func HashKeyAuths(keyAuths []string) string {
	if len(keyAuths) < 2 {
		return keyAuths[0]
	} else {
		keyMap := make(map[string]bool)

		for _, key := range keyAuths {
			keyMap[key] = true
		}

		dagBytes, _ := common.EncodeDagCbor(keyMap)

		cidz, _ := cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagCbor),
			MhType:   multihash.SHA2_256,
			MhLength: -1,
		}.Sum(dagBytes)

		return cidz.String()
	}
}
