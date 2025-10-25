package oraclee2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
	"vsc-node/modules/aggregate"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
)

const testNodeCount = 5

func TestE2E(t *testing.T) {
	outputLogFile, err := os.OpenFile("/tmp/oracle-e2e-test.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatal(err)
	}
	defer outputLogFile.Close()

	logWriter := io.MultiWriter(
		os.Stdout,
		outputLogFile,
	)

	loggerHandler := slog.NewTextHandler(logWriter, &slog.HandlerOptions{
		Level:     slog.LevelDebug,
		AddSource: true,
	})
	logger := slog.New(loggerHandler)
	slog.SetDefault(logger)

	if err := os.Setenv("DEBUG", "1"); err != nil {
		t.Fatal(err)
	}

	nodes := []*Node{}
	plugins := []aggregate.Plugin{}

	for i := range testNodeCount {
		nodeName := fmt.Sprintf("testnode-%d", i)

		node := MakeNode(nodeName)

		plugins = append(plugins, append(node.plugins, node.oracle)...)
		nodes = append(nodes, node)
	}

	p := aggregate.New(plugins)
	assert.NoError(t, p.Init())
	for _, node := range nodes {
		node.db.Drop(t.Context())
	}

	connectP2p(nodes)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	_, err = p.Start().Await(ctx)
	assert.NoError(t, err)
}

func connectP2p(runningNodes []*Node) {
	wg := &sync.WaitGroup{}
	peerAddrs := make([]string, 0)

	for _, node := range runningNodes {
		for _, addr := range node.p2p.Addrs() {
			peerAddrs = append(
				peerAddrs,
				addr.String()+"/p2p/"+node.p2p.ID().String(),
			)
		}
	}

	for _, node := range runningNodes {
		for _, peerStr := range peerAddrs {
			wg.Add(1)

			go func() {
				defer wg.Done()
				peerId, _ := peer.AddrInfoFromString(peerStr)

				ctx, cancel := context.WithTimeout(
					context.Background(),
					5*time.Second,
				)
				defer cancel()

				fmt.Println("Trying to connect", peerId)
				node.p2p.Connect(ctx, *peerId)
			}()
		}
	}

	wg.Wait()
}
