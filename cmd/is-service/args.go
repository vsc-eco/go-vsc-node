package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
)

// validateOperatorURL enforces that operator-supplied URL flags
// carry an explicit http/https scheme + host, do not embed userinfo,
// and do not carry a query string or fragment. The IS service prefers
// network-trust boundaries over bearer-in-URL; credentials anywhere
// in the URL would also leak through structured logs even with
// sanitizeURLForLog active (R7-OP-01-logleak / R8-SEC-01).
// Round-9 audit R9-INFO-SCHEME-01 + R9-SEC-QUERY-01 tightened the
// scheme allow-list and added query/fragment rejection. Returns a
// fatal startup error rather than silently sanitising.
func validateOperatorURL(flag, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a parseable URL: %w", flag, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s must include an explicit scheme and host (got %q)", flag, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s scheme must be http or https (got %q)", flag, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%s must NOT embed credentials in the URL (user:pass@); use a network-trust boundary instead", flag)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must NOT include a query string or fragment; secrets carried that way still reach upstream callers", flag)
	}
	// Round-10 audit R10-SEC-PATH-SMUGGLE-01 + round-11 audit
	// R11-INFO-PATH-SMUGGLE-DEFENSE-NARROW + round-12 audit
	// R12-DRIFT-ARGS-COMMENT-SEMICOLON: defense in depth. An
	// operator who percent-encodes any control byte, '?' / '#',
	// or matrix-style ';' into the path slips past the RawQuery /
	// Fragment gate above. The current logging path drops Path
	// anyway (sanitizeURLForLog), but reject at parse time so a
	// future sanitiser regression can't surface them. R12 closed
	// the code-vs-comment drift by adding the ';' check the
	// docstring promised but R11 had skipped.
	for i := 0; i < len(u.Path); i++ {
		c := u.Path[i]
		if c < 0x20 || c == 0x7f || c == '?' || c == '#' || c == ';' {
			return fmt.Errorf("%s path must not contain encoded delimiters (?, #, ;) or control bytes", flag)
		}
	}
	return nil
}

type args struct {
	debug               bool
	port                int
	network             string // "mainnet" or "testnet"
	chainID             string // "vsc-mainnet" or "vsc-testnet"
	primaryPubKey       string
	backupPubKey        string
	addressSignerSecret         string
	addressSignerEd25519KeyFile string
	sessionTTLMinutes   int
	dashdRPCURL         string
	dashdRPCUser        string
	dashdRPCPassword    string
	// L2 submitter — when l2GqlURL + l2PrivKeyHex are both set, the IS
	// service will actually post mapInstantSendV2 transactions instead
	// of using the log-only stub. The submitter's derived DID needs to
	// be HBD-funded for RC.
	l2GqlURL              string
	l2PrivKeyHex          string
	l2DashMappingContract string
	l2RcLimit             int64
	// libp2p broadcaster — when p2pBootstrapPeers is set, the IS
	// service joins the islock-attestation gossip topic via direct
	// libp2p (no full validator machinery). Without it, the noop
	// broadcaster is used and no attestations are gathered.
	p2pBootstrapPeers string
	p2pListenAddrs    string
	// drainTimeoutSeconds: graceful-shutdown soft deadline. Must be >=
	// orchestrator.collectTimeout + submitTimeout.
	drainTimeoutSeconds int
	// trustedProxies: comma-separated proxy hosts. Audit TC2-06.
	trustedProxies string
	// validatorSetCacheTTLSeconds: TTL for the per-epoch validator-set
	// cache. Round-5 audit R5-DRIFT-06 — made operator-configurable so
	// admin rotation latency is tunable in production. Default 30s.
	validatorSetCacheTTLSeconds int
	// testBypassDashdISLock: when true, the dashd watcher treats every
	// observed tx as IS-locked regardless of dashd's `instantlock`
	// field. **TEST-ONLY** — gated to `-network=devnet` at startup so
	// production deploys cannot enable it. Used by tests/devnet's
	// IS-login E2E because regtest dashd doesn't have an LLMQ quorum
	// to produce real IS-locks.
	testBypassDashdISLock bool
}

