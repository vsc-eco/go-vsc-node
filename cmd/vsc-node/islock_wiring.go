package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sync"

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
	chainID    string
	identity   common.IdentityConfig
	p2pServer  *libp2p.P2PServer
	enabled    bool
	trustAll   bool
	service     *islock.Service
	startErr    error
	startedOnce sync.Once
}

// NewIslockAttestationPlugin constructs the plugin. Returns an
// enabled instance only when MAGI_ISLOCK_ENABLE=true; otherwise
// it's a no-op (Init/Start/Stop succeed without side effects).
func NewIslockAttestationPlugin(
	chainID string,
	identity common.IdentityConfig,
	p2pServer *libp2p.P2PServer,
) *IslockAttestationPlugin {
	enabled := os.Getenv("MAGI_ISLOCK_ENABLE") == "true"
	trustAll := os.Getenv("MAGI_ISLOCK_TRUST_ALL") == "true"
	return &IslockAttestationPlugin{
		chainID:   chainID,
		identity:  identity,
		p2pServer: p2pServer,
		enabled:   enabled,
		trustAll:  trustAll,
	}
}

// Init is the aggregate.Plugin lifecycle hook called before Start.
// We defer the actual libp2p PubSubService construction to Start
// because the underlying p2pServer needs to be up first.
func (p *IslockAttestationPlugin) Init() error {
	if !p.enabled {
		return nil
	}
	if !p.trustAll {
		// Refuse to start an attestation service that would sign
		// everything without a real memory backing. The trustAll
		// flag is an explicit ack required to use the stub.
		return fmt.Errorf(
			"MAGI_ISLOCK_ENABLE=true requires MAGI_ISLOCK_TRUST_ALL=true " +
				"until real dashd-ZMQ memory backing is wired " +
				"(devnet-only safeguard)")
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
			memory := newTrustAllMemoryReader(p.trustAll)
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

// Stop is a no-op — the libp2p shutdown path tears down the
// pubsub service alongside its parent host.
func (p *IslockAttestationPlugin) Stop() error { return nil }

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

// ===== unused but documented for the linter =====

var _ context.Context // future hook for ctx-aware lookup
