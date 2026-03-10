package tss

import (
	"vsc-node/modules/config"
)

// TurnCredentialConfig defines config for issuing TURN credentials that are
// derived from a Hive account name plus a shared secret.
//
// This does not run a TURN server by itself; it only produces credentials
// that can be used with a TURN implementation that supports the long-term
// credential mechanism (username + HMAC password).
type TurnCredentialConfig struct {
	// Enabled gates the TURN credential service entirely.
	// When false, no credentials will be issued.
	Enabled bool `json:"enabled"`

	// Realm is the TURN realm string. It must match the realm configured
	// on the TURN server in order for credentials to be accepted.
	Realm string `json:"realm"`

	// Secret is the shared HMAC key used to derive TURN passwords from usernames.
	// It should be a high-entropy string and treated like a secret.
	Secret string `json:"secret"`

	// Servers is the list of TURN server URIs (e.g. turn:host:port?transport=udp)
	// that clients should try when establishing relayed connectivity.
	Servers []string `json:"servers"`

	// TTLSeconds controls how long issued credentials remain valid.
	// Defaults to 10 minutes if set to zero or negative.
	TTLSeconds int `json:"ttl_seconds"`
}

type turnCredentialConfigStruct struct {
	*config.Config[TurnCredentialConfig]
}

// TurnConfig is the exported handle type used by other packages.
type TurnConfig = *turnCredentialConfigStruct

// NewTurnConfig creates a new config wrapper for TURN credentials, stored
// alongside other node configuration in the data directory.
func NewTurnConfig(dataDir ...string) TurnConfig {
	var dataDirPtr *string
	if len(dataDir) > 0 {
		dataDirPtr = &dataDir[0]
	}

	return &turnCredentialConfigStruct{config.New(TurnCredentialConfig{
		Enabled:    false,
		Realm:      "vsc-tss",
		Secret:     "",
		Servers:    []string{},
		TTLSeconds: 600,
	}, dataDirPtr)}
}

