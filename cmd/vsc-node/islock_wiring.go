package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	libp2p "vsc-node/modules/p2p"

	islock "vsc-node/modules/islock-attestation"

	"github.com/chebyrash/promise"
	blsu "github.com/protolambda/bls12-381-util"
)

// IslockAttestationPlugin is an aggregate.Plugin wrapper around the
// modules/islock-attestation Service. Initialised at magi node
// startup so witnesses participate in the IS-lock attestation
// gossip topic, signing canonical messages for txids they have in
// memory.
//
// **Devnet gating**: the plugin only constructs the Service when
// MAGI_ISLOCK_ENABLE=true. Production wiring (real dashd ZMQ
// memory backing) is a separate follow-up — the current stub
// MemoryReader returns ok=true for every txid, which is safe
// ONLY when the validator-set + attestation request validator
// (modules/islock-attestation:HandleMessage rate-limiting +
// signature shape checks) bound the surface. See
// `MAGI_ISLOCK_TRUST_ALL` for the explicit ack.
type IslockAttestationPlugin struct {
	chainID   string
	network   string
	identity  common.IdentityConfig
	p2pServer *libp2p.P2PServer
	enabled   bool

	// trustAll: deprecated devnet-only fallback. When set without a
	// real dashd RPC URL, the MemoryReader returns ok=true for
	// every txid. Kept for backwards compatibility with the early
	// devnet wiring; new code paths should configure
	// MAGI_ISLOCK_DASHD_RPC instead.
	trustAll bool

	// dashdRPC: full URL of the validator's dashd JSON-RPC
	// endpoint. When set, the plugin starts a DashdPoller that
	// populates a real IsLockMemory cache from observed mempool
	// txs (filtering on `instantlock=true` unless
	// acceptUnlocked is also set).
	dashdRPC     string
	dashdRPCUser string
	dashdRPCPass string

	// acceptUnlocked: devnet-only bypass that downgrades the
	// instantlock=true check to "tx observed in mempool". Mirrors
	// cmd/is-service's -testBypassDashdISLock for the regtest case.
	acceptUnlocked bool

	// pollerCancel cancels the DashdPoller goroutine on Stop().
	pollerCancel context.CancelFunc

	service     *islock.Service
	startErr    error
	startedOnce sync.Once
}

// NewIslockAttestationPlugin constructs the plugin. Returns an
// enabled instance only when MAGI_ISLOCK_ENABLE=true; otherwise
// it's a no-op (Init/Start/Stop succeed without side effects).
//
// Operator env vars:
//   - MAGI_ISLOCK_ENABLE          (required) — turn the plugin on
//   - MAGI_ISLOCK_DASHD_RPC       — http URL of validator's dashd
//     (preferred — uses real IsLockMemory backed by observed mempool)
//   - MAGI_ISLOCK_DASHD_USER      — basic-auth user
//   - MAGI_ISLOCK_DASHD_PASS      — basic-auth password
//   - MAGI_ISLOCK_ACCEPT_UNLOCKED — devnet-only; records txs even
//     when dashd reports instantlock=false. Required for regtest
//     dashd (no LLMQ).
//   - MAGI_ISLOCK_TRUST_ALL       — DEPRECATED devnet-only; when
//     set WITHOUT a DASHD_RPC URL, MemoryReader returns ok=true
//     for every txid. Use the DASHD_RPC + ACCEPT_UNLOCKED pair
//     instead — closer to the production code path.
func NewIslockAttestationPlugin(
	chainID string,
	network string,
	identity common.IdentityConfig,
	p2pServer *libp2p.P2PServer,
) *IslockAttestationPlugin {
	enabled := os.Getenv("MAGI_ISLOCK_ENABLE") == "true"
	trustAll := os.Getenv("MAGI_ISLOCK_TRUST_ALL") == "true"
	dashdRPC := os.Getenv("MAGI_ISLOCK_DASHD_RPC")
	dashdUser := os.Getenv("MAGI_ISLOCK_DASHD_USER")
	dashdPass := os.Getenv("MAGI_ISLOCK_DASHD_PASS")
	acceptUnlocked := os.Getenv("MAGI_ISLOCK_ACCEPT_UNLOCKED") == "true"
	return &IslockAttestationPlugin{
		chainID:        chainID,
		network:        network,
		identity:       identity,
		p2pServer:      p2pServer,
		enabled:        enabled,
		trustAll:       trustAll,
		dashdRPC:       dashdRPC,
		dashdRPCUser:   dashdUser,
		dashdRPCPass:   dashdPass,
		acceptUnlocked: acceptUnlocked,
	}
}

