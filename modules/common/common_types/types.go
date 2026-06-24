package common_types

import (
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	wasm_context "vsc-node/modules/wasm/context"

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
	DataLayer() DataLayer
	//returns: contract information (contracts.Contract) contract exists (bool)
	GetContractInfo(id string, height uint64) (contracts.Contract, bool)
	// GetElectionInfoOrBlock is the fail-stop election read: it blocks on a
	// transient DB error instead of swallowing it and returning a zero-value
	// epoch (which would let one node decide a tx outcome differently from
	// peers — a fork). found=false means a deterministic absence (no election
	// covers the height yet), which every honest node sees identically.
	GetElectionInfoOrBlock(height uint64) (elections.ElectionResult, bool)
	// NodeDelegationMode returns the operator-published consensus-delegation
	// mode for node `account` as of blockHeight, normalized to a known value
	// (delegationmode.{Deactivated,Share,Custom}). Defaults to Deactivated when
	// the node has no announcement or an unset/unknown mode — delegation is
	// strict opt-in. Sourced from the witness DB (the operator's authenticated
	// on-chain announcement), so it is deterministic across nodes at a given
	// height. Consensus 0.3.0+ uses it to gate third-party stake acceptance.
	NodeDelegationMode(account string, blockHeight uint64) string
	SystemConfig() systemconfig.SystemConfig
	// PendulumOracleEnv returns key/value pairs merged into wasm contract env (system.get_env / get_env_key).
	// Keys use the "pendulum.*" prefix; nil or empty means no pendulum snapshot is available.
	PendulumOracleEnv() map[string]interface{}
	// PendulumApplier wires the swap-time pendulum SDK method
	// (system.pendulum_apply_swap_fees). May return nil when the pendulum
	// data plane (snapshot DB, ledger, whitelist) isn't fully initialized;
	// the SDK method short-circuits to ErrUnimplemented in that case.
	PendulumApplier() wasm_context.PendulumApplier
	// ActiveConsensusVersion returns the chain-active consensus triple at a block
	// height, sourced from the on-chain election (deterministic, height-addressable).
	// Used to gate consensus-version-coordinated features (e.g. try/catch ICC).
	ActiveConsensusVersion(blockHeight uint64) consensusversion.Version
}

type BlockStatusGetter interface {
	HeadHeight() *uint64
	BlockHeight() uint64
}
