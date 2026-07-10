package devnet

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// TestL7RedriveSweepDevnet is the end-to-end devnet proof of the L7-01 stuck-tx
// re-drive — the one thing unit tests structurally cannot prove: that a low-fee
// migration sweep, held unconfirmed in a REAL bitcoind-regtest mempool, is
// REPLACED under BIP-125 (nSequence 0xfffffffd) by a higher-fee redriveSpend
// tx that then confirms, settles the spend group, and unwedges NN#3 so the next
// createKey succeeds. It mirrors TestOracleDashChainRelay's contract+oracle
// scaffolding but for the btc-mapping-contract, then drives the vault lifecycle.
//
// Run:  go test -v -run TestL7RedriveSweepDevnet -timeout 40m ./tests/devnet/
//
// STATUS: scaffold on a proven harness (the devnet + TSS keygen/reshare are
// verified working here). The three marked steps (DEPOSIT via SPV, the
// migrate→sweep broadcast timing, and the RBF mempool assertions) are the
// devnet-iteration points — each is a real harness call, tuned against a live
// run. Env BTC_MAPPING_WASM_PATH overrides the wasm; DEVNET_KEEP=1 keeps the net.
func TestL7RedriveSweepDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet L7-01 re-drive test in short mode")
	}
	// SCAFFOLD GATE: the vault-flow helpers below (BTC deposit via SPV proof, the
	// low-fee sweep broadcast timing, and the RBF mempool eviction assertions) are the
	// devnet-iteration points — real harness calls tuned against a live run. Until they
	// are implemented, skip so the package still builds + the documented flow stands as
	// the exact build recipe. Set L7_DEVNET_RUN=1 to attempt the full run.
	if os.Getenv("L7_DEVNET_RUN") == "" {
		t.Skip("L7-01 devnet re-drive: scaffold — set L7_DEVNET_RUN=1 once the deposit/RBF helpers are implemented (see comments)")
	}
	requireDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 38*time.Minute)
	defer cancel()

	// ── btc-mapping-contract WASM (built via scratchpad/build-wasm.sh) ───
	wasmPath := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasmPath == "" {
		wasmPath = "/home/clauderfly/utxo-s1/btc-mapping-contract/bin/dev.wasm"
	}
	if _, err := os.Stat(wasmPath); err != nil {
		t.Fatalf("btc-mapping-contract wasm not found (%s): rebuild it first (scratchpad/build-wasm.sh): %v", wasmPath, err)
	}

	// ── Devnet: 6 nodes, bitcoind regtest, v2 rotation PINNED ────────────
	cfg := DefaultConfig()
	cfg.Nodes = 6
	cfg.GenesisNode = 6
	cfg.LogLevel = "info,oracle=debug,tss=debug,chain-relay=debug"
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval: 60,
			// PIN vault-rotation-v2 low so the run exercises the v2 path (not
			// the inert pre-v2 path). Without this the rotation/re-drive guards
			// are all no-ops (VaultRotationV2Enabled==false).
			VaultRotationV2ActivationHeight: 1,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("creating devnet: %v", err)
	}
	t.Cleanup(func() { d.Stop() })
	if err := d.Start(ctx); err != nil {
		dumpLogs(t, d, ctx)
		t.Fatalf("starting devnet: %v", err)
	}

	// ── Seed the BTC chain + deploy + seedBlocks + wire the oracle ───────
	const seedHeight = 1
	if _, err := d.MineBlocks(ctx, 10); err != nil {
		t.Fatalf("mining initial btc blocks: %v", err)
	}
	seedHeaderHex, err := btcHeaderHex(ctx, d, seedHeight) // bitcoind twin of GetDashBlockHeaderHex
	if err != nil {
		t.Fatalf("reading btc seed header: %v", err)
	}
	contractId, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasmPath, Name: "btc-mapping-contract",
		Description: "BTC UTXO mapping contract (devnet L7-01)", DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploying btc-mapping-contract: %v", err)
	}
	t.Logf("btc-mapping-contract deployed: %s", contractId)

	seedPayload := fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, seedHeaderHex, seedHeight)
	if _, err := d.CallContract(ctx, 1, contractId, "seedBlocks", seedPayload); err != nil {
		t.Fatalf("seedBlocks: %v", err)
	}
	if err := d.WriteOracleConfigs(ctx); err != nil {
		t.Fatalf("writing oracle configs: %v", err)
	}
	if err := d.SetOracleContractIDs(map[string]string{"BTC": contractId}); err != nil {
		t.Fatalf("setting ChainContracts[BTC]: %v", err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatalf("restarting magi nodes: %v", err)
	}

	// ── Vault gen-0: keygen → registerPublicKey → activate ───────────────
	// createKey makes the nodes keygen a TSS key for the contract; the
	// resulting compressed pubkey (from the keygen commitment / MongoDB, as
	// reshare_happy_test reads it) is registered so the contract can derive
	// its P2WSH vault address.
	if _, err := d.CallContract(ctx, 1, contractId, "createKey", ""); err != nil {
		t.Fatalf("createKey (gen-0): %v", err)
	}
	primaryPub := waitForKeygenPubkey(t, d, ctx, contractId, 0, 3*time.Minute) // twin of reshare_happy's keygen wait
	regPayload := fmt.Sprintf(`{"primary_public_key":"%s"}`, primaryPub)
	if _, err := d.CallContract(ctx, 1, contractId, "registerPublicKey", regPayload); err != nil {
		t.Fatalf("registerPublicKey (gen-0): %v", err)
	}
	if _, err := d.CallContract(ctx, 1, contractId, "activateKey", ""); err != nil {
		t.Fatalf("activateKey (gen-0): %v", err)
	}

	// ── DEPOSIT (iteration point 1): fund gen-0 with a real BTC deposit ──
	// Send a BTC tx to the gen-0 vault P2WSH, mine it, let the oracle relay
	// the header (addBlocks), then `map` with the tx + Merkle proof so the
	// contract credits a confirmed gen-0 UTXO. Helper builds the tx+proof.
	fundVaultDeposit(t, d, ctx, contractId, 0, 200000) // sats — funds gen-0

	// ── Rotate: gen-0 → retiring, gen-1 → active ─────────────────────────
	if _, err := d.CallContract(ctx, 1, contractId, "createKey", ""); err != nil {
		t.Fatalf("createKey (gen-1): %v", err)
	}
	gen1Pub := waitForKeygenPubkey(t, d, ctx, contractId, 1, 3*time.Minute)
	if _, err := d.CallContract(ctx, 1, contractId, "registerPublicKey", fmt.Sprintf(`{"primary_public_key":"%s"}`, gen1Pub)); err != nil {
		t.Fatalf("registerPublicKey (gen-1): %v", err)
	}
	if _, err := d.CallContract(ctx, 1, contractId, "activateKey", ""); err != nil {
		t.Fatalf("activateKey (gen-1): %v", err)
	}

	// ── migrateVault → build the sweep; the nodes TSS-sign it ────────────
	// Set the oracle fee-rate LOW so the sweep pays a low fee, then keep it
	// out of a block (iteration point 2: don't mine the sweep tx).
	setLowFeeRate(t, d, ctx)
	if _, err := d.CallContract(ctx, 1, contractId, "migrateVault", ""); err != nil {
		t.Fatalf("migrateVault: %v", err)
	}
	sweepTxid := waitForPendingSpendBroadcast(t, d, ctx, contractId, 5*time.Minute) // node broadcasts the signed sweep to bitcoind
	t.Logf("stuck sweep broadcast to regtest mempool: %s", sweepTxid)

	// ── Prove it's stuck, then re-drive (iteration point 3: RBF) ─────────
	assertInMempoolUnconfirmed(t, d, ctx, sweepTxid)
	// Advance the contract height past RedriveStaleBlocks (12) WITHOUT
	// confirming the sweep: mine empty-of-the-sweep blocks + relay headers.
	mineHeadersWithout(t, d, ctx, contractId, sweepTxid, 15)

	if _, err := d.CallContract(ctx, 1, contractId, "redriveSpend", sweepTxid); err != nil {
		t.Fatalf("redriveSpend: %v", err)
	}
	replTxid := waitForPendingSpendBroadcast(t, d, ctx, contractId, 5*time.Minute)
	t.Logf("re-drive replacement broadcast: %s", replTxid)

	// BIP-125: the replacement must EVICT the original in the real mempool.
	assertReplacedInMempool(t, d, ctx, sweepTxid, replTxid)

	// ── Confirm the replacement → settle the group → NN#3 unwedges ───────
	confirmTx(t, d, ctx, contractId, replTxid)
	assertSpendGroupCleared(t, d, ctx, contractId, sweepTxid, replTxid)

	// The whole point: the re-driven sweep settled + drained gen-0, so
	// createKey (NN#3) succeeds again — the freeze is lifted.
	if _, err := d.CallContract(ctx, 1, contractId, "createKey", ""); err != nil {
		t.Fatalf("createKey did NOT unwedge after the re-driven sweep settled (NN#3 still blocking): %v", err)
	}
	t.Logf("PASS: L7-01 re-drive evicted the stuck sweep, confirmed, settled, and unwedged NN#3")
}

