package elections

import (
	"vsc-node/modules/common"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
)

type electionHeaderRaw struct {
	ElectionCommonInfo
	Data cid.Cid
}

func (e ElectionHeader) Cid() (cid.Cid, error) {
	verifyObj := map[string]interface{}{
		"__t":    "approve_election",
		"data":   e.Data,
		"epoch":  e.Epoch,
		"net_id": e.NetId,
		"type":   e.Type,
	}
	bytes, _ := common.EncodeDagCbor(verifyObj)

	cid, err := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   multihash.SHA2_256,
		MhLength: -1,
	}.Sum(bytes)

	return cid, err
}
