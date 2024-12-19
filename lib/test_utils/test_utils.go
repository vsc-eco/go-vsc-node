package test_utils

import (
	"context"
	"testing"
	"vsc-node/modules/aggregate"

	"github.com/stretchr/testify/assert"
)

// manages the lifecycle of a plugin
//
// inits -> starts -> stops upon test completion
func RunPlugin(t *testing.T, plugin aggregate.Plugin) {
	assert.NoError(t, plugin.Init())
	go func() {
		_, err := plugin.Start().Await(context.Background())
		assert.NoError(t, err)
	}()
	t.Cleanup(func() {
		assert.NoError(t, plugin.Stop())
	})
}