// Init is the aggregate.Plugin lifecycle hook called before Start.
// We defer the actual libp2p PubSubService construction to Start
// because the underlying p2pServer needs to be up first.
//
// Backing-mode validation:
//   - DASHD_RPC set: production-shape, requires either
//     instantlock=true upstream OR ACCEPT_UNLOCKED+network=devnet.
//   - TRUST_ALL set with no DASHD_RPC: deprecated devnet-only
//     fallback. Refuses non-devnet networks.
//   - Neither: reject — refuse to start without ANY memory
//     backing, since the validator would sign garbage requests.
func (p *IslockAttestationPlugin) Init() error {
	if !p.enabled {
		return nil
	}
	hasRPC := p.dashdRPC != ""
	if !hasRPC && !p.trustAll {
		return fmt.Errorf(
			"MAGI_ISLOCK_ENABLE=true requires either MAGI_ISLOCK_DASHD_RPC " +
				"(production-shape memory backing) or MAGI_ISLOCK_TRUST_ALL " +
				"(deprecated devnet-only fallback)")
	}
	if p.acceptUnlocked && p.network != "devnet" {
		return fmt.Errorf(
			"MAGI_ISLOCK_ACCEPT_UNLOCKED=true requires -network=devnet (got %q); "+
				"this flag downgrades the IS-lock check and is gated to "+
				"the devnet-only test surface", p.network)
	}
	if p.trustAll && p.network != "devnet" {
		return fmt.Errorf(
			"MAGI_ISLOCK_TRUST_ALL=true requires -network=devnet (got %q); "+
				"trust-all is the deprecated devnet-only fallback", p.network)
	}
	return nil
}

// Start spins up the islock-attestation Service + binds it to a
// libp2p PubSubService. Returns a promise that resolves when the
// service is publishing/subscribed.
func (p *IslockAttestationPlugin) Start() *promise.Promise[any] {
	if !p.enabled {
		return promise.New(func(resolve func(any), reject func(error)) {
			resolve(nil)
		})
	}
	return promise.New(func(resolve func(any), reject func(error)) {
		p.startedOnce.Do(func() {
			signer, err := newIdentityAttestationSigner(p.identity)
			if err != nil {
				p.startErr = fmt.Errorf("build attestation signer: %w", err)
				return
			}
			// Memory backing: prefer real dashd-RPC poller. Falls
			// back to trust-all stub only when no RPC URL is set
			// (deprecated devnet path).
			var memory islock.MemoryReader
			if p.dashdRPC != "" {
				realMem := islock.NewIsLockMemory()
				poller := islock.NewDashdPoller(
					p.dashdRPC, p.dashdRPCUser, p.dashdRPCPass,
					realMem, 2*time.Second, p.acceptUnlocked)
				pctx, cancel := context.WithCancel(context.Background())
				p.pollerCancel = cancel
				go func() {
					if err := poller.Run(pctx); err != nil && pctx.Err() == nil {
						slog.Error("islock-attestation dashd poller exited",
							"err", err)
					}
				}()
				slog.Info("islock-attestation: dashd-RPC MemoryReader configured",
					"acceptUnlocked", p.acceptUnlocked,
					"network", p.network)
				memory = realMem
			} else {
				slog.Warn("islock-attestation: trust-all MemoryReader (deprecated; configure MAGI_ISLOCK_DASHD_RPC for production-shape backing)",
					"network", p.network)
				memory = newTrustAllMemoryReader(p.trustAll)
			}
			p.service = islock.NewService(p.chainID, signer, memory, nil)

			// Construct the pubsub service binding. Pubsub topic +
			// validator come from p.service via the PubSubServiceParams
			// interface methods (Topic, ValidateMessage, etc.).
			ps, err := libp2p.NewPubSubService(p.p2pServer, p.service)
			if err != nil {
				p.startErr = fmt.Errorf("build pubsub service: %w", err)
				return
			}
			p.service.Start(ps)
			slog.Info("islock-attestation service started",
				"chainID", p.chainID,
				"validatorDID", signer.ValidatorDID(),
				"topic", p.service.Topic(),
				"trustAllMemory", p.trustAll)
		})
		if p.startErr != nil {
			reject(p.startErr)
			return
		}
		resolve(nil)
	})
}

