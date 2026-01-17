package common_types

import (
	"vsc-node/lib/logger"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"

	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	format "github.com/ipfs/go-ipld-format"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multicodec"
)

type PeerInfoGetter interface {
	GetPeerId() string
	GetPeerAddrs() []multiaddr.Multiaddr
	GetPeerAddr() multiaddr.Multiaddr
	GetStatus() network.Reachability
}

type PutRawOptions struct {
	Codec multicodec.Code
	Pin   bool
}
type PutOptions struct {
	Broadcast bool
}
type GetOptions struct {
	NoStore bool
}

type DataLayer interface {
	PutRaw(data []byte, options PutRawOptions) (*cid.Cid, error)
	PutObject(data interface{}, options ...PutOptions) (*cid.Cid, error)
	PutJson(data interface{}, options ...PutOptions) (*cid.Cid, error)
	HashObject(data interface{}) (*cid.Cid, error)
	Get(cid cid.Cid, options *GetOptions) (format.Node, error)
	GetObject(cid cid.Cid, v interface{}, options GetOptions) error
	GetDag(cid cid.Cid) (*dagCbor.Node, error)
	GetRaw(cid cid.Cid) ([]byte, error)
}

type StateEngine interface {
	//TODO: Handle logger allocation per sub-routine
	Log() logger.Logger
	DataLayer() DataLayer
	//returns: contract information (contracts.Contract) contract exists (bool)
	GetContractInfo(id string, height uint64) (contracts.Contract, bool)
	GetElectionInfo(height ...uint64) elections.ElectionResult
	SystemConfig() systemconfig.SystemConfig
}

type BlockStatusGetter interface {
	HeadHeight() *uint64
	BlockHeight() uint64
}