// ─────────────────────────────────────────────────────────────────────────────
// L7-01 devnet helpers — the iteration points. Each is a real harness call to be
// implemented + tuned against a live run (the flow above is the exact recipe).
// They panic-with-guidance so a forced L7_DEVNET_RUN=1 fails loudly at the exact
// unimplemented step rather than silently. The primitives they wrap all exist on
// *Devnet: MineBlocks, BitcoindRPCURL/HostPort, CallContract, GQL queries, and
// waitForLogAnyNode (see oracle_dash_chain_relay_test.go for the pattern).
// ─────────────────────────────────────────────────────────────────────────────

func l7todo(t *testing.T, what string) { t.Fatalf("L7-01 devnet helper not implemented: %s", what) }

// btcHeaderHex reads block `height`'s 80-byte header hex from the devnet bitcoind
// (getblockhash + getblockheader false) — the twin of GetDashBlockHeaderHex.
func btcHeaderHex(ctx context.Context, d *Devnet, height uint64) (string, error) {
	_ = ctx
	_ = d
	_ = height
	return "", fmt.Errorf("btcHeaderHex not implemented (bitcoind getblockheader via BitcoindRPCURL)")
}

// waitForKeygenPubkey polls MongoDB / GQL for the keygen commitment of the vault
// key for `gen` and returns its compressed primary pubkey hex (as
// reshare_happy_test reads the keygen commitment). Blocks until keygen lands.
func waitForKeygenPubkey(t *testing.T, d *Devnet, ctx context.Context, contractId string, gen int, timeout time.Duration) string {
	_, _, _, _ = d, ctx, contractId, timeout
	l7todo(t, fmt.Sprintf("waitForKeygenPubkey(gen=%d)", gen))
	return ""
}

