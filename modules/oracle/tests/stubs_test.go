package tests

import (
	"testing"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/oracle/p2p"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	"vsc-node/modules/vstream"

	"github.com/stretchr/testify/assert"
)

var (
	conf       common.IdentityConfig
	electionDb elections.Elections
	witnessDb  witnesses.Witnesses
	vStream    *vstream.VStream
	state      *stateEngine.StateEngine
)

type stubP2pServer struct {
	*libp2p.P2PServer
	*testing.T
}

func (s *stubP2pServer) SendToAll(topic string, message []byte) {
	assert.Equal(s.T, p2p.OracleTopic, topic)
}
