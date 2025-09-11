package tests

import (
	"context"
	"testing"
	"time"
	"vsc-node/modules/oracle"

	"github.com/stretchr/testify/assert"
)

func TestE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancel()

	var (
		p2pSrv         = &stubP2pServer{nil, t}
		oracleP2pParam = &stubOracleP2pSpec{nil, t}
		blockScheduler = &stubBlockScheduler{}
		vStreamStub    = &stubVStream{
			funck:  nil,
			ticker: time.NewTicker(500 * time.Millisecond),
			ctx:    ctx,
		}
	)
	oracleSrv := oracle.New(
		p2pSrv.P2PServer,
		oracleP2pParam,
		conf,
		electionDb,
		witnessDb,
		vStreamStub,
		blockScheduler,
	)

	assert.NoError(t, oracleSrv.Init())
	defer oracleSrv.Stop()

	p := oracleSrv.Start()

	_, err := p.Await(ctx)
	assert.Nil(t, err)
}