// fundVaultDeposit sends `sats` to gen's P2WSH vault address on bitcoind, mines
// it, waits for the oracle to relay the header (addBlocks), then calls `map`
// with the tx + Merkle proof so the contract credits a confirmed UTXO.
func fundVaultDeposit(t *testing.T, d *Devnet, ctx context.Context, contractId string, gen int, sats int64) {
	_, _, _, _ = d, ctx, contractId, sats
	l7todo(t, fmt.Sprintf("fundVaultDeposit(gen=%d, sats=%d) — BTC tx + SPV Merkle proof to the vault P2WSH", gen, sats))
}

// setLowFeeRate drives the oracle base fee-rate low (via addBlocks latest_fee)
// so the next migrateVault sweep pays a low, evictable fee.
func setLowFeeRate(t *testing.T, d *Devnet, ctx context.Context) { _, _ = d, ctx; l7todo(t, "setLowFeeRate") }

// waitForPendingSpendBroadcast waits for a node to broadcast a newly-signed
// pending spend ("d-"/TxSpends) to the bitcoind mempool and returns its txid.
func waitForPendingSpendBroadcast(t *testing.T, d *Devnet, ctx context.Context, contractId string, timeout time.Duration) string {
	_, _, _ = d, ctx, contractId
	_ = timeout
	l7todo(t, "waitForPendingSpendBroadcast (node broadcasts the TSS-signed spend to bitcoind)")
	return ""
}

// assertInMempoolUnconfirmed asserts txid is in the bitcoind mempool and NOT yet
// in a block (getmempoolentry succeeds; getrawtransaction confirmations==0).
func assertInMempoolUnconfirmed(t *testing.T, d *Devnet, ctx context.Context, txid string) {
	_, _, _ = d, ctx, txid
	l7todo(t, "assertInMempoolUnconfirmed")
}

// mineHeadersWithout mines `n` blocks that EXCLUDE txid (keep it stuck) and
// relays the headers so the contract's "h" clock advances past RedriveStaleBlocks.
func mineHeadersWithout(t *testing.T, d *Devnet, ctx context.Context, contractId, txid string, n int) {
	_, _, _, _ = d, ctx, contractId, txid
	_ = n
	l7todo(t, "mineHeadersWithout (advance h past RedriveStaleBlocks without confirming the sweep)")
}

// assertReplacedInMempool asserts the BIP-125 eviction: oldTxid is GONE from the
// mempool and newTxid is present (getrawmempool).
func assertReplacedInMempool(t *testing.T, d *Devnet, ctx context.Context, oldTxid, newTxid string) {
	_, _, _, _ = d, ctx, oldTxid, newTxid
	l7todo(t, "assertReplacedInMempool (BIP-125 eviction proof)")
}

// confirmTx mines txid into a block, relays the header, and calls confirmSpend
// with the tx + Merkle proof so the contract settles it.
func confirmTx(t *testing.T, d *Devnet, ctx context.Context, contractId, txid string) {
	_, _, _, _ = d, ctx, contractId, txid
	l7todo(t, "confirmTx (mine + relay + confirmSpend)")
}

// assertSpendGroupCleared asserts settle-either cleared BOTH members' ms-/d-
// records + the g- group object (GQL contract-state reads).
func assertSpendGroupCleared(t *testing.T, d *Devnet, ctx context.Context, contractId, sweepTxid, replTxid string) {
	_, _, _, _, _ = d, ctx, contractId, sweepTxid, replTxid
	l7todo(t, "assertSpendGroupCleared (ms-/d-/g- all gone for both members)")
}
