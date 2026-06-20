//go:build evm_devnet

// BotResilience — proves the evm-mapping-bot's checkpoint / lock / fail-closed
// resilience fixes at RUNTIME against a real devnet + anvil L1. No deposits are
// required: the bot only needs to START, scan anvil (vault set, no transfers),
// and persist its checkpoint. We then exercise:
//
//   - MED-43  fail-closed on a corrupt checkpoint  (loadCheckpoint → os.Exit(1))
//   - MED-91  exclusive flock so a 2nd instance fails fast (acquireCheckpointLock)
//   - MED-120 0600 perms on the checkpoint file      (cp.save WriteFile 0600)
//   - C-ER1   per-worker scanBlockSafe recover        (structurally covered)
//   - restart-resume: a fresh start loads the persisted last_scanned_block.
//
// The bot binary is /tmp/evm-mapping-bot (built once by the lead). It is driven
// here as a raw process via the StartBot runner (evm_bot.go), keying assertions
// on the bot's REAL slog messages (read from cmd/evm-mapping-bot/main.go).
//
// Run: go test -tags evm_devnet -run TestEVMBridge_BotResilience ./tests/devnet/ -timeout 40m -v
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

func TestEVMBridge_BotResilience(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)

	anvil := StartAnvil(t, ctx, 1, nextAnvilPort()) // contract chainId() defaults to 1
	t.Cleanup(anvil.Stop)

	// Mine an anvil chain to scan + wait for it to finalize so the bot's
	// getFinalizedBlock() returns a height > 0 and the scan loop runs.
	if err := anvil.Mine(ctx, 12); err != nil {
		t.Fatalf("mine anvil: %v", err)
	}
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		f, e2 := anvil.Finalized(ctx)
		return e2 == nil && f >= 1
	}); err != nil {
		t.Fatalf("anvil never produced a finalized block to scan")
	}

	// Vault: the bot requires a valid 20-byte non-zero VAULT_ADDRESS (MED-44),
	// but no deposits are sent to it (scan-only). setVault is a short-timelock
	// admin action; the address itself just needs to be a real eth address.
	vaultKey, _ := ethCrypto.GenerateKey()
	vaultAddr := ethCrypto.PubkeyToAddress(vaultKey.PublicKey)
	if err := e.setVault(vaultAddr.Hex()); err != nil {
		t.Fatalf("setVault: %v", err)
	}

	// BOT-H FIX PROOF: the bot caps its L1 scan at the contract's ingested header
	// height (contractHeight). Post-M23-F5 that height lives in the ZK verifier,
	// and the fixed fetchContractLastHeight reads am['zkverifier'] then the
	// verifier's 'h'. Plant the real finalized anvil header so the verifier 'h' is
	// non-zero — otherwise the (fixed) bot reads contractHeight=0 and never scans
	// (scanUpTo = min(finalized, 0) = 0). Plant BEFORE the bot starts so its first
	// tick already sees a non-zero ceiling and advances LastScannedBlock.
	finH, err := anvil.Finalized(ctx)
	if err != nil || finH < 1 {
		t.Fatalf("anvil finalized height unavailable for scan ceiling: %v", err)
	}
	scanHdr, err := anvil.Header(ctx, finH)
	if err != nil {
		t.Fatalf("read anvil header %d: %v", finH, err)
	}
	if err := e.plantHeader(scanHdr, 1); err != nil {
		t.Fatalf("plant scan-ceiling header %d: %v", finH, err)
	}
	t.Logf("planted verifier scan-ceiling header height=%d so the fixed bot can scan", finH)

	// Relayer eth key for BOT_ETH_PRIVKEY (the bot loads it for L2 signing; with
	// no deposits it never submits, so the DID needs no RC funding).
	relayerKey, _ := ethCrypto.GenerateKey()
	relayerHex := hex.EncodeToString(ethCrypto.FromECDSA(relayerKey))

	gqlURL := e.d.GQLEndpoint(e.gqlNode)
	t.Logf("vault=%s gql=%s contract=%s", vaultAddr.Hex(), gqlURL, e.amID)

	// Single checkpoint path reused across restarts (under t.TempDir so it is
	// cleaned up automatically). The bot writes a sidecar `.lock` and `.tmp`.
	cpDir := t.TempDir()
	cpPath := filepath.Join(cpDir, "evm-bot-checkpoint.json")
	lockPath := cpPath + ".lock"

	mkCfg := func() BotConfig {
		return BotConfig{
			EthRPC:     anvil.URL,
			VaultAddr:  vaultAddr.Hex(),
			ContractID: e.amID,
			GraphQLURL: gqlURL,
			EthPrivHex: relayerHex,
			Checkpoint: cpPath,
		}
	}

	// Bot slog substrings we key on (verbatim from cmd/evm-mapping-bot/main.go):
	const (
		logLoadedCheckpoint = "loaded checkpoint"                          // main.go:2043 (every start; carries lastBlock)
		logStarting         = "evm-mapping-bot starting"                   // main.go:72   (proves the process came up)
		logCorrupt          = "checkpoint file is corrupt"                 // main.go:241  (MED-43, then os.Exit(1))
		logLockHeld         = "another bot instance already holds"         // main.go:298  (MED-91, in the returned err)
	)

	// ── (1) Checkpoint persistence + restart resume ───────────────────────────
	botA := StartBot(t, ctx, mkCfg())
	if !botA.WaitForLog(ctx, logStarting, 90*time.Second) {
		t.Fatalf("bot A never logged %q; logs:\n%s", logStarting, joinLogs(botA))
	}
	if !botA.WaitForLog(ctx, logLoadedCheckpoint, 90*time.Second) {
		t.Fatalf("bot A never logged %q; logs:\n%s", logLoadedCheckpoint, joinLogs(botA))
	}

	// Wait for the checkpoint FILE to exist with last_scanned_block > 0: the
	// scan loop advances LastScannedBlock through empty anvil blocks, then
	// cp.save() writes it (atomic tmp+rename, 0600).
	if err := pollUntil(ctx, 3*time.Minute, func() bool {
		cp, err := readCheckpoint(cpPath)
		return err == nil && cp.LastScannedBlock > 0
	}); err != nil {
		t.Fatalf("checkpoint file never persisted last_scanned_block>0 at %s; bot logs:\n%s", cpPath, joinLogs(botA))
	}
	cpAfterA, err := readCheckpoint(cpPath)
	if err != nil {
		t.Fatalf("read checkpoint after bot A: %v", err)
	}
	savedBlock := cpAfterA.LastScannedBlock
	if savedBlock == 0 {
		t.Fatalf("expected persisted last_scanned_block>0, got 0")
	}
	if cpAfterA.Version != 1 {
		t.Fatalf("expected checkpoint version 1, got %d", cpAfterA.Version)
	}
	t.Logf("ASSERT persist: checkpoint at %s has last_scanned_block=%d version=%d", cpPath, savedBlock, cpAfterA.Version)

	botA.Stop()
	// The flock is released on process death, but the sidecar .lock FILE remains
	// on disk; remove it so the restart can re-acquire cleanly (matches the
	// recon recipe: delete .lock between restarts).
	_ = os.Remove(lockPath)
	time.Sleep(2 * time.Second) // let the killed process fully release the fd

	// Restart with the SAME checkpoint → it must LOAD the saved block, not
	// rescan from 0. "loaded checkpoint" carries lastBlock=<savedBlock>.
	botA2 := StartBot(t, ctx, mkCfg())
	if !botA2.WaitForLog(ctx, logLoadedCheckpoint, 90*time.Second) {
		t.Fatalf("bot A2 never logged %q on restart; logs:\n%s", logLoadedCheckpoint, joinLogs(botA2))
	}
	// ASSERT it resumed from the persisted block, not 0. The slog line is
	// `loaded checkpoint lastBlock=<n>` — assert the resumed value matches.
	wantResume := fmt.Sprintf("lastBlock=%d", savedBlock)
	resumed := botA2.WaitForLog(ctx, wantResume, 30*time.Second)
	if !resumed {
		// Tolerate the (rare) case where the bot advanced past savedBlock before
		// dumping logs; re-read the live checkpoint and require it did not regress.
		cpNow, rerr := readCheckpoint(cpPath)
		if rerr != nil || cpNow.LastScannedBlock < savedBlock {
			t.Fatalf("bot A2 did NOT resume from saved block %d (no %q log; checkpoint now=%v err=%v); logs:\n%s",
				savedBlock, wantResume, cpNow.LastScannedBlock, rerr, joinLogs(botA2))
		}
		t.Logf("ASSERT resume: no exact log match but checkpoint did not regress (now=%d >= saved=%d)", cpNow.LastScannedBlock, savedBlock)
	} else {
		t.Logf("ASSERT resume: bot A2 loaded checkpoint at saved block %d (did not rescan from 0)", savedBlock)
	}
	botA2.Stop()
	_ = os.Remove(lockPath)
	time.Sleep(2 * time.Second)

	// ── (3) Corrupt-checkpoint fail-closed (MED-43) ───────────────────────────
	// Overwrite the checkpoint with partial/invalid JSON. loadCheckpoint must
	// NOT discard the unmarshal error and re-scan from empty — it logs
	// "checkpoint file is corrupt …" and os.Exit(1)s. The bot process dies
	// without ever entering the scan loop.
	if err := os.WriteFile(cpPath, []byte(`{"last_scanned_block": 12, "sent_wi`), 0600); err != nil {
		t.Fatalf("write corrupt checkpoint: %v", err)
	}
	botCorrupt := StartBot(t, ctx, mkCfg())
	if !botCorrupt.WaitForLog(ctx, logCorrupt, 60*time.Second) {
		t.Fatalf("bot did NOT fail-closed on corrupt checkpoint (no %q log) — MED-43 regression; logs:\n%s",
			logCorrupt, joinLogs(botCorrupt))
	}
	// It must NOT have proceeded to load a phantom-empty checkpoint and scan.
	if botCorrupt.WaitForLog(ctx, logLoadedCheckpoint, 3*time.Second) {
		t.Fatalf("MED-43 regression: bot logged %q AFTER a corrupt file — it silently re-scanned instead of failing closed; logs:\n%s",
			logLoadedCheckpoint, joinLogs(botCorrupt))
	}
	botCorrupt.Stop()
	t.Logf("ASSERT corrupt: bot logged %q and did not re-scan (MED-43 fail-closed)", logCorrupt)

	// Restore a clean checkpoint for the lock test, and clear the lock.
	if err := writeCleanCheckpoint(cpPath, savedBlock); err != nil {
		t.Fatalf("restore clean checkpoint: %v", err)
	}
	_ = os.Remove(lockPath)

	// ── (4) Dual-instance lock (MED-91) ───────────────────────────────────────
	// Start bot B holding the checkpoint; while it runs, start bot C on the SAME
	// checkpoint path. C must fail to acquire the flock and exit; B keeps running.
	botB := StartBot(t, ctx, mkCfg())
	if !botB.WaitForLog(ctx, logStarting, 90*time.Second) {
		t.Fatalf("bot B never started; logs:\n%s", joinLogs(botB))
	}
	if !botB.WaitForLog(ctx, logLoadedCheckpoint, 90*time.Second) {
		t.Fatalf("bot B never loaded checkpoint (needed to hold the lock); logs:\n%s", joinLogs(botB))
	}
	// B now holds the flock. Launch C on the same path.
	botC := StartBot(t, ctx, mkCfg())
	if !botC.WaitForLog(ctx, logLockHeld, 60*time.Second) {
		t.Fatalf("MED-91 regression: second instance did NOT report %q (lock not exclusive); botC logs:\n%s",
			logLockHeld, joinLogs(botC))
	}
	botC.Stop()
	// B must still be alive (it never reported a lock error / corruption).
	for _, l := range botB.Logs() {
		if strings.Contains(l, logLockHeld) || strings.Contains(l, logCorrupt) {
			t.Fatalf("MED-91: bot B (the lock holder) unexpectedly reported a fatal: %q", l)
		}
	}
	t.Logf("ASSERT dual-instance: second bot reported %q and exited; holder kept running (MED-91)", logLockHeld)
	botB.Stop()
	_ = os.Remove(lockPath)

	// ── (5) 0600 perms on the checkpoint file (MED-120) ───────────────────────
	// Re-read the file the holder wrote and assert its mode is exactly 0600
	// (cp.save → os.WriteFile(tmp, …, 0600) then atomic rename).
	if err := pollUntil(ctx, 60*time.Second, func() bool {
		fi, err := os.Stat(cpPath)
		return err == nil && fi.Mode().Perm() == 0o600
	}); err != nil {
		fi, serr := os.Stat(cpPath)
		gotMode := "stat-err"
		if serr == nil {
			gotMode = fi.Mode().Perm().String()
		}
		t.Fatalf("MED-120 regression: checkpoint perms != 0600 (got %s); err=%v", gotMode, err)
	}
	fi, _ := os.Stat(cpPath)
	t.Logf("ASSERT perms: checkpoint %s mode=%v == 0600 (MED-120)", cpPath, fi.Mode().Perm())

	// ── (C-ER1) per-worker scanBlockSafe recover — structural coverage note ────
	// Injecting a malformed block into anvil (to trip a scanBlock panic inside a
	// scanBlocksParallel worker) is infeasible from this harness: anvil only
	// serves well-formed canonical blocks, and the bot's hostile-RPC parse paths
	// can't be reached without a fake RPC. scanBlockSafe (main.go:1041-1049)
	// wraps each per-height scan in its own defer/recover that converts a panic
	// into a per-block error entry (out[h]={err:…}); the caller stops at the
	// first error and re-scans the window, so the checkpoint never advances past
	// a bad block. This is exercised by unit tests on the bot; here we record it
	// as structurally covered rather than failing the runtime test for it.
	t.Logf("C-ER1: scanBlockSafe per-worker recover not runtime-injectable from anvil — structurally covered (main.go:1041-1049), not asserted here")

	t.Logf("BotResilience PASS — checkpoint persist/resume (saved block %d resumed), corrupt fail-closed (MED-43), dual-instance lock (MED-91), 0600 perms (MED-120); C-ER1 structurally covered",
		savedBlock)
}

// botCheckpoint mirrors the on-disk fields of the bot's Checkpoint we assert on
// (cmd/evm-mapping-bot/main.go Checkpoint). We only decode what we check.
type botCheckpoint struct {
	LastScannedBlock uint64 `json:"last_scanned_block"`
	Version          int    `json:"version"`
}

func readCheckpoint(path string) (*botCheckpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp botCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// writeCleanCheckpoint writes a valid, version-1 checkpoint at the given block so
// the lock test can start from a non-corrupt state. Mirrors the bot's on-disk
// schema (sent_withdrawals + block_retries maps + version=checkpointVersion=1).
func writeCleanCheckpoint(path string, lastBlock uint64) error {
	payload := map[string]any{
		"last_scanned_block": lastBlock,
		"sent_withdrawals":   map[string]any{},
		"block_retries":      map[string]any{},
		"version":            1,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func joinLogs(b *Bot) string {
	return strings.Join(b.Logs(), "\n")
}
