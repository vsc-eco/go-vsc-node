package common

import "github.com/multiformats/go-multiaddr"

type PeerInfoGetter interface {
	GetPeerId() string
	GetPeerAddrs() []multiaddr.Multiaddr
	GetPeerAddr() multiaddr.Multiaddr
}

type SystemConfig struct {
	Network string
}
