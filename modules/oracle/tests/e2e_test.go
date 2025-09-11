package tests

import (
	"context"
	"testing"
	"vsc-node/modules/oracle"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestE2E(t *testing.T) {
	var (
		p2pSrv         = &stubP2pServer{nil, t}
		oracleP2pParam = &p2p.OracleP2pSpec{}
	)
	oracleSrv := oracle.New(
		p2pSrv.P2PServer,
		oracleP2pParam,
		conf,
		electionDb,
		witnessDb,
		vStream,
		state,
	)

	assert.NoError(t, oracleSrv.Init())
	defer oracleSrv.Stop()

	_, err := oracleSrv.Start().Await(context.Background())
	assert.NoError(t, err)
}