func parseArgs() (args, error) {
	a := args{}
	fs := flag.NewFlagSet("is-service", flag.ContinueOnError)

	fs.BoolVar(&a.debug, "debug", false, "verbose logging")
	fs.IntVar(&a.port, "port", 3030, "HTTP port to listen on")
	fs.StringVar(&a.network, "network", "testnet", "Dash network: mainnet or testnet")
	fs.StringVar(&a.chainID, "chainID", "", "Magi chain ID, e.g. vsc-mainnet / vsc-testnet (defaults derived from -network)")
	fs.StringVar(&a.primaryPubKey, "primaryPubkey", "",
		"hex-encoded 33-byte compressed pubkey for the bridge TSS primary key")
	fs.StringVar(&a.backupPubKey, "backupPubkey", "",
		"hex-encoded 33-byte compressed pubkey for the bridge TSS backup key")
	fs.StringVar(&a.addressSignerSecret, "addressSignerSecret", "",
		"HMAC secret for signing (deposit_address, instruction) tuples in API responses. "+
			"DEV/TEST ONLY — production must replace with -addressSignerEd25519KeyFile "+
			"or an HSM/KMS-backed signer per spec §5.7.")
	fs.StringVar(&a.addressSignerEd25519KeyFile, "addressSignerEd25519KeyFile", "",
		"Path to a 32-byte Ed25519 seed (hex-encoded, 0o600). Production-shape "+
			"asymmetric address signer — Altera pins the derived public key in "+
			"PUBLIC_IS_SERVICE_SIGNER_PUBKEY and verifies each /session/start's "+
			"addressSignature. Takes precedence over -addressSignerSecret. "+
			"HSM/KMS-backed implementation is the next workstream and will share "+
			"the AddressSigner interface.")
	fs.IntVar(&a.sessionTTLMinutes, "sessionTTLMinutes", 30, "how long sessions stay active before expiry")

	fs.StringVar(&a.dashdRPCURL, "dashdRPC", "",
		"dashd JSON-RPC URL for IS-lock observation (optional; e.g. http://vsc-dashd-testnet:9998). "+
			"When unset, IS_OBSERVED transitions must be driven externally.")
	fs.StringVar(&a.dashdRPCUser, "dashdRPCUser", "vsc-node-user", "dashd RPC username")
	fs.StringVar(&a.dashdRPCPassword, "dashdRPCPassword", "vsc-node-pass", "dashd RPC password")

	fs.StringVar(&a.l2GqlURL, "l2GqlURL", "",
		"VSC GraphQL endpoint for L2 mapInstantSendV2 submission "+
			"(e.g. https://api.vsc.eco/api/v1/graphql). When unset, the IS service runs "+
			"with the log-only submitter (no on-chain effect).")
	fs.StringVar(&a.l2PrivKeyHex, "l2PrivKey", "",
		"L2 signing private key (hex-encoded secp256k1). The derived did:pkh:eip155 "+
			"needs HBD to pay per-tx RC. Required to enable real L2 submission.")
	fs.StringVar(&a.l2DashMappingContract, "l2DashMappingContract", "",
		"dash-mapping-contract id (vsc1...) for the chain we're submitting to. "+
			"Required when l2GqlURL+l2PrivKey are set.")
	fs.Int64Var(&a.l2RcLimit, "l2RcLimit", 1000,
		"Per-tx RC budget (1000 RC = 1 HBD). 1000 is a safe upper bound for "+
			"mapInstantSendV2; lower to save RC, raise if txs abort on RC.")

	fs.StringVar(&a.p2pBootstrapPeers, "p2pBootstrapPeers", "",
		"Comma-separated list of libp2p multiaddrs (e.g. /ip4/x.x.x.x/tcp/p/p2p/<id>) "+
			"to bootstrap into the validator p2p mesh. When set, the IS service joins "+
			"the islock-attestation gossip topic and uses real broadcaster + response "+
			"collector. When unset, falls back to the no-op broadcaster (no on-network effect).")
	fs.StringVar(&a.p2pListenAddrs, "p2pListenAddrs", "",
		"Comma-separated libp2p listen multiaddrs. Defaults to /ip4/0.0.0.0/tcp/0 + /ip6/::/tcp/0.")

	fs.StringVar(&a.trustedProxies, "trustedProxies", "",
		"Comma-separated host/IP strings of trusted reverse proxies; X-Forwarded-For "+
			"from these is honoured for rate-limiting. Loopback is always trusted. "+
			"Audit TC2-06.")

	fs.IntVar(&a.validatorSetCacheTTLSeconds, "validatorSetCacheTTLSeconds", 30,
		"TTL in seconds for the per-epoch validator-set cache (R4-001 + "+
			"R5-DRIFT-06). Lower = faster reflection of admin-side rotations; "+
			"higher = fewer L2 GraphQL probes per Drive(). Default 30s.")

	fs.BoolVar(&a.testBypassDashdISLock, "testBypassDashdISLock", false,
		"TEST ONLY: treat every dashd-observed tx as IS-locked, "+
			"bypassing the `instantlock` field check. Gated to "+
			"-network=devnet at startup. Used by tests/devnet's "+
			"IS-login E2E because regtest dashd doesn't have an LLMQ "+
			"quorum to produce real IS-locks.")

	fs.IntVar(&a.drainTimeoutSeconds, "drainTimeoutSeconds", 240,
		"Graceful-shutdown drain timeout in seconds. MUST be >= orchestrator's "+
			"CollectTimeout(15s) + SubmitTimeout(30s) + reconcileL2 budget(~180s including "+
			"sleeps+per-poll timeouts) so in-flight L2 reconciles complete instead of leaving "+
			"sessions L2-credited-but-locally-failed. Default 240s. Round-3 audit R3-05.")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return a, err
	}

	if a.primaryPubKey == "" || a.backupPubKey == "" {
		return a, fmt.Errorf("-primaryPubkey and -backupPubkey are required")
	}
	// Round-3 audit OP-010: when both -network and -chainID are
	// explicit, enforce the pair. A misconfig like -network=mainnet
	// -chainID=vsc-testnet stamps payloads with the wrong chain_id and
	// every session times out in ATTESTATION_TIMEOUT while subsystem
	// logs look green (reads like a network outage).
	switch a.network {
	case "mainnet":
		if a.chainID == "" {
			a.chainID = "vsc-mainnet"
		} else if a.chainID != "vsc-mainnet" {
			return a, fmt.Errorf("-network=mainnet requires -chainID=vsc-mainnet, got %q", a.chainID)
		}
	case "testnet":
		if a.chainID == "" {
			a.chainID = "vsc-testnet"
		} else if a.chainID != "vsc-testnet" {
			return a, fmt.Errorf("-network=testnet requires -chainID=vsc-testnet, got %q", a.chainID)
		}
	case "devnet":
		// Devnet support is for tests/devnet/* multi-node E2E runs
		// only — vsc-devnet is the net_id devnet-setup stamps onto
		// L2 broadcasts. Production deploys MUST use mainnet or
		// testnet; the default-deny on unknown -network values
		// guards against an operator typo silently shipping under
		// the wrong chain.
		if a.chainID == "" {
			a.chainID = "vsc-devnet"
		} else if a.chainID != "vsc-devnet" {
			return a, fmt.Errorf("-network=devnet requires -chainID=vsc-devnet, got %q", a.chainID)
		}
	default:
		return a, fmt.Errorf("-network must be 'mainnet', 'testnet' or 'devnet', got %q", a.network)
	}
	// Test-only bypass: refuse to start with -testBypassDashdISLock=true
	// when -network isn't devnet. This is a defense-in-depth gate so a
	// production operator who flips the flag by mistake (or via a
	// compromised orchestrator) can't accidentally short-circuit the
	// IS-lock check on a live deployment.
	if a.testBypassDashdISLock && a.network != "devnet" {
		return a, fmt.Errorf(
			"-testBypassDashdISLock=true requires -network=devnet (got %q); " +
				"the flag is gated to the devnet-only test surface", a.network)
	}
	if a.port < 1 || a.port > 65535 {
		return a, fmt.Errorf("-port must be between 1 and 65535")
	}
	if a.sessionTTLMinutes < 1 {
		return a, fmt.Errorf("-sessionTTLMinutes must be > 0")
	}
	// Round-6 audit R6-OP-04: validate -validatorSetCacheTTLSeconds.
	// The previous behaviour silently coerced <=0 to 30s, which lets
	// an operator who passes 0 thinking it disables caching get the
	// 30s default with no signal.
	if a.validatorSetCacheTTLSeconds < 1 {
		return a, fmt.Errorf("-validatorSetCacheTTLSeconds must be > 0")
	}
	// Round-8 audit R8-SEC-01: reject URL flags that embed
	// credentials or lack an explicit scheme. Pre-R8 a misconfigured
	// -l2GqlURL=user:pass@host bypassed sanitizeURLForLog and
	// leaked through startup logs.
	if err := validateOperatorURL("-dashdRPC", a.dashdRPCURL); err != nil {
		return a, err
	}
	if err := validateOperatorURL("-l2GqlURL", a.l2GqlURL); err != nil {
		return a, err
	}
	return a, nil
}
