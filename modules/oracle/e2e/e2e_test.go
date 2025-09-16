package oraclee2e

import (
	"context"
	"fmt"
	"testing"
	"vsc-node/modules/aggregate"

	"github.com/stretchr/testify/assert"
)

func TestE2E(t *testing.T) {
	plugins := []aggregate.Plugin{}
	for i := range 10 {
		nodeName := fmt.Sprintf("testnode-%d", i)
		node := MakeNode(nodeName)
		plugins = append(plugins, append(node.plugins, node.oracle)...)
	}

	p := aggregate.New(plugins)
	assert.NoError(t, p.Init())

	_, err := p.Start().Await(context.Background())
	assert.NoError(t, err)
}
