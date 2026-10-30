package test_utils

import (
	"context"
	"vsc-node/modules/aggregate"
	start_status "vsc-node/modules/start-status"

	"github.com/stretchr/testify/assert"
)

type TestingT interface {
	assert.TestingT
	Cleanup(func())
}

// manages the lifecycle of a plugin
//
// inits -> starts -> stops upon test completion
func RunPlugin(t TestingT, plugin aggregate.Plugin, blockUntilComplete ...bool) {
	assert.NoError(t, plugin.Init())
	t.Cleanup(func() {
		assert.NoError(t, plugin.Stop())
	})
	run := func() {
		_, err := plugin.Start().Await(context.Background())
		assert.NoError(t, err)
		if err != nil {
			panic(err)
		}
	}
	if len(blockUntilComplete) >= 1 && blockUntilComplete[0] {
		run()
	} else {
		go func() {
			// Unlike the blocking path above, a Start rejection is NOT
			// reported here: async rejections are stop-induced (the
			// streamer pattern rejects its Start promise when stopped —
			// including a test's own mid-test Stop calls) and typically
			// land after the test has completed, where asserting would
			// panic the whole suite. Lifecycle errors surface through the
			// cleanup Stop and each test's own behavior assertions.
			_, _ = plugin.Start().Await(context.Background())
		}()
		starter, ok := plugin.(start_status.Starter)
		if ok {
			starter.Started().Await(context.Background())
		}
	}
}
