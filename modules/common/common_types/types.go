package common_types

import (
	"context"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	blocks "github.com/ipfs/go-block-format"
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
	Get(ctx context.Context, cid cid.Cid, options *GetOptions) (format.Node, error)
	GetObject(ctx context.Context, cid cid.Cid, v interface{}, options GetOptions) error
	GetDag(ctx context.Context, cid cid.Cid) (*dagCbor.Node, error)
	GetRaw(ctx context.Context, cid cid.Cid) ([]byte, error)
	// GetMany fetches multiple CIDs concurrently using a Bitswap session.
	GetMany(ctx context.Context, cids []cid.Cid) (map[cid.Cid]blocks.Block, error)
}

type StateEngine interface {
	DataLayer() DataLayer
	//returns: contract information (contracts.Contract) contract exists (bool)
	GetContractInfo(id string, height uint64) (contracts.Contract, bool)
	GetElectionInfo(height ...uint64) elections.ElectionResult
	SystemConfig() systemconfig.SystemConfig
	WasmRuntime() *wasm_runtime.Wasm
	GetCachedCode(c cid.Cid) ([]byte, bool)
	PutCachedCode(c cid.Cid, code []byte)
}

type BlockStatusGetter interface {
	HeadHeight() *uint64
	BlockHeight() uint64
}
