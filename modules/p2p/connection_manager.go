package libp2p

import (
	"time"

	libp2pConnmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
)

// Pentest findings N-M3 (relay limits) and N-M4 (no ConnectionManager):
// libp2p.go used to call relay.WithInfiniteLimits() and pass no
// ConnectionManager option, leaving the host willing to accept
// unbounded peer connections AND to relay unmetered traffic for
// any client.
//
// p2pConnectionManager returns a libp2p.ConnectionManager option
// with sensible caps: low water 200, high water 400, 30s grace
// before pruning. These are the libp2p defaults — the only thing
// the production node was missing was the option itself.

const (
	connMgrLowWater  = 200
	connMgrHighWater = 400
	connMgrGrace     = 30 * time.Second
)

func p2pConnectionManager() *libp2pConnmgr.BasicConnMgr {
	mgr, err := libp2pConnmgr.NewConnManager(
		connMgrLowWater,
		connMgrHighWater,
		libp2pConnmgr.WithGracePeriod(connMgrGrace),
	)
	if err != nil {
		// NewConnManager only errors on invalid (negative or zero)
		// limits; the constants above are positive so this path is
		// unreachable in practice. Returning a usable BasicConnMgr
		// with explicit defaults is the safer fallback than
		// panicking at startup.
		mgr, _ = libp2pConnmgr.NewConnManager(connMgrLowWater, connMgrHighWater)
	}
	return mgr
}
