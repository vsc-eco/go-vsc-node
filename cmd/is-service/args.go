package main

import (
	"flag"
	"fmt"
	"os"
)

type args struct {
	debug               bool
	port                int
	network             string // "mainnet" or "testnet"
	chainID             string // "vsc-mainnet" or "vsc-testnet"
	primaryPubKey       string
	backupPubKey        string
	addressSignerSecret string
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
			"DEV/TEST ONLY — production must replace with HSM/KMS asymmetric signer (see §5.7).")
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

	fs.IntVar(&a.drainTimeoutSeconds, "drainTimeoutSeconds", 240,
		"Graceful-shutdown drain timeout in seconds. MUST be >= orchestrator's "+
			"CollectTimeout(15s) + SubmitTimeout(30s) + reconcileL2 budget(~180s including " +
			"sleeps+per-poll timeouts) so in-flight L2 reconciles complete instead of leaving " +
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
	default:
		return a, fmt.Errorf("-network must be 'mainnet' or 'testnet', got %q", a.network)
	}
	if a.port < 1 || a.port > 65535 {
		return a, fmt.Errorf("-port must be between 1 and 65535")
	}
	if a.sessionTTLMinutes < 1 {
		return a, fmt.Errorf("-sessionTTLMinutes must be > 0")
	}
	return a, nil
}
