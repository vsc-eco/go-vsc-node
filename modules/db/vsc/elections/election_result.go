package elections

import (
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"

	"github.com/ipfs/go-cid"
)

func (e ElectionResult) Cid() (cid.Cid, error) {
	dataCid, err := ElectionData{
		e.electionCommonInfo,
		e.electionDataInfo,
	}.Cid()
	if err != nil {
		return cid.Cid{}, err
	}

	return ElectionHeader{
		e.electionCommonInfo,
		electionHeaderInfo{
			dataCid.String(),
		},
	}.Cid()
}

func (e ElectionResult) MemberKeys() []dids.BlsDID {
	return utils.Map(e.Members, func(m ElectionMember) dids.BlsDID {
		return dids.BlsDID(m.Key)
	})
}
