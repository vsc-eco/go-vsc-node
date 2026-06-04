# IS Service — Operator Runbook

The IS (InstantSend) service mediates Dash-InstantSend login: a user pays a
small DASH dust to a session-specific deposit address, the service watches
dashd, collects validator BLS attestations on the IS-lock, and submits the
attested bundle to the L2 mapping contract as a `mapInstantSendV2` call.

This document is the **operator-facing** view: how to deploy, configure,
monitor, and roll the service back. Code-level audit references live inline
in `args.go` next to each flag; this runbook expands the per-flag help into
deployable templates and per-action sequencing.

> **Audience.** Operators bringing a witness fleet up against either testnet
> or mainnet. Assumes familiarity with the contract toolchain
> ([docs.magi.eco](https://docs.magi.eco/)), the `magi-deployer` workflow,
> and the witness libp2p mesh.

---

## 1. Flag reference (production deploy template)

All flags also accept `-flag=value` form. Required = the service refuses to
start without it. **Bold flag names** flag-up the values you almost certainly
need to change per-deploy.

| Flag | Default | Required | Purpose |
|---|---|---|---|
| **`-network`** | `testnet` | yes (mainnet) | `mainnet` \| `testnet` \| `devnet`. Drives the CAIP-2 genesis-hex used in the DashDID and the bridge address derivation. `devnet` is test-only (the IS service refuses some production gates — see SEC-3 / SEC-6). |
| **`-chainID`** | derived from `-network` | no | Override the Magi chain ID baked into the canonical signed message. `mainnet`→`vsc-mainnet`, `testnet`→`vsc-testnet`, `devnet`→`vsc-devnet`. Set this only if you've pinned a non-default chainID at the contract layer too — mismatch surfaces as silent ATTESTATION_TIMEOUT (R3-OP-010). |
| **`-primaryPubkey`** | — | **yes** | 33-byte hex compressed secp256k1 pubkey of the bridge TSS primary key. Stamped into every deposit address. |
| **`-backupPubkey`** | — | **yes** | 33-byte hex compressed pubkey for the backup TSS key. |
| **`-addressSignerSecret`** | empty | dev/test only | HMAC secret for signing `(deposit_address, instruction)` tuples. **DEPRECATED** for production — use `-signerVaultAddr` (Vault Transit) or `-addressSignerEd25519KeyFile` instead. The HMAC stub is gated by the explicit DEV/TEST log on every startup. |
| `-addressSignerEd25519KeyFile` | empty | production-shape | Path to a 32-byte Ed25519 seed (hex-encoded, 0o600). Asymmetric signer — the private key lives on disk, the derived public key is pinned in Altera's `PUBLIC_IS_SERVICE_SIGNER_PUBKEY`. Spec §5.7 compatible but the private key still on the IS-service host's filesystem. Takes precedence over `-addressSignerSecret`. |
| **`-signerVaultAddr`** | empty | production-recommended | HashiCorp Vault address (e.g. `https://vault.internal:8200`). When set, the IS service signs every `/session/start` via Vault's `transit/sign` API — **the private key never leaves Vault**. Spec §5.7's strongest interpretation. Takes precedence over both the Ed25519 file path and the HMAC stub. See §1.3 below for the Vault setup recipe. |
| `-signerVaultMount` | `transit` | no | Vault transit-engine mount path. |
| `-signerVaultKeyName` | empty | yes (with `-signerVaultAddr`) | Vault transit key name. Must be `type=ed25519` so the on-wire signature shape matches the other signer kinds. |
| `-signerVaultToken` | empty | no | Vault token inline. **DEV/TEST ONLY — REJECTED on any -network != devnet** (SEC-6 / R16-SEC-sec6-testnet-not-gated). Tokens leak via process tables / shell history / kubectl describe / docker inspect. Use `-signerVaultTokenFile`. |
| `-signerVaultTokenFile` | empty | yes (one of) | Path to a file containing the Vault token. Whitespace trimmed. Operator should set 0o600. Alternative: `VAULT_TOKEN` env. |
| **`-port`** | `3030` | no | HTTP listen port. |
| **`-sessionTTLMinutes`** | `30` | no | How long a session stays open before the server marks it expired. Don't shrink below the user-side trap deadline (240s post-cancel) or you'll race the client. |
| **`-dashdRPC`** | empty | recommended | dashd JSON-RPC URL for the IS_OBSERVED transition (e.g. `http://vsc-dashd-testnet:9998`). When unset, IS_OBSERVED must be driven externally and the happy-path tests skip the dashd-driven branch. URL parse rejects userinfo, query, fragment, smuggled `;` paths (R9–R12 audit). |
| `-dashdRPCUser` | `vsc-node-user` | no | dashd basic-auth user. |
| `-dashdRPCPassword` | `vsc-node-pass` | no | dashd basic-auth password. |
| **`-l2GqlURL`** | empty | yes (real submission) | VSC GraphQL endpoint (e.g. `https://api.vsc.eco/api/v1/graphql`). Empty = the log-only submitter; no on-chain effect, useful for staging. |
| **`-l2PrivKey`** | empty | yes (real submission) | secp256k1 private key hex. The derived `did:pkh:eip155` account pays RC for each submission — **must be HBD-funded** before the first L2 tx. Treat as a hot wallet. |
| **`-l2DashMappingContract`** | empty | yes (real submission) | `vsc1...` contract ID of the dash-mapping-contract on the chain you're submitting to. Required when `-l2GqlURL` + `-l2PrivKey` are set. |
| **`-l2RcLimit`** | `1000` | no | Per-tx RC budget. 1000 RC ≈ 1 HBD; safe upper bound for `mapInstantSendV2`. Lower to save HBD, raise if txs abort with insufficient RC. |
| **`-p2pBootstrapPeers`** | empty | yes (production) | Comma-separated libp2p multiaddrs. When set, the service joins the validator islock-attestation gossip and uses the real broadcaster + collector. Empty = no-op broadcaster (no on-network effect). |
| `-p2pListenAddrs` | `/ip4/0.0.0.0/tcp/0`, `/ip6/::/tcp/0` | no | Override only if you need a fixed port or to bind a specific interface. |
| **`-trustedProxies`** | empty | yes (behind LB) | Comma-separated trusted reverse-proxy hosts/IPs. `X-Forwarded-For` from these is honoured for rate-limiting. Loopback always trusted. Audit TC2-06. |
| `-validatorSetCacheTTLSeconds` | `30` | no | TTL for the per-epoch validator-set cache (R4-001 + R5-DRIFT-06). Lower = faster admin-rotation reflection; higher = fewer L2 GraphQL probes. Safe range 5–300s. |
| `-drainTimeoutSeconds` | `240` | no | Graceful-shutdown drain in seconds. **Must remain ≥ 240** (= 15s CollectTimeout + 30s SubmitTimeout + ~180s reconcileL2 incl. sleeps) or in-flight reconciles get killed mid-flight. Round-3 audit R3-05. |
| `-debug` | `false` | no | Verbose logging. Don't run in prod (PII risk: full L2 payloads logged). |

### 1.1 Minimal viable testnet command

```bash
./is-service \
  -network=testnet \
  -primaryPubkey=02aaaa...   -backupPubkey=03bbbb... \
  -dashdRPC=http://vsc-dashd-testnet:9998 \
  -dashdRPCUser=vsc-node-user -dashdRPCPassword=vsc-node-pass \
  -l2GqlURL=https://api.vsc.eco/api/v1/graphql \
  -l2PrivKey=$IS_L2_PRIVKEY \
  -l2DashMappingContract=vsc1BnMAaeUzhzVcfKMDG5vphthhymk6irjLNq \
  -p2pBootstrapPeers=/ip4/A.B.C.D/tcp/4001/p2p/12D3Koo... \
  -trustedProxies=10.0.0.1,10.0.0.2
```

### 1.2 Mainnet differences

- `-network=mainnet`, `-chainID=vsc-mainnet` (or omit and let it derive).
- `-l2DashMappingContract` = the mainnet contract ID (set at contract-deploy time; capture from the deploy log).
- **`-addressSignerSecret` must be replaced with either `-signerVaultAddr` (Vault Transit, recommended per spec §5.7) OR `-addressSignerEd25519KeyFile` (file-based Ed25519)**. The HMAC stub still works (gated by an explicit DEV/TEST log) but is deprecated for production. See §1.3 below for the Vault Transit setup recipe; the file-based signer is a one-liner: point the flag at a 0o600 32-byte-hex seed file.
- `-p2pBootstrapPeers` = mainnet validator fleet multiaddrs. Empty = service runs but no real attestations land.
- Client-side: set `PUBLIC_DASH_NETWORK=mainnet` and add the production IS host to `MAINNET_HOST_SUFFIXES` in `src/lib/auth/dash/config.ts` *before* flipping the URL (otherwise the client rejects the URL/network mismatch).

### 1.3 Vault Transit signer — operator recipe

Vault Transit is the production-recommended signer backend (spec §5.7 compliant — the Ed25519 private key never leaves the Vault process). Setup is one-shot per IS service deployment:

```bash
# 1. Enable the transit secrets engine (one-time, idempotent).
vault secrets enable transit

# 2. Create the IS service's signing key. Type MUST be ed25519 so
#    the wire-format signature shape matches Altera's verification.
vault write -f transit/keys/is-service-signer type=ed25519

# 3. Read out the public key. Pin this hex (the 32-byte raw pubkey
#    expressed as 64 hex chars) in Altera's
#    PUBLIC_IS_SERVICE_SIGNER_PUBKEY env var.
vault read transit/keys/is-service-signer
#   → look at `data.latest_version` (e.g. 1, or higher after rotations),
#     then grab `data.keys.<that-version>.public_key` (base64),
#     decode it, hex-encode the 32-byte result.
#   The IS service reads the SAME latest_version + public key at
#   startup and logs the hex ("address signer: Vault Transit ...
#   pubkey=<hex>") — match against Altera's pin.

# 4. Create a scoped policy + token for the IS service. Only grants
#    update on transit/sign/<key> + read on transit/keys/<key>.
cat <<'EOF' | vault policy write is-signer -
path "transit/sign/is-service-signer"   { capabilities = ["update"] }
path "transit/keys/is-service-signer"   { capabilities = ["read"]   }
EOF

# 5. Issue a renewable token bound to that policy. Period defines
#    the renewal window — set to a few hours so a leaked token has
#    a bounded TTL even if cleanup misfires.
vault token create -policy=is-signer -period=6h -display-name=is-service

#   → put the `token` field into a file readable only by the IS
#     service uid:
echo '<token>' > /etc/is-service/vault.token
chmod 0600 /etc/is-service/vault.token

# 6. Set up token-renewal as a sidecar / cron / systemd timer:
#    every ~3h, call `vault token renew` and rewrite the token
#    file. The IS service re-reads the file on every sign call.
#    (Renewal isn't built into the IS service itself — that's
#    deliberate; operators are best positioned to plug into their
#    existing secret-rotation tooling.)
```

Then start the IS service with:

```bash
./is-service \
  ...
  -signerVaultAddr=https://vault.internal:8200 \
  -signerVaultKeyName=is-service-signer \
  -signerVaultTokenFile=/etc/is-service/vault.token
```

At startup the IS service fetches the public key from Vault + logs its hex. The IS service refuses to start if Vault is unreachable, the token is invalid, or the key isn't `type=ed25519` — fail-fast guarantees you never silently issue unverifiable signatures.

The IS service captures Vault's `latest_version` at startup and pins `key_version=<startup>` in every `Sign()` request. A Vault key rotation while the IS service is running is therefore a NO-OP for ongoing requests; rotation requires a coordinated IS service restart + Altera `PUBLIC_IS_SERVICE_SIGNER_PUBKEY` update (see audit SEC-1).

---

## 2. Per-deploy admin actions (mapping contract)

After deploying the mapping contract to a fresh network, the operator must
run these admin actions **in order** before any user `mapInstantSendV2`
calls succeed. All are gated by the mapping contract's `checkAdmin()` and
must be signed by the deployer identity (`hive:magi.contracts` on testnet;
oracle on mainnet).

| # | Action | RC | Gate | Depends on | Purpose |
|---|---|---|---|---|---|
| 1 | `setForwarderContractId(vsc1...)` | ~500 | admin | — | Pin the canonical dash-forwarder-contract ID. **Immutable on mainnet after first write** — do not run twice. |
| 2 | `seedBlocks(height, headers...)` | 5000–10000 | admin | — | Seed initial Dash block headers + height. Mainnet: one-shot, idempotent; testnet: replaceable. |
| 3 | `setValidatorSet(epoch, payload)` | ~3000 | admin | step 2 | Register a 4-field payload `<did>=<pk>=<pop>=<account>` per validator for `epoch`. Payload format: `<epoch>;<entry>\|<entry>\|...`. Until this runs, NO attestations verify. Real BLS PoP via `cmd/gen-validator-set-payload`. |
| 4 | `setMinAttestations(n)` | ~200 | admin | step 3 with ≥1 epoch | Set the N-of-M quorum threshold for fast-path verification. Default 1; mainnet bring-up: raise to ≥2M/3+1 **before** opening the service to users. |
| 5 | `addAllowedTarget(vsc1..., unlockBlock)` | ~500 | admin | step 1 | Propose a target for the op=call allow-list (e.g. magi-dex router). Enters a 7-day (86_400 block) timelock. |
| 6 | `commitAllowedTarget(vsc1...)` | ~300 | permissionless | step 5 timelock elapsed | Promote the queued target to active. Anyone can call once the unlock block is reached. |

**Recipes.** The payload for step 3 must be produced by
`dash-mapping-contract/cmd/gen-validator-set-payload` so the announcer-side
BLS PoP (`domain || pkBytes || accountBytes`) matches what the contract's
`SaveValidatorSetForEpoch` reconstructs. Run with `--help` for the
flag list; `cmd/gen-validator-set-payload/main.go` is the canonical
source. R4-CSM-01 critical fix coverage relies on this binding.

**Rollback.** If step 3 lands with a bad payload, immediately call it again
with a corrected payload for the **same epoch** — the contract overwrites
in place. The validator-set cache TTL bounds the staleness window
(`-validatorSetCacheTTLSeconds`, default 30s). For step 1, no rollback is
available on mainnet (immutable); pause the contract via the pause action
before any production traffic if the wrong forwarder was wired.

---

## 3. Sequencing the rollout

A clean witness-fleet bring-up runs as:

1. **Operator side** — deploy mapping contract, capture contract ID.
2. **Operator side** — run admin steps 1–4 above (forwarder ID, seed
   blocks, validator set for the current epoch, min-attestations).
3. **Witness side** — each witness operator wires `-p2pBootstrapPeers` to
   the published bootstrap list, brings up the IS service, confirms gossip
   peers via the service's structured logs.
4. **Witness side** — fund the L2 signer key (`-l2PrivKey`) with HBD per
   the size-of-the-fleet RC ledger.
5. **Client side** — set `PUBLIC_IS_SERVICE_URL` + `PUBLIC_DASH_NETWORK`
   (+ optional `PUBLIC_IS_SERVICE_SIGNER_PUBKEY` pin) and rebuild Altera.
6. **Operator side** — run admin step 5–6 (allow-list governance) if any
   custom op=call targets need to clear timelock before user traffic.
7. **Open the gates** — point user traffic at Altera.

---

### 3.1 Rate limits (built-in per-IP buckets)

Audit-driven caps applied at the HTTP layer; clients exceeding them
get `429 Too Many Requests`. Buckets are per-source-IP, where the IP
comes from `X-Forwarded-For` (rightmost) when the peer is in
`-trustedProxies` and from `RemoteAddr` otherwise.

| Endpoint | Cap | Audit | Rationale |
|---|---|---|---|
| `POST /session/start` | 10 / min | TC2-06 / SEC-4 (R15) | Each call creates a watched address + session-store entry — relatively expensive. |
| `GET /session/{sid}/status` | 60 / min | R16-SEC-status-cancel-no-rate-limit | Altera polls at ~2s = 30/min per tab; 60/min covers 2 tabs with headroom. Bucket is independent of `/cancel`. |
| `POST /session/{sid}/cancel` | 10 / min | R16-SEC-status-cancel-no-rate-limit + R17-CORR-status-cancel-shared-bucket-multi-tab-cancel-fails | Cancel is one-shot per session in normal flows. Separate from `/status` so a hot polling flow can't lock the user out of cancelling. |
| `POST /test/observed/{sid}`, `POST /test/attestation/{sid}` | bodies capped at 64 KiB (SEC-11) | R15-SEC-11 | Test endpoints; only registered when `-testBypassDashdISLock=true` (devnet-only). |

Operators behind a CDN / WAF can stack their own rate limits on top
— these are defense-in-depth against direct-to-pod traffic.

---

## 4. Monitoring + log patterns

Operationally useful greps (search the structured log stream for these
prefixes):

| Pattern | Meaning | Action |
|---|---|---|
| `Dash session: /status backoff` (client console) | Per-session: IS `/status` is failing. Logged on first failure and on transition into the 30s cap. R14-OPS-BACKOFF-WARN-FLOODS-AT-CAP edge-trigger. | Spike across users → IS service degraded. |
| `Dash session: /status backoff recovered after N consecutive failures` (client) | Session recovered from backoff (R14 promoted to console.warn). | Confirms transient blip cleared. |
| `IS session failed with operator detail:` (client console) | The server returned a terminal `forwardError`. Operator-language string follows. R3-CSM-10. | Inspect chain state to recover. |
| `validator-set cache miss (epoch=N)` (service logs) | Cache TTL expired or never populated for that epoch. | Confirms `-validatorSetCacheTTLSeconds` tuning. |
| `attestation rejected: unknown pubkey for did=...` (service logs) | A peer signed with a pubkey that isn't in the registered set for the epoch. | Validator-set freshness issue OR malicious peer. |
| `RC limit exceeded` (in tx error tail) | `-l2RcLimit` set too low for the current payload size. | Raise `-l2RcLimit`, top up signer HBD. |
| `drain timeout exceeded; force-exiting with N in-flight` (service shutdown) | `-drainTimeoutSeconds` too low for the production reconcile budget. | Restore default 240s; investigate slow L2. |

Suggested alert rules (Prometheus-shape, when metrics endpoint lands):

- `is_status_5xx_rate_5m > 0.01` for >2m — operator paging.
- `is_l2_submit_failure_rate_5m > 0.05` for >5m — non-paging warn.
- `is_validator_set_cache_miss_rate_5m > 0.5` for >10m — investigate; the
  cache should hit 90%+ in steady state.
- `is_forward_queue_pending_count > 100` — backlog growth; user-side
  symptom is rising `/status backoff` flood.

---

## 5. Common operational scenarios

### 5.1 Stuck session after dashd outage

Symptom: Client console shows repeated `/status backoff`. The IS service
returned 5xx, the user is at the 30s steady cadence (post R14 edge-trigger
the warn fires once at cap-entry).

Resolution:
1. Confirm dashd is up. The IS service depends on `-dashdRPC` for
   IS_OBSERVED transitions.
2. Once dashd recovers, the next happy-path `/status` resets `pollErrCount`
   and the client logs `recovered after N consecutive failures`.
3. If a stuck user-facing modal exhausts the 240s trap deadline, the user
   sees `cancel acknowledged but session did not finish on the server in
   time`. They can retry; the new session is independent.

### 5.2 Validator rotation mid-epoch

The contract's epoch model assumes a fresh `setValidatorSet(epoch+1, ...)`
lands BEFORE epoch+1 begins. If you miss the boundary, the contract falls
back to epoch N's set for `ValidatorSetGraceBlocks` (1200 blocks ≈ 1 hour
at 3s Hive blocks). Beyond that window the fast-path verifier rejects.

To rotate cleanly:

1. Build the new payload via `gen-validator-set-payload --epoch=$((N+1)) ...`.
2. Submit `setValidatorSet` ahead of the boundary.
3. Confirm via `getStateByKeys vs-$((N+1))` that the new set landed.
4. Witnesses pick up the new set automatically at the next cache TTL
   (`-validatorSetCacheTTLSeconds`, default 30s).

### 5.3 Rolling back a bad IS-service deploy

The service is stateless beyond the in-memory session map and the libp2p
mesh registration. Safe rollback:

1. Stop the new binary (SIGTERM honours `-drainTimeoutSeconds`).
2. Re-launch the prior binary with the same flags.
3. In-flight sessions either complete (server-side 30-min TTL) or expire;
   the client maps both to `failed` via the trap deadline.

If the bad deploy submitted partial L2 transactions, those land or fail on
their own merits — the contract's idempotency means duplicate submissions
are rejected without state mutation.

---

## 6. Open items for mainnet flip

- [ ] `-addressSignerSecret` HMAC → either `-signerVaultAddr` Vault Transit (recommended, spec §5.7, see §1.3 above for the operator-runnable recipe) OR `-addressSignerEd25519KeyFile` (file-based Ed25519 signer; same wire format as Vault, the private key lives on the IS-service host filesystem at 0o600).
- [ ] `PUBLIC_IS_SERVICE_SIGNER_PUBKEY` pinned in Altera (currently
      falls back to visual fingerprint when blank).
- [ ] `MAINNET_HOST_SUFFIXES` populated in `src/lib/auth/dash/config.ts`.
- [ ] Mainnet validator-set epoch cadence + `setValidatorSet` cron wired.
- [ ] Prometheus metrics endpoint enabled + scrape targets configured.
- [ ] Operator dashboards (Grafana) for IS-service latency, L2 submit
      rate, forward-queue depth, validator-set cache hit rate.
- [ ] Mainnet `-l2RcLimit` validated against largest realistic payload.
- [ ] HBD top-up cron for `-l2PrivKey`-derived signer account.

When all checked: the service is ready for the mainnet cutover.
