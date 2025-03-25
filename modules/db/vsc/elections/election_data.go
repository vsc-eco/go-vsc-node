package elections

import (
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"

	"github.com/ipfs/go-cid"
	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multihash"
)

func (e ElectionData) Cid() (cid.Cid, error) {
	node, err := cbornode.WrapObject(e, multihash.SHA2_256, -1)
	if err != nil {
		return cid.Cid{}, err
	}

	return node.Cid(), nil
}

func (e ElectionData) MemberKeys() []dids.BlsDID {
	return utils.Map(e.Members, func(m ElectionMember) dids.BlsDID {
		return dids.BlsDID(m.Key)
	})
}
