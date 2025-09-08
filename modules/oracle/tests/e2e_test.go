package tests

import (
	"context"
	"testing"
	"vsc-node/modules/oracle"

	"github.com/stretchr/testify/assert"
)

func TestE2E(t *testing.T) {
	oracleSrv := oracle.New(p2pSrv, conf, electionDb, witnessDb)

	assert.NoError(t, oracleSrv.Init())
	defer oracleSrv.Stop()

	_, err := oracleSrv.Start().Await(context.Background())
	assert.NoError(t, err)
}
