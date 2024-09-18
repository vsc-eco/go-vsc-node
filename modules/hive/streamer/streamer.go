package streamer

import (
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db"
)

type Streamer struct {
	db *db.Db
}

var _ a.Plugin = &Streamer{}

func New(db *db.Db) *Streamer {
	return &Streamer{db}
}

func (s *Streamer) Init() error {
	panic("unimplemented")
}

func (s *Streamer) Start() error {
	panic("unimplemented")
}

func (s *Streamer) Stop() error {
	panic("unimplemented")
}
