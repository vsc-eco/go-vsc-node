package streamer

import (
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/witnesses"
)

type Streamer struct {
	witnesses witnesses.Witnesses
}

var _ a.Plugin = &Streamer{}

func New(witnesses witnesses.Witnesses) *Streamer {
	return &Streamer{witnesses: witnesses}
}

func (s *Streamer) Init() error {

	return nil
	// panic("unimplemented")
}

func (s *Streamer) Start() error {

	return nil
	// panic("unimplemented")
}

func (s *Streamer) Stop() error {

	return nil
	// panic("unimplemented")
}
