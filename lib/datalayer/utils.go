package datalayer

import (
	"vsc-node/modules/common/common_types"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
)

type Session struct {
	da     *DataLayer
	blocks map[string]sessionBlock
}

func (s *Session) Commit() []cid.Cid {
	out := make([]cid.Cid, 0)
	for _, block := range s.blocks {

		s.da.PutRaw(block.bytes, common_types.PutRawOptions{
			Codec: multicodec.Code(block.cid.Prefix().Codec),
		})
		out = append(out, block.cid)
	}
	return out
}

func (s *Session) Put(bytes []byte, cid cid.Cid) {
	s.blocks[cid.String()] = sessionBlock{bytes: bytes, cid: cid}
}

type sessionBlock struct {
	bytes []byte
	cid   cid.Cid
}

func NewSession(da *DataLayer) *Session {
	return &Session{
		da:     da,
		blocks: make(map[string]sessionBlock),
	}
}