// Stop cancels the dashd poller (if active) and lets the libp2p
// shutdown path tear down the pubsub service alongside its parent
// host.
func (p *IslockAttestationPlugin) Stop() error {
	if p.pollerCancel != nil {
		p.pollerCancel()
	}
	return nil
}

// ===== AttestationSigner adapter =====

type identityAttestationSigner struct {
	priv      *dids.BlsPrivKey
	did       string
	pubkeyHex string
}

func newIdentityAttestationSigner(identity common.IdentityConfig) (*identityAttestationSigner, error) {
	priv, err := identity.BlsPrivKey()
	if err != nil {
		return nil, fmt.Errorf("identity BLS priv: %w", err)
	}
	pub, err := blsu.SkToPk(priv)
	if err != nil {
		return nil, fmt.Errorf("derive BLS pub: %w", err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		return nil, fmt.Errorf("derive BLS DID: %w", err)
	}
	pubBytes := pub.Serialize()
	return &identityAttestationSigner{
		priv:      priv,
		did:       did.String(),
		pubkeyHex: hex.EncodeToString(pubBytes[:]),
	}, nil
}

func (s *identityAttestationSigner) ValidatorDID() string { return s.did }
func (s *identityAttestationSigner) PubkeyHex() string    { return s.pubkeyHex }

func (s *identityAttestationSigner) Sign(req islock.IsLockAttestationRequest) (string, error) {
	return islock.Sign(req, s.priv)
}

// ===== MemoryReader adapter =====

// trustAllMemoryReader is a stub IsLockMemory implementation that
// returns ok=true for every txid lookup. **TEST-ONLY** — gated by
// MAGI_ISLOCK_TRUST_ALL=true, which the plugin's Init() refuses to
// run without explicitly. Real production deployments need a
// dashd-ZMQ-backed memory reader that only returns ok=true after
// the validator independently observed `instantlock=true` for the
// txid via its own dashd. Until that wiring lands, the trust-all
// stub lets the devnet harness exercise the gossip pipeline
// without a real LLMQ quorum.
type trustAllMemoryReader struct{ enabled bool }

func newTrustAllMemoryReader(enabled bool) *trustAllMemoryReader {
	return &trustAllMemoryReader{enabled: enabled}
}

// Lookup returns a placeholder rawTxHex + rawTxHash for any txid
// when trust-all mode is active. Otherwise returns ok=false.
//
// The returned rawTxHex is "" — modules/islock-attestation's
// handleRequest only uses it for the canonical-message hash
// calculation, but since trust-all mode is for testing only,
// the IS service-side caller is expected to supply
// CanonicalSigningMessage-matching values via the request fields
// themselves. The Sign call uses req.TxId/RawTxHashHex/etc.
// directly and never consults memory after Lookup returns ok=true.
func (r *trustAllMemoryReader) Lookup(txid string) (rawTxHex string, rawTxHash []byte, ok bool) {
	if !r.enabled {
		return "", nil, false
	}
	return "", nil, true
}

