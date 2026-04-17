# Health Checks and Retrying Failed VSC Transactions

This guide covers the two HTTP endpoints operators use day-to-day:

- `GET /health` — liveness and operational status (anonymous), plus a gated list of failed VSC L2 transactions (authenticated).
- `POST /retry` — re-submit persisted `FAILED` VSC L2 transactions by their tx ID.

See [README.md](README.md) for the full HTTP API reference.

## Prerequisites

The authenticated `/health` view and `/retry` both require the **`OpsApiKey`** bearer token. This is a different token from `SignApiKey` (which only `/sign` accepts) — the split exists so a frontend or monitoring agent can be granted ops access without also gaining the ability to sign Bitcoin transactions.

Both tokens live in the bot's `config.json` under the `-data-dir` path (default `./data/config.json`). If `OpsApiKey` is unset, `/retry` returns 403 and `/health` hides the failed-tx list.

Generate tokens — for example:

```bash
openssl rand -base64 32   # for OpsApiKey
openssl rand -base64 32   # for SignApiKey (different value)
```

Set them in `config.json`:

```json
{
  "SignApiKey": "sign-token-here",
  "OpsApiKey": "ops-token-here"
}
```

Restart the bot. The examples below assume the ops token is exported:

```bash
export BOT_URL="http://localhost:8000"
export OPS_API_KEY="ops-token-here"
```

---

## Checking health

### Anonymous (monitoring probe)

```bash
curl -s "$BOT_URL/health" | jq
```

Typical healthy response (HTTP 200):

```json
{
  "status": "ok",
  "blockHeight": 2451088,
  "lastBlockAt": "2026-04-16T18:22:15Z",
  "pendingSentTxs": 0
}
```

Unhealthy response (HTTP 503):

```json
{
  "status": "unhealthy",
  "blockHeight": 2451088,
  "lastBlockAt": "2026-04-16T18:22:15Z",
  "pendingSentTxs": 2,
  "issues": [
    "2 txs broadcast but unconfirmed after 2+ blocks",
    "3 VSC transaction(s) failed"
  ]
}
```

The anonymous view includes the failed-tx _count_ in `issues` but not the tx IDs or error text. That is enough for a liveness probe to fire an alert without leaking operational detail.

> Note: the endpoint returns **503** when `status` is `unhealthy`. If you use `curl -f`, a degraded bot will exit non-zero. Drop `-f` (or accept 503) if you want to parse the body.

### Authenticated (full detail)

Add the bearer token to expose `failedVscTxs`:

```bash
curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" | jq
```

```json
{
  "status": "unhealthy",
  "blockHeight": 2451088,
  "lastBlockAt": "2026-04-16T18:22:15Z",
  "pendingSentTxs": 0,
  "failedVscTxs": [
    {
      "txId": "bafyreigd...",
      "action": "map",
      "payload": { "...": "..." },
      "failedAt": "2026-04-16T17:55:03Z"
    }
  ],
  "issues": ["1 VSC transaction(s) failed"]
}
```

The list is capped at the 100 most recent failures, newest first. Each record contains:

| Field           | Meaning                                                        |
| --------------- | -------------------------------------------------------------- |
| `txId`          | VSC L2 tx CID — the value to pass to `/retry`.                 |
| `action`        | `"map"` or `"confirmSpend"`.                                   |
| `payload`       | Original contract-call arguments, kept for re-submission.      |
| `failedAt`      | When the bot observed `FAILED` status on-chain.                |
| `lastRetriedAt` | Present if `/retry` has run for this tx (used for throttling). |

Extract just the IDs for piping into a retry:

```bash
curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" \
  | jq -r '.failedVscTxs[].txId'
```

---

## Retrying failed transactions

### Single tx

```bash
curl -s -X POST "$BOT_URL/retry" \
  -H "Authorization: Bearer $OPS_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"txIds": ["bafyreigd..."]}' | jq
```

Response (always HTTP 200 if the request itself was accepted):

```json
[{ "txId": "bafyreigd..." }]
```

A per-tx failure shows up in the `error` field:

```json
[{ "txId": "bafyreigd...", "error": "throttled: retried <2m ago" }]
```

### Multiple txs

```bash
curl -s -X POST "$BOT_URL/retry" \
  -H "Authorization: Bearer $OPS_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"txIds": ["bafy1...", "bafy2...", "bafy3..."]}' | jq
```

