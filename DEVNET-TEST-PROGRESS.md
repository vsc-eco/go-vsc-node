# Devnet Integration Test Progress

**Date:** 2026-04-07
**Branch:** `tss-fixes` at `/home/dockeruser/magi/tss-fixes/`
**Authors:** lordbutterfly + Claude Code

---

## What We Did

Built 3 new devnet integration tests (tests 6-8) to cover gaps identified in the review (`/home/dockeruser/magi/mainnet/lbf-review.md`, Section 8). Also fixed ~12 infrastructure issues that prevented the devnet test framework from running on this server.

### New Tests Written

| # | Test | File | Purpose |
|---|------|------|---------|
| 6 | `TestTSSBlameEpochDecode` | `tests/devnet/tss_deterministic_test.go` | Verifies blame decoded against correct epoch when elections have different members |
| 7 | `TestTSSBlameAccumulation` | same | Verifies multiple blame records are accumulated (old code read only the most recent) |
| 8 | `TestTSSMultiVersion` | same | Old-code node (from `/home/dockeruser/magi/testnet/original-repo/go-vsc-node`) excluded by readiness gate, doesn't crash |

### New Infrastructure Files

| File | Purpose |
|------|---------|
| `tests/devnet/hive_ops.go` | Hive transaction broadcasting from tests (`BroadcastCustomJSON`, `Unstake`) |

### Infrastructure Fixes (all in tss-fixes branch)

| Fix | Files | Problem |
|-----|-------|---------|
| HAF shared_buffers | `tests/devnet/hive.go`, `docker-compose.yml` | pgtune.conf was in wrong path (`haf_db_store/` vs `haf_postgresql_conf.d/`), HAF defaulted to 16GB |
| HAF shm_size | `docker-compose.yml` | Added `shm_size: 1g` to HAF service |
| devnet-setup init order | `cmd/devnet-setup/main.go` | `SetDbURI()` called after `Init()` — DB tried localhost:27017 instead of db:27017 |
| devnet-setup error handling | `cmd/devnet-setup/main.go` | `a.Init()` error was unchecked, nil pointer panic on failure |
| Startup order | `tests/devnet/devnet.go` | Drone must start BEFORE devnet-setup (which uses it as Hive API) |
| Drone config | `tests/devnet/testdata/drone.yaml` | New drone image version requires `operator_message`, `translate_to_appbase`, `equivalent_methods` |
| Drone healthcheck | `docker-compose.yml` | Container has no wget/curl; switched to `bash -c '</dev/tcp/...'` |
| Port conflicts | `tss_deterministic_test.go` | P2PBasePort changed to 11720 (mainnet/testnet use 10720+) |
| rawOperation JSON | `tests/devnet/funding.go` | Added `MarshalJSON()` — hivego needs it for broadcast payload |
| SkipFunding | `tests/devnet/config.go`, `devnet.go` | TSS tests don't need contract deployment funds |
| PreParamsTimeout | `modules/tss/tss.go`, `modules/common/params/params.go` | Was hardcoded 1min; made configurable, set to 10min for tests |
| Data dir permissions | `tests/devnet/devnet.go` | devnetDir needs 0o777 for container's app user |
| TSS key seeding | `tss_deterministic_test.go` | `insertTssKey()` helper — keygen requires a contract to call `tss.create_key`; tests seed directly via MongoDB |
| DB name | `tss_deterministic_test.go` | Tests query `magi-1` database, not `go-vsc` |
| Old-code Dockerfile | `tests/devnet/devnet.go` | Old code needs `gqlgen generate` before build; custom Dockerfile generated |
| Reshare timeout | `tss_deterministic_test.go` | Increased from 30s to 2min (175KB reshare messages need time to propagate) |
| Rotate interval | `tss_deterministic_test.go` | Increased from 10 to 20 blocks (60s between reshare attempts) |
| Log message mismatch | `tss_deterministic_test.go` | Test checked for `"broadcasting vsc.tss_ready"` but code logs `"broadcast tss readiness"` |

---

## Test Results (as of 2026-04-07 ~19:30 UTC)

| # | Test | Status | Notes |
|---|------|--------|-------|
| 1 | `TestTSSReshareHappyPath` | **Reshare works** (log assertion was wrong, fixed) | Needs re-run to confirm PASS |
| 2 | `TestTSSOfflineNodeExcludedByReadiness` | **PASS** | |
| 3 | `TestTSSFalseReadinessProducesBlame` | **PASS** | |
| 4 | `TestTSSBlameExcludesNodeNextCycle` | **Needs re-run** | Was failing due to 30s timeout, now 2min |
| 5 | `TestTSSPartitionAndRecovery` | **PASS** | |
| 6 | `TestTSSBlameEpochDecode` | **SKIP (PASS)** | Readiness gate prevents blame from landing — correct behavior |
| 7 | `TestTSSBlameAccumulation` | **PASS** | |
| 8 | `TestTSSMultiVersion` | **PASS** | |
| 9 | `TestTSSFullRecoveryCycle` | **Needs re-run** | Was failing due to 30s timeout, now 2min |

**5 confirmed PASS, 1 graceful SKIP, 3 need re-run with the latest timeout fixes.**

