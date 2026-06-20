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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

const evmBotBin = "/tmp/evm-mapping-bot"

// BotConfig is the env for one bot process.
type BotConfig struct {
	EthRPC      string // anvil URL
	VaultAddr   string // 0x..20-byte
	ContractID  string
	GraphQLURL  string // a devnet node GQL endpoint
	EthPrivHex  string // 64-hex L2 relayer key (its DID must be RC-funded)
	Checkpoint  string // checkpoint file path
}

// Bot is a running bot process with captured logs.
type Bot struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	logs []string
}

// StartBot launches the bot; it streams logs into the Bot for assertions.
func StartBot(t *testing.T, ctx context.Context, cfg BotConfig) *Bot {
	if _, err := os.Stat(evmBotBin); err != nil {
		t.Fatalf("bot binary missing at %s (build: go build -o %s ./cmd/evm-mapping-bot): %v", evmBotBin, evmBotBin, err)
	}
	if cfg.Checkpoint == "" {
		cfg.Checkpoint = fmt.Sprintf("/tmp/evm-bot-cp-%d.json", time.Now().UnixNano())
	}
	cmd := exec.CommandContext(ctx, evmBotBin)
	cmd.Env = append(os.Environ(),
		"ETH_RPC="+cfg.EthRPC,
		"VAULT_ADDRESS="+cfg.VaultAddr,
		"CONTRACT_ID="+cfg.ContractID,
		"GRAPHQL_URLS="+cfg.GraphQLURL,
		"BOT_ETH_PRIVKEY="+cfg.EthPrivHex,
		"NET_ID=vsc-devnet",
		"NETWORK=testnet",
		"RC_LIMIT=5000000",
		"CHECKPOINT_FILE="+cfg.Checkpoint,
	)
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
	deadline := time.Now().Add(timeout)
	for {
		b.mu.Lock()
		for _, l := range b.logs {
			if strings.Contains(l, substr) {
				b.mu.Unlock()
				return true
			}
		}
		b.mu.Unlock()
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}
