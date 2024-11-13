package status

import (
	"fmt"
	"time"

	"vsc-node/experiments/p2p/peers"
	"vsc-node/modules/aggregate"

	"github.com/chebyrash/promise"
)

type Status struct {
	prefix string
	peers  *peers.Peers
	ticks  *time.Ticker
	done   chan bool
}

var _ aggregate.Plugin = &Status{}

const DefaultInterval time.Duration = time.Second

func New(peers *peers.Peers) *Status {
	return NewWithIntervalAndPrefix("", peers, DefaultInterval)
}

func NewWithIntervalAndPrefix(prefix string, peers *peers.Peers, interval time.Duration) *Status {
	return &Status{prefix, peers, time.NewTicker(interval), make(chan bool)}
}

// Init implements aggregate.Plugin.
func (s *Status) Init() error {
	return nil
}

// Start implements aggregate.Plugin.
func (s *Status) Start() *promise.Promise[any] {
	go func() {
		for {
			select {
			case <-s.done:
				return
			case <-s.ticks.C:
				fmt.Println(s.prefix, "peer count:", s.peers.Size())
			}
		}
	}()

	return nil
}

// Stop implements aggregate.Plugin.
func (s *Status) Stop() error {
	s.ticks.Stop()
	s.done <- true
	return nil
}
