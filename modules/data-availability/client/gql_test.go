package data_availability_client_test

import (
	"testing"
	data_availability_client "vsc-node/modules/data-availability/client"

	"github.com/stretchr/testify/assert"
)

func TestFetchElection(t *testing.T) {
	res, err := data_availability_client.FetchElection()
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, len(res.Members), 9)
}
