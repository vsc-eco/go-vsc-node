package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
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
	debug                       bool
	port                        int
	network                     string // "mainnet", "testnet", or "devnet" (test-only)
	chainID                     string // "vsc-mainnet", "vsc-testnet", or "vsc-devnet" (test-only)
	primaryPubKey               string
	backupPubKey                string
	addressSignerSecret         string
	addressSignerEd25519KeyFile string
	// Vault Transit signer (recommended for production per spec
	// §5.7). When -signerVaultAddr is set, the IS service signs
	// each /session/start's addressSignature via a single
	// transit/sign HTTP call. The private key never leaves Vault.
	signerVaultAddr      string
	signerVaultMount     string // default "transit"
	signerVaultKeyName   string
	signerVaultToken     string // discouraged inline; prefer TokenFile
	signerVaultTokenFile string
	sessionTTLMinutes    int
	dashdRPCURL          string
	dashdRPCUser         string
	dashdRPCPassword     string
	dashdRPCPasswordFile string // audit M15: file alt
	// L2 submitter — when l2GqlURL + l2PrivKeyHex are both set, the IS
	// service will actually post mapInstantSendV2 transactions instead
	// of using the log-only stub. The submitter's derived DID needs to
	// be HBD-funded for RC.
	l2GqlURL              string
	l2PrivKeyHex          string
	l2PrivKeyFile         string // audit M15: file alt for l2PrivKey
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
	fs.StringVar(&a.network, "network", "testnet",
		"Dash network: mainnet | testnet | devnet (devnet is test-only; "+
			"used by tests/devnet's IS-login E2E with a regtest dashd)")
	fs.StringVar(&a.chainID, "chainID", "",
		"Magi chain ID: vsc-mainnet | vsc-testnet | vsc-devnet "+
			"(defaults derived from -network; devnet is test-only)")
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
			"For full HSM-backed signing (spec §5.7), use -signerVaultAddr instead.")
	fs.StringVar(&a.signerVaultAddr, "signerVaultAddr", "",
		"HashiCorp Vault address (e.g. https://vault.internal:8200). When set, "+
			"the IS service signs each /session/start's addressSignature via "+
			"Vault's transit/sign API. Takes precedence over the file-based "+
			"Ed25519 signer + the HMAC stub. The private key never leaves Vault.")
	fs.StringVar(&a.signerVaultMount, "signerVaultMount", "transit",
		"Vault transit-engine mount path. Default 'transit'.")
	fs.StringVar(&a.signerVaultKeyName, "signerVaultKeyName", "",
		"Vault transit key name (must be type=ed25519). Required when "+
			"-signerVaultAddr is set.")
	fs.StringVar(&a.signerVaultToken, "signerVaultToken", "",
		"Vault token (DEV/TEST ONLY — REJECTED on any -network != devnet "+
			"per audit SEC-6 / R16-SEC-sec6-testnet-not-gated, because "+
			"the literal value leaks via /proc/<pid>/cmdline, ps, systemd "+
			"journal, and orchestrator inspect surfaces. Mainnet AND real "+
			"testnet both run on production-shape infrastructure with the "+
			"same leak surface). Always prefer -signerVaultTokenFile or "+
			"VAULT_TOKEN env for production.")
	fs.StringVar(&a.signerVaultTokenFile, "signerVaultTokenFile", "",
		"Path to a file containing the Vault token (preferred). Whitespace "+
			"is trimmed. File permissions are NOT checked because Vault tokens "+
			"are short-lived rotatable secrets, but operators should still "+
			"set 0o600.")
	fs.IntVar(&a.sessionTTLMinutes, "sessionTTLMinutes", 30, "how long sessions stay active before expiry")

	fs.StringVar(&a.dashdRPCURL, "dashdRPC", "",
		"dashd JSON-RPC URL for IS-lock observation (optional; e.g. http://vsc-dashd-testnet:9998). "+
			"When unset, IS_OBSERVED transitions must be driven externally.")
	fs.StringVar(&a.dashdRPCUser, "dashdRPCUser", "vsc-node-user", "dashd RPC username")
	fs.StringVar(&a.dashdRPCPassword, "dashdRPCPassword", "vsc-node-pass",
		"dashd RPC password (DEV/TEST default; production should use "+
			"-dashdRPCPasswordFile per audit M15 to keep the secret out of /proc/cmdline + journal)")
	fs.StringVar(&a.dashdRPCPasswordFile, "dashdRPCPasswordFile", "",
		"Path to a file containing the dashd RPC password (audit M15 file alt for "+
			"-dashdRPCPassword; preferred for production). Whitespace is trimmed.")

	fs.StringVar(&a.l2GqlURL, "l2GqlURL", "",
		"VSC GraphQL endpoint for L2 mapInstantSendV2 submission "+
			"(e.g. https://api.vsc.eco/api/v1/graphql). When unset, the IS service runs "+
			"with the log-only submitter (no on-chain effect).")
	fs.StringVar(&a.l2PrivKeyHex, "l2PrivKey", "",
		"L2 signing private key (hex-encoded secp256k1). The derived did:pkh:eip155 "+
			"needs HBD to pay per-tx RC. Required to enable real L2 submission. "+
			"DEV/TEST inline form; production should use -l2PrivKeyFile per audit M15.")
	fs.StringVar(&a.l2PrivKeyFile, "l2PrivKeyFile", "",
		"Path to a file containing the L2 signing private key (audit M15 file alt for "+
			"-l2PrivKey; preferred for production). File permissions are NOT checked "+
			"but operators should set 0o600. Whitespace is trimmed.")
	fs.StringVar(&a.l2DashMappingContract, "l2DashMappingContract", "",
		"dash-mapping-contract id (vsc1...) for the chain we're submitting to. "+
			"Required when l2GqlURL+l2PrivKey are set.")
	fs.Int64Var(&a.l2RcLimit, "l2RcLimit", 10000,
		"Per-tx RC budget (1000 RC = 1 HBD). Default 10000 (10 HBD). The "+
			"op=call path (mapInstantSendV2 → dispatchForward → forwarder. "+
			"execute → target ContractCallAs) burns through 1000 quickly — "+
			"devnet observations of `err=gas_limit_hit / errMsg=cost limit "+
			"exceeded` on the mapInstantSendV2 contract output were caused "+
			"by the prior 1000-default cap. op=auth still fits comfortably "+
			"under the new ceiling. Operators can lower this for op=auth-"+
			"only deployments; raise further if downstream targets are "+
			"unusually expensive.")

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
			"-testBypassDashdISLock=true requires -network=devnet (got %q); "+
				"the flag is gated to the devnet-only test surface", a.network)
	}
	// Audit SEC-6 (R15) + R16-SEC-sec6-testnet-not-gated (MED):
	// refuse to start with -signerVaultToken=<literal> on any
	// production-shape deployment (mainnet OR real testnet). A
	// literal token in argv leaks via /proc/<pid>/cmdline
	// (world-readable on default Linux), `ps auxw`, systemd journal,
	// and container-orchestrator inspect surfaces (k8s describe,
	// docker inspect). Within the token's TTL anyone with shell
	// access to a co-tenant container or read access to systemd
	// journal can sign arbitrary deposit addresses. R15 originally
	// gated only mainnet; the real testnet runs against operator
	// infrastructure too (not just a dev laptop) so the same leak
	// surface applies. Devnet is the only acceptably-local mode
	// and keeps the literal path for ergonomic local testing.
	if a.signerVaultToken != "" && a.network != "devnet" {
		return a, fmt.Errorf(
			"-signerVaultToken=<literal> is forbidden when -network=%s — "+
				"the token would leak via /proc/<pid>/cmdline, ps, "+
				"systemd journal, and container-orchestrator inspect surfaces. "+
				"Use -signerVaultTokenFile=<path> (the only production-safe option; "+
				"supports vault-agent rotation).",
			a.network)
	}
	// Audit R17-SEC-sec6-testnet-gate-env-var-leak-surface-acknowledged-
	// but-not-mitigated (INFO → enforced): the R16 widened gate refused
	// the literal flag on non-devnet but left VAULT_TOKEN env as the
	// "safer" alternative. Env vars also leak via kubectl describe and
	// docker inspect, so on production-shape orchestrators (k8s, ECS,
	// systemd-managed VMs) the env path is no better than the argv path.
	// Refuse VAULT_TOKEN env outside devnet UNLESS -signerVaultTokenFile
	// is also configured (then env is unused by ResolveVaultToken's
	// precedence rules + the check is moot).
	if a.signerVaultAddr != "" &&
		a.signerVaultTokenFile == "" &&
		a.network != "devnet" &&
		os.Getenv("VAULT_TOKEN") != "" {
		return a, fmt.Errorf(
			"VAULT_TOKEN env var present without -signerVaultTokenFile when "+
				"-network=%s — env vars leak via kubectl describe, docker "+
				"inspect, and /proc/<pid>/environ. Set -signerVaultTokenFile="+
				"<path> instead (vault-agent sink-file pattern is the "+
				"recommended ops shape). Devnet is the only network that "+
				"accepts the env path.", a.network)
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

	// Audit M15: resolve file-based secret alternatives. Each file
	// is read once at startup, trimmed of whitespace, and treated as
	// the secret's inline equivalent. The file-form is preferred for
	// production because the secret doesn't appear in /proc/cmdline,
	// `ps`, or journald argv captures.
	if a.dashdRPCPasswordFile != "" {
		if a.dashdRPCPassword != "" && a.dashdRPCPassword != "vsc-node-pass" {
			return a, fmt.Errorf("-dashdRPCPassword and -dashdRPCPasswordFile are mutually exclusive")
		}
		b, err := os.ReadFile(a.dashdRPCPasswordFile)
		if err != nil {
			return a, fmt.Errorf("could not read -dashdRPCPasswordFile: %w", err)
		}
		a.dashdRPCPassword = strings.TrimSpace(string(b))
		if a.dashdRPCPassword == "" {
			return a, fmt.Errorf("-dashdRPCPasswordFile is empty")
		}
	}
	if a.l2PrivKeyFile != "" {
		if a.l2PrivKeyHex != "" {
			return a, fmt.Errorf("-l2PrivKey and -l2PrivKeyFile are mutually exclusive")
		}
		b, err := os.ReadFile(a.l2PrivKeyFile)
		if err != nil {
			return a, fmt.Errorf("could not read -l2PrivKeyFile: %w", err)
		}
		a.l2PrivKeyHex = strings.TrimSpace(string(b))
		if a.l2PrivKeyHex == "" {
			return a, fmt.Errorf("-l2PrivKeyFile is empty")
		}
	}

	return a, nil
}
