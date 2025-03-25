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
func RunPlugin(t *testing.T, plugin aggregate.Plugin, blockUntilComplete ...bool) {
	assert.NoError(t, plugin.Init())
	t.Cleanup(func() {
		assert.NoError(t, plugin.Stop())
	})
	run := func() {
		_, err := plugin.Start().Await(context.Background())
		assert.NoError(t, err)
	}
	if len(blockUntilComplete) >= 1 && blockUntilComplete[0] {
		run()
	} else {
		go run()
	}
}
