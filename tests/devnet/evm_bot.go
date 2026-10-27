//go:build evm_devnet

// evm-mapping-bot runner for the L1-bridge waves. Launches the real bot binary
// (bot-pr154) against (anvil L1, devnet GQL, the deployed contract) so its
// REAL deposit-relay (buildMapPayload/MPT) and withdrawal-close-out
// (buildConfirmSpendPayload/encodeReceiptRLP = the H-F1/H-F2/CC-1 code) run
// end-to-end. The binary is built once to /tmp/evm-mapping-bot.
package devnet

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// BotConfig is the env for one bot process.
type BotConfig struct {
	EthRPC     string // anvil URL
	VaultAddr  string // 0x..20-byte
	ContractID string
	GraphQLURL string // a devnet node GQL endpoint
	EthPrivHex string // 64-hex L2 relayer key (its DID must be RC-funded)
	Checkpoint string // checkpoint file path
	Tokens     string // TOKENS: ERC-20 addresses to watch (comma-separated)
	HealthAddr string // HEALTH_ADDR: serve GET /health here (with alarms checked every 10s)
	StartBlock string // START_BLOCK for a new checkpoint (default 1: an anvil chain is short)
	NeedsFile  string // NEEDS_FILE: where the bot asks the operator for a proof (RP-9)
}

// Bot is a running bot process with captured logs.
type Bot struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	logs []string
}

// StartBot launches the bot; it streams logs into the Bot for assertions.
func StartBot(t *testing.T, ctx context.Context, cfg BotConfig) *Bot {
	evmBotBin := evmArtifact(t, "EVM_BOT_BIN")
	if cfg.Checkpoint == "" {
		cfg.Checkpoint = filepath.Join(t.TempDir(), "evm-bot-checkpoint.json")
	}
	cmd := exec.CommandContext(ctx, evmBotBin)
	cmd.Env = append(os.Environ(),
		"ETH_RPC="+cfg.EthRPC,
		"CONTRACT_ID="+cfg.ContractID,
		"GRAPHQL_URLS="+cfg.GraphQLURL,
		"BOT_ETH_PRIVKEY="+cfg.EthPrivHex,
		"NET_ID=vsc-devnet",
		"NETWORK=testnet",
		"RC_LIMIT=5000000",
		"CHECKPOINT_FILE="+cfg.Checkpoint,
	)
	if cfg.VaultAddr != "" { // optional: the bot takes the vault from the TSS key and refuses a different one
		cmd.Env = append(cmd.Env, "VAULT_ADDRESS="+cfg.VaultAddr)
	}
	if cfg.Tokens != "" {
		cmd.Env = append(cmd.Env, "TOKENS="+cfg.Tokens)
	}
	if cfg.HealthAddr != "" {
		cmd.Env = append(cmd.Env, "HEALTH_ADDR="+cfg.HealthAddr, "HEALTH_CHECK_EVERY=10s")
	}
	// RP-1: a new checkpoint starts at START_BLOCK or the verifier tip, never
	// block 1; a wave's deposits can predate the tip it plants.
	if cfg.StartBlock == "" {
		cfg.StartBlock = "1"
	}
	cmd.Env = append(cmd.Env, "START_BLOCK="+cfg.StartBlock)
	if cfg.NeedsFile != "" {
		cmd.Env = append(cmd.Env, "NEEDS_FILE="+cfg.NeedsFile)
	}
	cmd.SysProcAttr = dieWithTest()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("bot start: %v", err)
	}
	b := &Bot{cmd: cmd}
	scan := func(r *bufio.Scanner) {
		for r.Scan() {
			line := r.Text()
			b.mu.Lock()
			b.logs = append(b.logs, line)
			b.mu.Unlock()
		}
	}
	go scan(bufio.NewScanner(stdout))
	go scan(bufio.NewScanner(stderr))
	return b
}

func (b *Bot) Stop() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}

// Logs returns a snapshot of captured log lines.
func (b *Bot) Logs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.logs))
	copy(out, b.logs)
	return out
}

// WaitForLog polls until a log line contains `substr` or timeout.
func (b *Bot) WaitForLog(ctx context.Context, substr string, timeout time.Duration) bool {
	deadline := devnetNow().Add(timeout)
	for {
		b.mu.Lock()
		for _, l := range b.logs {
			if strings.Contains(l, substr) {
				b.mu.Unlock()
				return true
			}
		}
		b.mu.Unlock()
		if devnetNow().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-devnetAfter(time.Second):
		}
	}
}

// BotHealth is the bot's GET /health report (cmd/evm-mapping-bot healthReport).
type BotHealth struct {
	Status    string   `json:"status"`
	CheckedAt string   `json:"checkedAt"`
	Issues    []string `json:"issues"`
}

// Health reads the bot's /health at addr (its BotConfig.HealthAddr).
func (b *Bot) Health(ctx context.Context, addr string) (*BotHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h BotHealth
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}
