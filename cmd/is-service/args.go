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

	if err := fs.Parse(os.Args[1:]); err != nil {
		return a, err
	}

	if a.primaryPubKey == "" || a.backupPubKey == "" {
		return a, fmt.Errorf("-primaryPubkey and -backupPubkey are required")
	}
	switch a.network {
	case "mainnet":
		if a.chainID == "" {
			a.chainID = "vsc-mainnet"
		}
	case "testnet":
		if a.chainID == "" {
			a.chainID = "vsc-testnet"
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
