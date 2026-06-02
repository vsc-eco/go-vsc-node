// Command is-service is the Dash InstantSend login webservice that
// translates between user Dash wallet payments and Magi L2 operations.
//
// See:
//   - magi/testnet/docs/superpowers/specs/2026-05-14-dash-instantsend-login-design.md
//   - go-vsc-node-develop/docs/dash-is-login/0-scoping-spike-decision.md
//
// Architecture (high level):
//
//  1. Browser POSTs /session/start specifying op=auth or op=call.
//  2. IS service derives a deterministic Dash deposit address from
//     (primary_bridge_pubkey, backup_bridge_pubkey, instruction).
//     The address is per-session unique because the sid is embedded
//     in the instruction. No on-chain registration is needed.
//  3. Frontend shows the QR. User pays via DashPay InstantSend.
//  4. IS service watches dashd ZMQ rawtxlock; when the lock fires for
//     one of our addresses, transitions session to IS_OBSERVED.
//  5. IS service requests attestations from Magi validators via p2p
//     (modules/islock-attestation). Collects N-of-M BLS-signed
//     responses.
//  6. IS service submits one L2 tx (dash-mapping-contract.mapInstantSend)
//     with the attestation bundle. dash-mapping verifies the BLS
//     aggregate against the active validator set, credits the
//     DashDID's internal balance, and (for op=call) invokes
//     dash-forwarder-contract.execute() which calls the target with
//     effectiveCaller=DashDID via the call_as host function.
//  7. Session transitions to ON_CHAIN; frontend gets sessionToken.
//
// Scope of v1: HTTP API, deposit-address derivation, session state
// machine, in-memory store, rate limits, address-signature HMAC stub.
// ZMQ subscriber, p2p attestation collection, GraphQL polling, and L2
// tx submission are stubs that will be wired up in follow-up commits
// once the supporting infra (workstreams 5/6) is in place.
//
// Run with: `go run ./cmd/is-service -primaryPubkey <hex> -backupPubkey <hex> -network testnet -port 3030`
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	args, err := parseArgs()
	if err != nil {
		slog.Error("failed to parse args", "err", err)
		os.Exit(1)
	}

	if args.debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	// v1 dev signer: HMAC with an in-memory secret derived from the args
	// file. NOT FOR PRODUCTION — production needs an HSM/KMS asymmetric
	// signer per spec §5.7. Refusing to start with an empty secret prevents
	// accidental no-signature deployment.
	if args.addressSignerSecret == "" {
		slog.Error("addressSignerSecret must be set (development HMAC stub; replace with HSM/KMS for prod)")
		os.Exit(1)
	}
	signer := NewAddressSignerHMAC([]byte(args.addressSignerSecret))

	// Optional dashd watcher — when -dashdRPC is set, the IS service
	// auto-transitions sessions to IS_OBSERVED as their deposit address
	// receives IS-locked txs. Without it, the IS-observed transition
	// must be driven externally (e.g. for offline/devnet runs).
	var dashd *DashdWatcher
	if args.dashdRPCURL != "" {
		client := NewDashdRPCClient(args.dashdRPCURL, args.dashdRPCUser, args.dashdRPCPassword)
		// Round-3 audit OP-007: probe dashd at startup so a degraded
		// backend fails-fast instead of accepting user funds against a
		// silently-dead watcher. Mirrors the libp2p broadcaster's
		// existing fail-fast.
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, probeErr := client.GetRawMempool(probeCtx)
		probeCancel()
		if probeErr != nil {
			slog.Error("dashd RPC startup probe failed — refusing to start with degraded backend",
				"rpc", args.dashdRPCURL, "err", probeErr)
			os.Exit(1)
		}
		dashd = NewDashdWatcher(client)
		slog.Info("dashd watcher configured + probed", "rpc", args.dashdRPCURL)
	} else {
		slog.Info("dashd watcher NOT configured — IS_OBSERVED transitions must be driven externally")
	}

	// Orchestrator drives the IS_OBSERVED → ATTESTING → ON_CHAIN
	// progression. v1 wiring uses a no-op broadcaster (logs only) and
	// the log-only submitter — both are placeholders for the real p2p
	// libp2p host and L2 client that production deployments configure
	// via the server-with-p2p variant of NewServer (TODO: add a
	// production main wiring once the libp2p host is exposed in this
	// command's main).
	collector := newAttestationCollector()

	// libp2p broadcaster — when -p2pBootstrapPeers is set, the IS
	// service joins the islock-attestation gossip topic and uses real
	// broadcast + response collection. Without it, the noop
	// broadcaster is used (sessions stall at ATTESTING_TIMEOUT).
	var broadcaster Broadcaster = noopBroadcaster{}
	var p2pCloseFn func() error
	if args.p2pBootstrapPeers != "" {
		boot := splitCSV(args.p2pBootstrapPeers)
		listen := splitCSV(args.p2pListenAddrs)
		topicPrefix := "/vsc/" + args.network // matches PubSubTopicPrefix
		p2pBcast, err := newLibp2pBroadcaster(context.Background(), libp2pBroadcasterConfig{
			ChainID:        args.chainID,
			TopicPrefix:    topicPrefix,
			BootstrapPeers: boot,
			ListenAddrs:    listen,
		})
		if err != nil {
			slog.Error("libp2p broadcaster failed to start", "err", err)
			os.Exit(1)
		}
		// Subscriber goroutine routes responses straight into the
		// collector — bootstrap before the orchestrator so responses
		// arriving immediately after broadcast aren't dropped.
		p2pBcast.Start(context.Background(), collector.Deliver)
		broadcaster = p2pBcast
		p2pCloseFn = p2pBcast.Close
		slog.Info("libp2p broadcaster started",
			"topic", topicPrefix+"/islock-attestation/v1",
			"bootstrapConnectedPeers", p2pBcast.BootstrapConnectedCount(),
			"configuredPeers", len(boot))
	} else {
		slog.Info("libp2p broadcaster NOT configured — using noop (no attestations gathered)")
	}

	// L2 submitter — all-or-nothing on the trio. Partial configs are a
	// silent footgun (audit `no-l2-config-trio-validation`): if any of
	// {l2GqlURL, l2PrivKey, l2DashMappingContract} is set, ALL three
	// must be set or we refuse to start. The previous gate only checked
	// two and silently fell back to log-only when a third was missing.
	var submitter Submitter = SubmitterLogOnly{}
	anyL2 := args.l2GqlURL != "" || args.l2PrivKeyHex != "" || args.l2DashMappingContract != ""
	if anyL2 {
		missing := []string{}
		if args.l2GqlURL == "" {
			missing = append(missing, "-l2GqlURL")
		}
		if args.l2PrivKeyHex == "" {
			missing = append(missing, "-l2PrivKey")
		}
		if args.l2DashMappingContract == "" {
			missing = append(missing, "-l2DashMappingContract")
		}
		if len(missing) > 0 {
			slog.Error("L2 config incomplete — refusing to start with partial L2 setup",
				"missing", missing)
			os.Exit(1)
		}
		l2, err := NewSubmitterL2(SubmitterL2Config{
			GraphQLEndpoint: args.l2GqlURL,
			ContractId:      args.l2DashMappingContract,
			NetId:           args.chainID,
			RcLimit:         args.l2RcLimit,
			PrivateKeyHex:   args.l2PrivKeyHex,
		})
		if err != nil {
			slog.Error("L2 submitter config invalid", "err", err)
			os.Exit(1)
		}
		submitter = l2
		slog.Info("L2 submitter configured",
			"endpoint", args.l2GqlURL,
			"contract", args.l2DashMappingContract,
			"did", l2.DID())
	} else {
		slog.Info("L2 submitter NOT configured — using log-only stub (no on-chain effect)")
	}

	sessions := NewSessionStore(time.Duration(args.sessionTTLMinutes) * time.Minute)

	// Round-4 audit R4-001: wire ValidatorSetForEpoch + EpochFor for
	// the orchestrator's DID↔pubkey cross-check (R3-07). The hook
	// reads the contract's per-epoch validator set via the L2 GraphQL
	// getStateByKeys query; without it, a rogue libp2p peer can sign
	// with its own BLS key while claiming another validator's DID
	// and force the contract to burn submitter RC on rejection.
	//
	// Fail-loud at startup if the hook is nil while the L2 submitter
	// is configured (the wiring is meaningless in log-only mode but
	// silent absence is the bug R4-001 caught in round-3).
	var validatorSetForEpoch func(ctx context.Context, epoch uint64) map[string]string
	var epochFor func(ctx context.Context) uint64
	var validatorSetCacheRef *validatorSetCache
	if l2, ok := submitter.(*SubmitterL2); ok {
		cacheTTL := time.Duration(args.validatorSetCacheTTLSeconds) * time.Second
		if cacheTTL <= 0 {
			cacheTTL = 30 * time.Second
		}
		cache := newValidatorSetCache(l2, args.l2DashMappingContract, cacheTTL)
		validatorSetCacheRef = cache
		validatorSetForEpoch = cache.Get
		// v1 stub: a constant 0 epoch. Replacing this with the active
		// epoch from the witness module is workstream-5b follow-up;
		// in v1 deployments the contract grace-window fallback picks
		// up the most-recently-registered set regardless.
		epochFor = func(context.Context) uint64 { return 0 }
		slog.Info("R3-07 ValidatorSetForEpoch hook wired",
			"contract", args.l2DashMappingContract)
	} else {
		slog.Info("ValidatorSetForEpoch hook NOT wired — log-only submitter (no L2 RC at risk)")
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:             sessions,
		Collector:            collector,
		Broadcaster:          broadcaster,
		Submitter:            submitter,
		ChainID:              args.chainID,
		ValidatorSetForEpoch: validatorSetForEpoch,
		EpochFor:             epochFor,
		ValidatorSetSource:   args.l2GqlURL,
	})

	// /healthz probe — surfaces LIVE libp2p connected-peer count + mesh
	// membership + degraded flag. Audit R2-N6 caught the original
	// cached-boot-time count.
	var broadcasterHealth func() (int, bool)
	if bcaster, ok := broadcaster.(*libp2pBroadcaster); ok {
		broadcasterHealth = func() (int, bool) {
			// Mesh membership is what actually matters for attestation
			// delivery — degraded = zero peers on our topic.
			mesh := bcaster.MeshPeerCount()
			return mesh, mesh == 0
		}
	}

	// Round-3 audit OP-003: /healthz consults DashdWatcher.Health() so
	// a stalled watcher surfaces as red without operator restart-with-debug.
	var dashdHealthFn func() (int, error, time.Time)
	if dashd != nil {
		dashdHealthFn = dashd.Health
	}

	// Round-4 audit R4-007: /healthz consults the L2 submitter's
	// balance + RC via GraphQL so RC-exhaustion outages no longer
	// present as silent ATTESTING pile-ups.
	//
	// Round-5 audit R5-OP-02/R5-OP-06: a synchronous 5s GraphQL probe
	// per /healthz request becomes a self-DoS amplifier under any
	// burst of synthetic probes. The probe now runs in a background
	// goroutine on a fixed interval; /healthz reads an atomic
	// snapshot. Hysteresis (3 consecutive fails) prevents flapping.
	var submitterHealthFn func() (string, int64, int64, int, error)
	var submitterMonitor *submitterHealthMonitor
	if l2, ok := submitter.(*SubmitterL2); ok {
		probe := func(ctx context.Context) (string, int64, int64, error) {
			bal, rc, err := l2.FetchSubmitterHealth(ctx)
			return l2.DID(), bal, rc, err
		}
		submitterMonitor = newSubmitterHealthMonitor(15*time.Second, probe)
		submitterHealthFn = func() (string, int64, int64, int, error) {
			snap := submitterMonitor.Snapshot()
			if snap == nil {
				// Round-6 audit R6-CORR-03: surface the warmup
				// window as a sentinel error so handleHealthz can
				// distinguish it from a real probe failure and
				// skip the degraded flip. Without this, a /healthz
				// hit during the first ~5s window (after process
				// start, before the goroutine's first probe
				// completes) was returning rc=0 + did=non-empty
				// and tripping the rc<=0 degraded branch.
				return l2.DID(), 0, 0, 0, errSubmitterWarmup
			}
			return snap.DID, snap.BalanceHbdCents, snap.RcRemaining, snap.ConsecutiveFails, snap.Err
		}
	}

	trustedProxies := splitCSV(args.trustedProxies)
	if len(trustedProxies) > 0 {
		slog.Info("trusted reverse proxies configured", "hosts", trustedProxies)
	}
	srv, err := NewServer(ServerConfig{
		PrimaryPubKeyHex:  args.primaryPubKey,
		BackupPubKeyHex:   args.backupPubKey,
		Network:           args.network,
		ChainID:           args.chainID,
		SessionTTL:        time.Duration(args.sessionTTLMinutes) * time.Minute,
		Signer:            signer,
		Dashd:             dashd,
		Orch:              orch,
		Sessions:          sessions,
		BroadcasterHealth: broadcasterHealth,
		DashdHealth:       dashdHealthFn,
		SubmitterHealth:   submitterHealthFn,
		TrustedProxies:    trustedProxies,
	})
	if err != nil {
		slog.Error("server config invalid", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", args.port),
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// dashd watcher goroutine — polls dashd RPC + fires IS-observed
	// transitions. Stops when ctx is cancelled.
	if dashd != nil {
		go func() {
			// Round-3 audit OP-011: errors.Is so a future wrap doesn't
			// turn shutdown into spurious slog.Error.
			if err := dashd.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("dashd watcher failed", "err", err)
			}
		}()
	}

	// Session-prune janitor — minimal overhead, keeps memory bounded
	// against any forgotten-to-cancel sessions.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				removed, prunedAddrs := srv.sessions.Prune()
				if removed > 0 {
					slog.Debug("pruned expired sessions", "count", removed)
				}
				// Audit R2-N3: also Unwatch the dashd entries for
				// pruned sessions. Without this, every TTL-expired
				// session permanently leaks a watcher map entry.
				if dashd != nil {
					for _, addr := range prunedAddrs {
						dashd.Unwatch(addr)
					}
				}
			}
		}
	}()

	// Round-5 audit R5-OP-02/R5-OP-06: background submitter-health
	// refresher. The /healthz hook reads the atomic snapshot this
	// publishes — no synchronous GraphQL on the probe path.
	if submitterMonitor != nil {
		go submitterMonitor.Run(ctx)
	}

	// Round-5 audit R5-OP-05: cache janitor. The validator-set cache
	// can accumulate entries when epochFor() varies; drop entries
	// whose expiresAt is more than 10 minutes in the past.
	if validatorSetCacheRef != nil {
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					removed := validatorSetCacheRef.GC(10 * time.Minute)
					if removed > 0 {
						slog.Debug("validator-set cache GC removed entries",
							"count", removed)
					}
				}
			}
		}()
	}

	slog.Info("is-service listening",
		"port", args.port,
		"network", args.network,
		"chainID", args.chainID,
	)

	serverErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		slog.Error("http server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Order matters (audit `orchestrator-detached-context-on-shutdown`):
		//   1. HTTP server: stop accepting new requests.
		//   2. Drain spawned Drive goroutines so any in-flight
		//      mapInstantSendV2 submissions complete OR honour the
		//      drain deadline.
		//   3. ONLY THEN close the p2p broadcaster — otherwise an
		//      in-flight BroadcastRequest fails publish-on-closed-topic.
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
			os.Exit(1)
		}
		// Drain deadline: configurable, must cover the full Drive
		// budget = CollectTimeout(15s) + SubmitTimeout(30s) +
		// reconcileL2 worst-case (~180s with default backoffs +
		// per-poll timeouts). Round-3 audit R3-05/CSM-04/OP-002
		// raised the default from 60s after the round-2 reconciler
		// extended the budget without updating the drain default.
		drainTimeout := time.Duration(args.drainTimeoutSeconds) * time.Second
		const minSafeDrain = 225 * time.Second
		if drainTimeout < minSafeDrain {
			slog.Warn("drainTimeoutSeconds shorter than orchestrator full budget (collect+submit+reconcile=~225s); in-flight L2 reconciles may be cut, producing L2-credit-but-FORWARD_FAILED divergence",
				"drainTimeoutSeconds", args.drainTimeoutSeconds,
				"recommendedMinSeconds", int(minSafeDrain/time.Second))
		}
		srv.Drain(drainTimeout)
		if p2pCloseFn != nil {
			_ = p2pCloseFn()
		}
		slog.Info("shutdown complete")
	}
}

// splitCSV breaks a comma-separated config value, trimming spaces and
// discarding empties. Returns nil for "" input.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
