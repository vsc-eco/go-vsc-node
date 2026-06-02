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

	srv, err := NewServer(ServerConfig{
		PrimaryPubKeyHex: args.primaryPubKey,
		BackupPubKeyHex:  args.backupPubKey,
		Network:          args.network,
		ChainID:          args.chainID,
		SessionTTL:       time.Duration(args.sessionTTLMinutes) * time.Minute,
		Signer:           signer,
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
				if removed := srv.sessions.Prune(); removed > 0 {
					slog.Debug("pruned expired sessions", "count", removed)
				}
			}
		}
	}()

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
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
			os.Exit(1)
		}
		slog.Info("shutdown complete")
	}
}
