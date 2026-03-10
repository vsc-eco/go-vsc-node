package tss

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
	"vsc-node/modules/common"
)

// TurnCredentials represents a single issued TURN credential set for a given
// Hive account and (optionally) TSS session.
type TurnCredentials struct {
	Username  string
	Password  string
	Realm     string
	Servers   []string
	ExpiresAt time.Time
}

// TurnService issues TURN credentials that are derived from the node's
// Hive identity plus a shared HMAC secret. The credentials are suitable
// for TURN servers that use the long-term credential mechanism.
type TurnService struct {
	conf     TurnConfig
	identity common.IdentityConfig
}

// globalTurnService is intentionally package-private and configured
// explicitly from the main binary. This keeps TSS logic decoupled from
// how TURN is deployed while still making the credential service
// discoverable over the existing TSS RPC interface.
var globalTurnService *TurnService

// ConfigureTurnService initialises the global TURN credential service.
// It should be called once during node startup from the main package.
//
// When the config is disabled or misconfigured, the service is left nil
// and callers of the RPC helper will receive an error instead of
// credentials.
func ConfigureTurnService(conf TurnConfig, id common.IdentityConfig) error {
	if conf == nil {
		globalTurnService = nil
		return nil
	}

	// Ensure config is loaded from disk.
	if err := conf.Init(); err != nil {
		return err
	}

	cfg := conf.Get()
	if !cfg.Enabled {
		// Service explicitly disabled; keep global nil so RPCs are a no-op.
		globalTurnService = nil
		return nil
	}

	if cfg.Secret == "" {
		return fmt.Errorf("tss TURN config is enabled but secret is empty")
	}

	globalTurnService = &TurnService{
		conf:     conf,
		identity: id,
	}
	return nil
}

// GenerateCredentials derives a short-lived TURN username/password pair
// for the node's Hive account. The username encodes the account name and
// expiry, and the password is an HMAC over the username using the shared
// secret from config.
//
// The optional sessionId can be used to scope credentials to a specific
// TSS session, but it is not required by the TURN protocol itself.
func (s *TurnService) GenerateCredentials(sessionId string) (TurnCredentials, error) {
	cfg := s.conf.Get()
	hiveUser := s.identity.Get().HiveUsername
	if hiveUser == "" {
		return TurnCredentials{}, fmt.Errorf("hive username is not configured")
	}

	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	expUnix := expiresAt.Unix()

	// Basic username format compatible with long-term TURN creds:
	// <account>:<expiry>[:<sessionId>]
	username := fmt.Sprintf("%s:%d", hiveUser, expUnix)
	if sessionId != "" {
		username = username + ":" + sessionId
	}

	mac := hmac.New(sha256.New, []byte(cfg.Secret))
	if _, err := mac.Write([]byte(username)); err != nil {
		return TurnCredentials{}, err
	}
	password := hex.EncodeToString(mac.Sum(nil))

	return TurnCredentials{
		Username:  username,
		Password:  password,
		Realm:     cfg.Realm,
		Servers:   append([]string{}, cfg.Servers...),
		ExpiresAt: expiresAt,
	}, nil
}

