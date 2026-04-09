# Devnet Integration Test Progress

**Date:** 2026-04-07 / 2026-04-08
**Branch:** `feature/onchain-tss-gossip` at `/home/dockeruser/magi/tss-fixes/`
**Authors:** lordbutterfly + Claude Code + Milo

---

## Summary

17 TSS devnet integration tests — **16 PASS, 1 graceful SKIP, 0 FAIL**.

The tests cover the full TSS lifecycle: keygen, reshare, blame, recovery,
multi-version nodes, network partitions, leader crashes, flapping nodes,
simultaneous restarts, and preparams exhaustion.

---

## Test Results (2026-04-08 ~14:00 UTC)

### Original tests (from `tss_deterministic_test.go`, now split into separate files)

| # | Test | File | Status | Notes |
|---|------|------|--------|-------|
| 1 | `TestTSSReshareHappyPath` | `reshare_happy_test.go` | **PASS** | Keygen → readiness → reshare, baseline smoke test |
| 2 | `TestTSSOfflineNodeExcludedByReadiness` | `readiness_test.go` | **PASS** | Offline node excluded by readiness gate, no blame needed |
| 3 | `TestTSSFalseReadinessProducesBlame` | `readiness_test.go` | **PASS** | Node broadcasts readiness then disconnects → blame |
| 4 | `TestTSSBlameExcludesNodeNextCycle` | `blame_cycle_test.go` | **PASS** | Blamed node excluded in next reshare cycle |
| 5 | `TestTSSPartitionAndRecovery` | `partition_recovery_test.go` | **PASS** | 2-node partition, reshare continues, recovery after heal |
| 6 | `TestTSSBlameEpochDecode` | `blame_cycle_test.go` | **SKIP** | Readiness gate too effective for false readiness timing (see note below) |
| 7 | `TestTSSBlameAccumulation` | `blame_cycle_test.go` | **PASS** | Multiple blames accumulated, both offenders excluded |
| 8 | `TestTSSMultiVersion` | `multiversion_test.go` | **PASS** | Old-code node excluded, doesn't crash, SSID mismatch expected |
| 9 | `TestTSSFullRecoveryCycle` | `recovery_full_test.go` | **PASS** | Full lifecycle: keygen → reshare → disconnect → blame → reconnect → reshare |

### New tests (2026-04-08)

| # | Test | File | Status | Notes |
|---|------|------|--------|-------|
| 10 | `TestBlameDisconnectedNode` | `blame_disconnect_test.go` | **PASS** | Fully disconnected node, readiness gate excluded it, reshare succeeded |
| 11 | `TestEdgeLeaderCrashDuringBLS` | `edge_cases_test.go` | **PASS** | Leader stopped mid-BLS, blame + recovery by new leader |
| 12 | `TestEdgePartitionNoQuorum` | `edge_cases_test.go` | **PASS** | 3/4 split, neither has quorum, clean recovery after heal |
| 13 | `TestEdgeNodeRestartMidCycle` | `edge_cases_test.go` | **PASS** | Node restarted between readiness and reshare, 3 reshares landed |
| 14 | `TestEdgePreparamsExhaustion` | `edge_cases_test.go` | **PASS** | 4+ cycles with latency, preparams held up, recovery after removal |
| 15 | `TestEdgeKeygenBlameCrossEpoch` | `edge_cases_test.go` | **PASS** | Keygen blame at epoch 0 correctly decoded, reshare in epoch 1 works |
| 16 | `TestEdgeFlappingNode` | `edge_cases_test.go` | **PASS** | Node online/offline every cycle, 3 reshares + 1 blame, network converges |
| 17 | `TestEdgeSimultaneousRestart` | `edge_cases_test.go` | **PASS** | All 7 nodes restart at once, 3 reshares after recovery |

### Note on TestTSSBlameEpochDecode (SKIP)

This test tries to create "false readiness" blame during reshare to test
epoch decode logic. It SKIPs because the on-chain readiness gate is so
effective that the disconnected node is excluded before it can cause blame.
The underlying bug (blame decoded against wrong election epoch) IS tested
by `TestEdgeKeygenBlameCrossEpoch` which uses keygen blame instead — that
test PASSES.

---

## What We Built

### Day 1 (2026-04-07): Infrastructure + 9 original tests

- Split monolithic `tss_deterministic_test.go` into 7 focused files
- Fixed ~18 devnet infrastructure issues (see below)
- Fixed readiness broadcast spam (in-memory dedup in `tss.go`)
- Added configurable `PreParamsTimeout` to TssParams

### Day 2 (2026-04-08): 8 edge case tests + fixes

- `TestBlameDisconnectedNode` — fully disconnected node (not just slow)
- 7 edge case tests in `edge_cases_test.go` covering leader crash, partition deadlock,
  node restart, preparams exhaustion, keygen blame cross-epoch, flapping node,
  simultaneous restart