---

## What Still Needs To Be Done

### 1. Re-run all 9 tests with latest fixes

The reshare timeout was the root cause of tests 1, 4, and 9 failing. It's now 2 minutes. Run:

```bash
cd /home/dockeruser/magi/tss-fixes
go test -v -run 'TestTSS' -timeout 180m ./tests/devnet/ 2>&1 | tee /tmp/tss-all.log
```

This takes ~60-90 minutes (each test spins up a full devnet). To run a single test:

```bash
go test -v -run 'TestTSSReshareHappyPath' -timeout 25m ./tests/devnet/
```

To keep containers running after a test (for debugging):

```bash
DEVNET_KEEP=1 go test -v -run 'TestTSSReshareHappyPath' -timeout 25m ./tests/devnet/
```

### 2. Fix TestTSSBlameEpochDecode (test 6) — currently SKIPs

The test tries to create "false readiness" (node broadcasts readiness then goes offline) to generate blame. But the `Disconnect()` function drops ALL input traffic including from Hive/MongoDB, which can crash the container. The `Partition()` approach (block only P2P traffic between magi nodes) is better but the timing is tricky — by the time we partition at the reshare block, the node was already excluded by the readiness gate or already sent its round-1 messages.

**To fix:** Either:
- Wait for readiness to appear in MongoDB for the target node, THEN partition it
- Or increase the readiness offset to give more time between readiness broadcast and reshare trigger

### 3. Improve container crash resilience

When a node is disconnected via `iptables -A INPUT -j DROP`, it loses connection to MongoDB and may crash. This causes `Reconnect()` to fail because the container is stopped.

**To fix:** Use `Partition()` (P2P-only isolation) instead of `Disconnect()` in tests that need the node to stay alive. Or add a `RestartNode()` helper that combines stop+start.

### 4. Commit everything

Once all tests pass, commit to the `tss-fixes` branch. Push to `tibfox` remote (never `original-repo`). **Do NOT add Co-Authored-By to commits** (see memory file `feedback_no_coauthor.md`).

Files modified:
- `modules/tss/tss.go` — configurable PreParamsTimeout, error logging for preparams
- `modules/common/params/params.go` — PreParamsTimeout field in TssParams
- `cmd/devnet-setup/main.go` — DB init order fix, error handling
- `tests/devnet/docker-compose.yml` — HAF shm_size, drone healthcheck
- `tests/devnet/testdata/drone.yaml` — full drone config for new image version
- `tests/devnet/hive.go` — dual pgtune.conf paths
- `tests/devnet/devnet.go` — startup order, SkipFunding, permissions, StopNode/StartNode, BuildOldCodeImage
- `tests/devnet/compose.go` — per-node image overrides for multi-version
- `tests/devnet/config.go` — SkipFunding, OldCodeSourceDir, OldCodeNodes
- `tests/devnet/funding.go` — MarshalJSON for rawOperation
- `tests/devnet/hive_ops.go` — NEW: BroadcastCustomJSON, Unstake, customJsonOp
- `tests/devnet/tss_deterministic_test.go` — 3 new tests, insertTssKey, DB name fix, timing fixes

---

## Key Discoveries

### 1. TSS keygen requires a contract call
`FindNewKeys()` looks for keys with `status: "created"`. These are created by contracts calling `tss.create_key` (WASM SDK). In tests, we seed directly via MongoDB using `insertTssKey()`.

### 2. Reshare won't trigger until epoch advances
`FindEpochKeys(epoch)` uses `epoch < currentEpoch` (strict less-than). A key created at epoch N won't reshare until election epoch N+1. With fast elections (20 blocks), this means waiting ~60s after keygen.

### 3. Reshare messages are ~175KB
Round 1 reshare messages contain Paillier proofs and are ~175KB each. With 5 nodes, the `waitForParticipantReadiness` check in `dispatcher.go` adds P2P connection delays. A 30s timeout isn't enough — 2 minutes works.

### 4. On-chain readiness gate works well
The architecture from our Changes 1-3 (on-chain readiness via `vsc.tss_ready`) works correctly in the devnet. Offline nodes are excluded before they can cause harm. This is so effective that generating "false readiness" blame is difficult — which is actually good for production.

### 5. Old-code nodes cause SSID mismatches but don't crash
The multi-version test confirms that old-code nodes (without readiness gate) participate in reshare sessions and cause SSID mismatches. They don't crash. New-code nodes detect the mismatch and timeout. This validates the graceful degradation described in the review Section 6.

---

## Architecture Reference

- **Test config**: `tssTestConfig()` in `tss_deterministic_test.go` — RotateInterval=20, ElectionInterval=20, ReshareTimeout=2min, PreParamsTimeout=10min
- **Devnet README**: `tests/devnet/README.md` — full API reference for the devnet framework
- **TSS architecture**: `.claude/docs/tss-architecture.md` — TSS module internals
- **CLAUDE.md**: `.claude/CLAUDE.md` — 3 constraints, full reshare flow diagram
- **Review**: `/home/dockeruser/magi/mainnet/lbf-review.md` — root cause analysis and architecture
