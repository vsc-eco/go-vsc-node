package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// LOW 71: a generated L2 key is kept in a 0600 key file and never logged, and
// the next start loads the same key.
func TestLOW71_GeneratedKeyIsSavedNotLogged(t *testing.T) {
	t.Setenv("BOT_ETH_PRIVKEY", "")
	t.Setenv("BOT_ETH_KEY_FILE", "")
	logs := captureLogs(t)
	cfg := EVMBotConfig{CheckpointFile: t.TempDir() + "/cp.json"}
	priv, did := initEthKey(cfg)
	keyHex := hex.EncodeToString(ethCrypto.FromECDSA(priv))
	if strings.Contains(logs.String(), keyHex) {
		t.Fatal("the generated private key was written to the log")
	}
	st, err := os.Stat(cfg.CheckpointFile + ".key")
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("key file: %v, mode %v; want it saved 0600", err, st)
	}
	if b, _ := os.ReadFile(cfg.CheckpointFile + ".key"); string(b) != keyHex {
		t.Fatal("key file does not hold the generated key")
	}
	if _, did2 := initEthKey(cfg); did2 != did {
		t.Fatalf("restart loaded %s, want the saved key's %s", did2, did)
	}
}

// B17: without GRAPHQL_URLS the bot exits instead of using a dead default.
func TestB17_MissingGraphQLURLsStopsTheBot(t *testing.T) {
	if os.Getenv("B17_CHILD") == "1" {
		parseConfig()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestB17_MissingGraphQLURLsStopsTheBot")
	cmd.Env = append(os.Environ(), "B17_CHILD=1", "GRAPHQL_URLS=", "VAULT_ADDRESS=0x00000000000000000000000000000000000000fa", "CONTRACT_ID=vsc1bridge")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "GRAPHQL_URLS must be set") {
		t.Fatalf("parseConfig without GRAPHQL_URLS: err=%v output=%s", err, out)
	}
}

// B10: an unregistered relayer DID is reported at startup.
func TestB10_UnregisteredRelayerIsReported(t *testing.T) {
	const did = "did:pkh:eip155:1:0x00000000000000000000000000000000000000B0"
	for registered, wantErr := range map[bool]bool{false: true, true: false} {
		l2 := newFakeL2()
		if registered {
			l2.set("vsc1bridge", "rl-"+did, "1")
		}
		logs := captureLogs(t)
		checkRelayerRegistered(context.Background(), l2.gql(t), "vsc1bridge", did)
		if got := strings.Contains(logs.String(), "not a registered relayer"); got != wantErr {
			t.Errorf("registered=%v: error logged=%v, want %v", registered, got, wantErr)
		}
	}
}

// A malformed TOKENS entry stops the bot at startup: skipped, its deposits
// would never be mapped.
func TestTokensEnv_MalformedEntryStopsTheBot(t *testing.T) {
	if os.Getenv("TOKENS_CHILD") == "1" {
		parseConfig()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestTokensEnv_MalformedEntryStopsTheBot")
	cmd.Env = append(os.Environ(), "TOKENS_CHILD=1", "GRAPHQL_URLS=http://127.0.0.1:1", "CONTRACT_ID=vsc1bridge", "NETWORK=testnet", "TOKENS=0x1c7d4b196cb0c7b01d743fbc6116a902379c7238,0x12")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "bad TOKENS") || !strings.Contains(string(out), `\"0x12\"`) {
		t.Fatalf("parseConfig with a malformed TOKENS entry: err=%v output=%s", err, out)
	}
}
