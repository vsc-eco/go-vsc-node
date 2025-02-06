package elections

import (
	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multihash"
)

type electionHeaderRaw struct {
	electionCommonInfo
	Data cid.Cid
}

func (e ElectionHeader) Cid() (cid.Cid, error) {
	c, err := cid.Parse(e.Data)
	if err != nil {
		return cid.Cid{}, err
	}

	raw := electionHeaderRaw{
		e.electionCommonInfo,
		c,
	}

	node, err := cbornode.WrapObject(raw, multihash.SHA2_256, -1)
	if err != nil {
		return cid.Cid{}, err
	}

	return node.Cid(), nil
}
