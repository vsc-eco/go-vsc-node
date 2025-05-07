package hive_mock_network_test

import (
	"context"
	"testing"
	"time"
	hive_mock_network "vsc-node/experiments/hive/mock-network"

	"github.com/chebyrash/promise"
	"github.com/stretchr/testify/assert"
)

func sleepPromise(d time.Duration) *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		time.Sleep(d)
		resolve(nil)
	})
}

func Test(t *testing.T) {
	close, done := hive_mock_network.Network()
	defer close(context.Background())
	_, err := promise.Race(context.Background(), sleepPromise(20*time.Minute), done).Await(context.Background())
	assert.NoError(t, err)
}
