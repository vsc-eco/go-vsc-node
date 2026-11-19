package datalayer

import (
	"github.com/ipfs/go-cid"
)

type Session struct {
	da     *DataLayer
	blocks map[string]sessionBlock
}

func (s *Session) Commit() []cid.Cid {
	out := make([]cid.Cid, 0, len(s.blocks))
	values := make([][]byte, 0, len(s.blocks))
	for _, block := range s.blocks {
		values = append(values, block.bytes)
		out = append(out, block.cid)
	}
	// One write for all of them; the same blocks as one PutRaw each. A failed
	// batch is retried one block at a time inside PutRawMany.
	if _, err := s.da.PutRawMany(values); err != nil {
		log.Error("datalayer session: storing blocks failed", "blocks", len(values), "err", err)
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