- Fixed Dockerfile.devnet to run `gqlgen generate` (needed after Milo's GQL changes)
- Fixed P2PBasePort in all test configs (conflicts with mainnet/testnet on 10720+)
- Relaxed SSID mismatch assertions in tests where disconnected nodes cause expected mismatches

### Code changes to `modules/tss/tss.go`

1. **Readiness broadcast dedup**: Added `readinessSent map[string]bool` for in-memory
   deduplication. The old MongoDB check was racy (broadcast takes 1-2 blocks to land
   on-chain), causing 3-5 duplicate `vsc.tss_ready` txs per key per cycle. Now: exactly
   one broadcast per key per cycle.

2. **Configurable PreParamsTimeout**: `GeneratePreParams()` timeout was hardcoded to
   1 minute. On loaded servers with multiple nodes, Paillier prime generation can take
   longer. Now reads from `TssParams.PreParamsTimeout` (defaults to 1min if unset).

3. **Error logging for preparams**: Added `log.Error` when preparams generation fails
   (was silently swallowed).

### Infrastructure fixes

| Fix | Files | Problem |
|-----|-------|---------|
| HAF shared_buffers | `hive.go`, `docker-compose.yml` | pgtune.conf in wrong path, HAF defaulted to 16GB |
| devnet-setup init order | `cmd/devnet-setup/main.go` | `SetDbURI()` after `Init()` → DB tried localhost |
| devnet-setup error handling | `cmd/devnet-setup/main.go` | `a.Init()` unchecked → nil panic |
| Startup order | `devnet.go` | Drone must start before devnet-setup |
| Drone config | `testdata/drone.yaml` | New version needs `operator_message`, `translate_to_appbase`, `equivalent_methods` |
| Drone healthcheck | `docker-compose.yml` | No wget in container → `bash /dev/tcp` |
| Dockerfile gqlgen | `Dockerfile.devnet` | GQL schema changes need `gqlgen generate` before build |
| Port conflicts | all test configs | P2PBasePort 10720 → 11720 (avoids mainnet/testnet) |
| rawOperation JSON | `funding.go` | Missing `MarshalJSON()` for hivego broadcast |
| SkipFunding | `config.go`, `devnet.go` | TSS tests don't need contract deploy funds |
| PreParamsTimeout | `tss.go`, `params.go` | Hardcoded 1min → configurable |
| Data dir perms | `devnet.go` | devnetDir needs 0o777 for container app user |
| TSS key seeding | `tss_helpers_test.go` | `insertTssKey()` for tests without contract deploy |
| DB name | `tss_helpers_test.go` | Nodes use `magi-N`, not `go-vsc` |
| Old-code Dockerfile | `devnet.go` | Old code needs `gqlgen generate`; custom Dockerfile |
| Reshare timeout | test configs | 30s → 2min (175KB messages need propagation time) |
| Rotate interval | test configs | 10 → 20 blocks (60s between attempts) |
| Wait timeouts | `recovery_full_test.go` | 3min → 5min for block waits during blame cycles |
| SSID assertions | `blame_cycle_test.go` | Relaxed to warnings where disconnected nodes cause expected mismatches |

---

## Key Discoveries

### 1. On-chain readiness gate is highly effective
The `vsc.tss_ready` architecture works so well that offline/disconnected
nodes are excluded BEFORE they can cause damage. Blame is rarely needed —
the readiness gate prevents the problem. This is exactly the design intent.

### 2. TSS keygen requires a contract call
`FindNewKeys()` needs a key with `status: "created"`, which is created by
contracts calling `tss.create_key`. Tests either deploy the call-tss
contract (Milo's tests) or seed directly via MongoDB (`insertTssKey()`).

### 3. Reshare won't trigger until epoch advances
`FindEpochKeys(epoch)` uses strict `epoch < currentEpoch`. A key at epoch N
needs election epoch N+1 before reshare triggers.

### 4. Reshare messages are ~175KB
Round 1 messages contain Paillier proofs. 30s timeout isn't enough for 5-7
nodes to exchange them. 2 minutes works reliably.

### 5. Leader crash is recoverable
When the leader dies during BLS collection, the commitment is lost but the
network recovers on the next cycle with a different leader. No manual
intervention needed.

### 6. Simultaneous restart works
All 7 nodes restarting at once (coordinated deployment) is recoverable.
Preparams regenerate in ~20-30s, and reshare succeeds within 1-2 cycles.

### 7. Flapping nodes don't prevent convergence
A node toggling online/offline every cycle gets correctly blamed when
offline and re-included when online. The network makes continuous progress.

---

## File layout (test files)

```
tests/devnet/
  tss_helpers_test.go         Shared helpers, config, startDevnet
  reshare_happy_test.go       TestTSSReshareHappyPath
  readiness_test.go           TestTSSOfflineNodeExcludedByReadiness, TestTSSFalseReadinessProducesBlame
  blame_cycle_test.go         TestTSSBlameExcludesNodeNextCycle, TestTSSBlameEpochDecode, TestTSSBlameAccumulation
  blame_disconnect_test.go    TestBlameDisconnectedNode
  partition_recovery_test.go  TestTSSPartitionAndRecovery
  multiversion_test.go        TestTSSMultiVersion
  recovery_full_test.go       TestTSSFullRecoveryCycle
  edge_cases_test.go          7 TestEdge* tests (leader crash, partition, restart, etc.)
  blame_test.go               TestBlameExcludesNodeOnRetry (Milo)
  blame_ssid_test.go          TestBlameSSIDMismatch (Milo)
  blame_partial_test.go       TestBlamePartialLatency (Milo)
  blame_bidir_test.go         TestBlameBidirectionalLatency (Milo)
```

## Running tests

```bash
# Single test (~10-15 min)
go test -v -run TestTSSReshareHappyPath -timeout 25m ./tests/devnet/

# All our tests (~2-3 hours sequential)
go test -v -run 'TestTSS|TestBlameDisconnectedNode|TestEdge' -timeout 300m ./tests/devnet/

# Keep containers for debugging
DEVNET_KEEP=1 go test -v -run TestTSSReshareHappyPath -timeout 25m ./tests/devnet/

# Run one at a time on loaded servers (recommended)
for t in TestTSSReshareHappyPath TestEdgeLeaderCrashDuringBLS TestEdgePartitionNoQuorum; do
  go test -v -run $t -timeout 30m ./tests/devnet/ && echo "PASS: $t" || echo "FAIL: $t"
  sudo rm -rf .devnet/
done
```