### Retry everything in the failed store

Pipe the authenticated health output into a single retry call:

```bash
ids=$(curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" \
  | jq -c '[.failedVscTxs[].txId]')

curl -s -X POST "$BOT_URL/retry" \
  -H "Authorization: Bearer $OPS_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"txIds\": $ids}" | jq
```

### Throttles

Two throttles apply and are both enforced server-side:

- **Global**: at most one retry request proceeds every 10 seconds. Additional requests inside the window are rejected with a 429-style body before any per-tx state is touched.
- **Per-tx**: the same `txId` may be retried at most once every 2 minutes. A throttled tx shows up in the response as `{"txId": "...", "error": "throttled..."}` but other txs in the same batch still run.

Retry takes the original payload stored at failure time, re-submits it via the VSC L2 tx pool (under a new nonce/CID), and deletes the original record. If the retried tx fails again on-chain, the fresh failure record takes the original's place — `failedVscTxs` always contains **at most one entry per logical operation**, with the CID of the most recent attempt.

### Failures are never auto-retried

The bot does **not** automatically re-broadcast a transaction that reached `FAILED` on-chain. `FAILED` is treated as a terminal signal that something is wrong with the payload (bad address, RC exhaustion, contract rejection), so the tx is persisted to the failed store for an operator to inspect before deciding whether `/retry` is appropriate.

The only automatic retry is for **broadcast failures** — network errors or RPC unavailability where the transaction was never accepted by the VSC node. Those are retried up to 3 times with exponential backoff, and only one `FailedVscTx` entry is ever written per logical call (since no CID is assigned until broadcast succeeds).

### HTTP status codes for `/retry`

| Code | Meaning                                                          |
| ---- | ---------------------------------------------------------------- |
| 200  | Request accepted. Check per-tx `error` in response body.         |
| 400  | Invalid JSON, empty `txIds`, or a `txId` that isn't a valid CID. |
| 401  | `Authorization` header missing or doesn't match `OpsApiKey`.     |
| 403  | Endpoint disabled because `OpsApiKey` is empty in config.        |

---

## Common workflows

### Daily triage

```bash
# 1. Quick anonymous probe (for dashboards / uptime checks)
curl -sf "$BOT_URL/health" > /dev/null && echo OK || echo DEGRADED

# 2. If degraded, pull detail
curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" | jq '.issues, .failedVscTxs'
```

### Inspect a specific tx before retrying

```bash
curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" \
  | jq '.failedVscTxs[] | select(.txId == "bafyreigd...")'
```

### Bulk retry once per hour (cron-friendly)

```bash
#!/usr/bin/env bash
set -euo pipefail
: "${BOT_URL:?}"; : "${OPS_API_KEY:?}"

ids=$(curl -s -H "Authorization: Bearer $OPS_API_KEY" "$BOT_URL/health" \
  | jq -c '[.failedVscTxs[].txId // empty]')

# Skip if nothing to do
[ "$ids" = "[]" ] && exit 0

curl -s -X POST "$BOT_URL/retry" \
  -H "Authorization: Bearer $OPS_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"txIds\": $ids}" | jq
```

---

## Troubleshooting

| Symptom                                                      | Likely cause                                                                                                                          |
| ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| `/health` returns 503 with `block processing stale for Ns`   | Bot isn't seeing new chain blocks. Check chain RPC.                                                                                   |
| `/health` returns 503 with `N txs broadcast but unconfirmed` | L2 tx pool or witness signing is lagging.                                                                                             |
| `/retry` returns 403                                         | `OpsApiKey` is unset in `config.json`.                                                                                                |
| `/retry` returns 401                                         | Bearer token doesn't match `OpsApiKey` in `config.json` (note: `SignApiKey` does **not** work on `/retry`).                           |
| `/retry` per-tx `error: invalid txId`                        | ID isn't a valid IPFS CID — check for trailing newline or wrong ID type (Hive txs are 64-char hex and not accepted here).             |
| `failedVscTxs` missing from `/health`                        | Request is unauthenticated. Add `Authorization: Bearer <OpsApiKey>`, or confirm `OpsApiKey` is set.                                   |
| Retry seems to succeed but tx reappears in `failedVscTxs`    | Contract is rejecting the payload itself (bad address, insufficient RCs). Inspect `payload` and contract state before retrying again. |
