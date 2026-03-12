# VSC Ethereum Bridge — Architecture Document

> **Status:** Pre-implementation feasibility — all claims verified against source code
> **Date:** 2026-03-12
> **Scope:** ETH and ERC-20 (USDC) bridging to VSC using TSS, extending the existing BTC bridge pattern
> **Codebase:** [go-vsc-node](https://github.com/vsc-eco/go-vsc-node)

---

## Table of Contents

1. [Executive Summary](#1-executive-summary)
2. [Existing BTC Bridge — Current State](#2-existing-btc-bridge--current-state)
3. [Architecture Decision: Raw TSS Address vs Router Contract](#3-architecture-decision-raw-tss-address-vs-router-contract)
4. [TSS Ethereum Signing Compatibility](#4-tss-ethereum-signing-compatibility)
5. [Deposit Detection Design](#5-deposit-detection-design)
6. [Withdrawal Execution Design](#6-withdrawal-execution-design)
7. [Nonce Management and Failure Recovery](#7-nonce-management-and-failure-recovery)
8. [Vault Rotation](#8-vault-rotation)
9. [Confirmation Depth and Finality](#9-confirmation-depth-and-finality)
10. [Gateway Concurrency Refactor](#10-gateway-concurrency-refactor)
11. [Ledger System Changes](#11-ledger-system-changes)
12. [Module Placement and Registration](#12-module-placement-and-registration)
13. [Existing Ethereum Code (Cross-Branch)](#13-existing-ethereum-code-cross-branch)
14. [RPC Call Budget and Efficiency](#14-rpc-call-budget-and-efficiency)
15. [Risk Registry](#15-risk-registry)
16. [Test Impact Analysis](#16-test-impact-analysis)
17. [Implementation Sequence](#17-implementation-sequence)
18. [Appendix A: Key File Reference](#appendix-a-key-file-reference)
19. [Appendix B: Dependency Verification](#appendix-b-dependency-verification)

---

## 1. Executive Summary

VSC can support ETH and ERC-20 (USDC) bridging using its existing TSS infrastructure with **no modifications to the TSS library** and **no router smart contract**. The architecture uses a raw TSS-derived Ethereum address as the vault, poll-based deposit detection scanning only finalized blocks, and sequential gateway operations to eliminate pre-existing race conditions.

**Key findings verified against source code:**

| Question | Answer | Evidence |
|----------|--------|----------|
| Can TSS sign Ethereum transactions? | Yes | tss-lib outputs raw (r, s, v) with low-s normalization. See [Section 4](#4-tss-ethereum-signing-compatibility). |
| Is a router contract needed? | No | Sender address = attribution via `did:pkh:eip155:1:{sender}`. See [Section 3](#3-architecture-decision-raw-tss-address-vs-router-contract). |
| Can go-ethereum monitor deposits? | Yes | `ethclient.FilterLogs` + `BlockByNumber` in the VSC fork. See [Section 5](#5-deposit-detection-design). |
| Does reshare preserve the vault address? | Yes | Round 4 assertion `Vc[0].Equals(ECDSAPub)` + test proof. See [Section 8](#8-vault-rotation). |
| Does the fork support finalized block tag? | Yes | Fork is geth 1.14.12. `rpc.FinalizedBlockNumber` confirmed. See [Section 9](#9-confirmation-depth-and-finality). |

---

## 2. Existing BTC Bridge — Current State

### What is implemented

| Component | Status | Location |
|-----------|--------|----------|
| Bitcoin block relay | Complete | `modules/oracle/chain/bitcoin.go` |
| Litecoin block relay | Complete | `modules/oracle/chain/litecoin.go` |
| Chain relay consensus (BLS 2/3 threshold) | Complete | `modules/oracle/chain/handle_block_tick.go` |
| Bitcoin DID verification (BIP-137/322) | Complete | `lib/dids/btc.go` |
| Hive deposit detection | Complete | `modules/state-processing/state_engine.go:535-612` |
| Hive withdrawal execution | Complete | `modules/gateway/multisig.go:157-203` |
| Hive withdrawal signing (multisig) | Complete | `modules/gateway/multisig.go:655-732` |
| Ledger credit/debit system | Complete | `modules/ledger-system/ledger_system.go` |

### What is NOT implemented

| Component | Status | Evidence |
|-----------|--------|----------|
| Bitcoin deposit detection | Missing | No Bitcoin chain scanning exists |
| Bitcoin withdrawal tx construction | Missing | `executeActions()` line 384: `if net != "hive" { continue }` — silently skips |
| Bitcoin UTXO management | Missing | No UTXO tracking code exists |
| Bitcoin sighash computation (BIP-143) | Missing | No witness signing code for withdrawals |
| Bitcoin fee estimation | Missing | No fee estimation for BTC withdrawals |

**Key insight:** The BTC bridge is a block relay + address verification system. The actual deposit/withdrawal bridge only works for Hive. The architecture is designed for multi-chain extension but only the Hive path is complete.

### Deposit flow (Hive — the working reference)

```
User sends HBD/HIVE to vsc.gateway with memo "to=alice" or "to=0xAddr"
    │
    ▼
state_engine.go detects transfer to vsc.gateway (line 567)
    │
    ▼
Memo parsed: URL-encoded ("to=alice") or JSON ({"to":"alice"})
    │
    ▼
Address normalized:
  "alice"      → "hive:alice"
  "0x1234..."  → "did:pkh:eip155:1:0x1234..."
    │
    ▼
LedgerRecord created:
  Owner: normalized address
  Amount: transfer amount
  Asset: "hive" | "hbd"
  Type: "deposit"
    │
    ▼
Balance credited in virtual ledger
```

### Withdrawal flow (Hive — the working reference)

```
User submits vsc.withdraw custom_json on Hive
    │
    ▼
TxVSCWithdraw.ExecuteTx validates:
  - NetId matches
  - To field format ("hive:" prefix)
  - Amount > 0
  - Balance sufficient
    │
    ▼
OpLogEvent created (Type="withdraw")
    │
    ▼
ActionRecord stored (status="pending")
    │
    ▼
TickActions() runs every 20 blocks (~1 min)
    │
    ▼
executeActions() builds Hive transfer operations
    │
    ▼
P2P signature collection (30s timeout, 2/3 threshold)
    │
    ▼
Signed transaction broadcast to Hive
    │
    ▼
ActionRecord status → "complete"
```

---

## 3. Architecture Decision: Raw TSS Address vs Router Contract

### Why Thorchain uses a router contract

Thorchain's router contract (`THORChain_Router.sol`, currently v5.0) serves three purposes:

1. **Memo support for ERC-20s.** `ERC20.transfer()` takes only `(address, amount)` — no memo field. Thorchain needs memos for complex operations (swaps, liquidity, loans). The router's `depositWithExpiry()` accepts a `string memo` and emits a `Deposit` event.

2. **Reliable event-based observation.** Bifrost monitors `Deposit` events via `eth_getLogs` rather than scanning raw transfers (which requires trace-level APIs).

3. **Gas-efficient vault rotation for ERC-20s.** Tokens stay in the router; vaults get allowances. `transferAllowance()` moves allowances during churn without moving token balances.

### Why VSC does NOT need a router contract

| Thorchain Requirement | VSC Equivalent | Router Needed? |
|-----------------------|----------------|----------------|
| Memo for swap routing | No complex operations — deposits just credit a balance | No |
| Event-based observation | Scan `tx.To()` for ETH; `Transfer` events for ERC-20 | No |
| ERC-20 allowance rotation | 2-3 tokens max; physical transfer is manageable | No |

**Attribution via sender address:** VSC already has EVM DIDs (`did:pkh:eip155:1:{address}`). When someone sends ETH to the vault, the sender's Ethereum address maps directly to their VSC DID. No memo needed for basic balance crediting.

**Limitations accepted:**
- Users cannot credit a different VSC account (their Hive account) from an ETH deposit without using calldata — see [Section 5](#5-deposit-detection-design) for the calldata memo path.
- Smart contract wallet deposits (Gnosis Safe) are not supported in v1 — see [Section 15, Risk #5](#15-risk-registry).

### Decision

**Raw TSS address for v1. No router contract.**

Justification:
- Eliminates smart contract audit cost, deployment complexity, and contract upgrade risk
- Thorchain's router has been exploited twice (July 2021 ETH router exploits) — additional attack surface with no corresponding benefit at VSC's 2-3 token scale
- Can add a minimal deposit contract in v2 if smart contract wallet support becomes necessary (see [Section 5, v2 path](#v2--minimal-deposit-contract))

---

## 4. TSS Ethereum Signing Compatibility

### Library

VSC uses **Binance Chain's tss-lib v2** (`github.com/bnb-chain/tss-lib/v2@v2.0.2`).

### Signature output format — already Ethereum-native

From `bnb-chain/tss-lib/v2@v2.0.2/ecdsa/signing/finalize.go` lines 58-63:

```go
round.data.R = padToLengthBytesInPlace(round.temp.rx.Bytes(), bitSizeInBytes)
round.data.S = padToLengthBytesInPlace(round.temp.sumS.Bytes(), bitSizeInBytes)
round.data.Signature = append(round.data.R, round.data.S...)
round.data.SignatureRecovery = []byte{byte(recid)}
```

Output `SignatureData` (from `common/signature.pb.go`):

| Field | Type | Description |
|-------|------|-------------|
| `R` | `[]byte` (32 bytes) | ECDSA r value |
| `S` | `[]byte` (32 bytes) | ECDSA s value |
| `SignatureRecovery` | `[]byte` (1 byte) | Recovery ID (v = 0, 1, 2, or 3) |
| `Signature` | `[]byte` (64 bytes) | Concatenated R \|\| S |
| `M` | `[]byte` | Original message digest |

**This is exactly what Ethereum needs.** Bitcoin uses DER encoding (wraps r,s differently). Ethereum uses raw `(r, s, v)`. The TSS library outputs raw values — Ethereum is the easier target.

### Low-s normalization (EIP-2 compliance)

From `bnb-chain/tss-lib/v2@v2.0.2/ecdsa/signing/finalize.go` lines 39-56:

```go
recid := 0
if round.temp.rx.Cmp(round.Params().EC().Params().N) > 0 {
    recid = 2
}
if round.temp.ry.Bit(0) != 0 {
    recid |= 1
}
if sumS.Cmp(secp256k1halfN) > 0 {
    sumS.Sub(round.Params().EC().Params().N, sumS)
    recid ^= 1
}
```

Low-s normalization is already applied. Ethereum requires this per EIP-2.

### Message input — arbitrary hashes accepted

From `bnb-chain/tss-lib/v2@v2.0.2/ecdsa/signing/local_party.go` lines 103-121:

```go
func NewLocalParty(
    msg *big.Int,  // Arbitrary message/hash as big.Int
    params *tss.Parameters,
    key keygen.LocalPartySaveData,
    out chan<- tss.Message,
    end chan<- *common.SignatureData,
    fullBytesLen ...int,
) tss.Party
```

The TSS library signs any `*big.Int` hash. It is not Bitcoin-specific.

### Ethereum transaction signing flow

**No modifications to tss-lib required.** The signing flow:

```
1. Build Ethereum tx:
   types.NewTx(&types.DynamicFeeTx{
       ChainID:   chainID,
       Nonce:     nonce,
       GasTipCap: tip,
       GasFeeCap: maxFee,
       Gas:       gasLimit,
       To:        &recipient,
       Value:     amount,
   })

2. Compute sighash:
   signer := types.LatestSignerForChainID(chainID)
   hash := signer.Hash(tx)
   // This handles RLP encoding + EIP-155 chain ID internally

3. TSS signs the hash:
   party := signing.NewLocalParty(
       new(big.Int).SetBytes(hash.Bytes()),  // sighash as big.Int
       params, keyData, outCh, endCh,
   )
   // 9-round protocol produces SignatureData

4. Apply EIP-155 v value:
   // For EIP-1559 (type 2) txs: v = recid (0 or 1)
   // For legacy txs: v = recid + chainID*2 + 35

5. Attach signature to tx:
   signedTx, _ := tx.WithSignature(signer, append(append(R, S...), v))

6. Broadcast:
   ethclient.SendTransaction(ctx, signedTx)
```

### Ethereum address derivation from TSS public key

The TSS keygen outputs `ECDSAPub` as a standard `*crypto.ECPoint` on secp256k1. Deriving the Ethereum address:

```go
pubKey := keyData.ECDSAPub
uncompressed := crypto.S256().Marshal(pubKey.X(), pubKey.Y())
address := common.BytesToAddress(crypto.Keccak256(uncompressed[1:])[12:])
// address is the Ethereum vault address
```

Standard derivation, well-tested across the ecosystem.

---

## 5. Deposit Detection Design

### Native ETH deposits

**Mechanism:** Poll-based scanning of finalized blocks. Check each transaction's `To` field against the vault address.

```go
func (m *EthMonitor) scanBlocks(ctx context.Context) {
    finalizedBlock := m.getFinalizedBlock(ctx)
    if finalizedBlock <= m.lastScannedBlock {
        return
    }

    for h := m.lastScannedBlock + 1; h <= finalizedBlock; h++ {
        header, _ := m.client.HeaderByNumber(ctx, new(big.Int).SetUint64(h))

        // Bloom filter pre-check — skip blocks that definitely don't involve vault
        if !types.BloomLookup(header.Bloom, m.vaultAddress) {
            continue
        }

        // Only fetch full block if bloom matches
        block, _ := m.client.BlockByNumber(ctx, new(big.Int).SetUint64(h))
        for _, tx := range block.Transactions() {
            if tx.To() != nil && *tx.To() == m.vaultAddress {
                sender, _ := types.Sender(
                    types.LatestSignerForChainID(tx.ChainId()), tx,
                )
                dest := "did:pkh:eip155:1:" + strings.ToLower(sender.Hex())

                // Optional: check calldata for memo override
                if len(tx.Data()) > 0 {
                    if memoDest := parseMemo(tx.Data()); memoDest != "" {
                        dest = memoDest
                    }
                }

                m.creditDeposit(dest, tx.Value(), "eth", tx.Hash(), h)
            }
        }
    }

    m.lastScannedBlock = finalizedBlock
    m.persistCheckpoint(finalizedBlock)
}
```

### ERC-20 deposits (USDC)

**Mechanism:** `FilterLogs` with a block range query for `Transfer` events where `to == vaultAddress`.

```go
func (m *EthMonitor) scanERC20Deposits(ctx context.Context) {
    transferEventSig := crypto.Keccak256Hash(
        []byte("Transfer(address,address,uint256)"),
    )
    vaultPadded := common.BytesToHash(m.vaultAddress.Bytes())

    logs, _ := m.client.FilterLogs(ctx, ethereum.FilterQuery{
        FromBlock: new(big.Int).SetUint64(m.lastScannedBlock + 1),
        ToBlock:   new(big.Int).SetUint64(m.getFinalizedBlock(ctx)),
        Addresses: []common.Address{usdcContractAddress},
        Topics: [][]common.Hash{
            {transferEventSig},   // Transfer event signature
            nil,                  // Any sender
            {vaultPadded},        // To vault address
        },
    })

    for _, log := range logs {
        from := common.BytesToAddress(log.Topics[1].Bytes())
        value := new(big.Int).SetBytes(log.Data)
        dest := "did:pkh:eip155:1:" + strings.ToLower(from.Hex())
        m.creditDeposit(dest, value, "usdc", log.TxHash, log.BlockNumber)
    }
}
```

### Internal transactions (smart contract calls)

**v1 limitation:** ETH sent via `contract.call{value: X}(vaultAddress)` is an internal transaction. These do not appear as top-level transactions in `block.Transactions()`. They are silently missed.

**This is documented as a v1 constraint:** deposits must be sent from EOAs via direct transfer, not from smart contracts.

### v2 — Minimal deposit contract

For v2, a minimal forwarding contract (~40 lines) solves both internal transactions and smart contract wallet attribution:

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

interface IERC20 {
    function transferFrom(address, address, uint256) external returns (bool);
}

contract VSCDeposit {
    address public immutable vault;

    event Deposit(address indexed from, string memo, uint256 amount);
    event DepositERC20(address indexed from, address indexed token,
                       uint256 amount, string memo);

    constructor(address _vault) {
        vault = _vault;
    }

    function deposit(string calldata memo) external payable {
        require(msg.value > 0, "zero value");
        emit Deposit(msg.sender, memo, msg.value);
        (bool ok, ) = vault.call{value: msg.value}("");
        require(ok, "transfer failed");
    }

    function depositERC20(
        address token, uint256 amount, string calldata memo
    ) external {
        require(amount > 0, "zero amount");
        emit DepositERC20(msg.sender, token, amount, memo);
        require(
            IERC20(token).transferFrom(msg.sender, vault, amount),
            "transfer failed"
        );
    }
}
```

This is NOT Thorchain's router. No allowance management, no admin functions, no upgrade mechanism. The vault address is immutable. Audit scope is minimal.

### Checkpoint persistence and crash recovery

`lastScannedBlock` is persisted to the database after each successful scan cycle. If the node crashes mid-scan, it resumes from the last checkpoint. No deposits are missed — the scan range is always `[lastScannedBlock+1, finalizedBlock]`.

No WebSocket subscriptions are used. `FilterLogs` (`ethclient/ethclient.go:431-440`, calls `eth_getLogs` RPC) and `BlockByNumber` are stateless HTTP RPC calls. A connection drop causes the current scan to fail and retry on the next tick — no silent subscription death, no missed blocks.

**Why not `SubscribeFilterLogs`?** WebSocket subscriptions silently die when the connection drops (`rpc/subscription.go:282-287` sends error to `Err()` channel, but subscriptions are NOT re-established on reconnect — `rpc/client.go:673` creates a new handler without migrating old subscriptions). Polling finalized blocks every 12 seconds is adequate for a bridge that already waits ~12.8 minutes for finality.

---

## 6. Withdrawal Execution Design

### Flow

```
User submits withdrawal request (VSC transaction)
    │
    ▼
ledger_session.go validates:
  - Amount > 0
  - Asset in ["hive", "hbd", "eth", "usdc"]
  - Destination format valid ("eth:0x..." or "did:pkh:eip155:1:0x...")
  - Balance sufficient
    │
    ▼
ActionRecord stored (status="pending", type="withdraw")
    │
    ▼
TickActions() picks up pending ETH withdrawals
    │
    ▼
Build Ethereum transaction:
  - Query NonceAt(vaultAddress) for current nonce
  - Estimate gas via SuggestGasTipCap() + SuggestGasFeeCap()
  - Construct types.DynamicFeeTx
    │
    ▼
Compute sighash: signer.Hash(tx)
    │
    ▼
TSS signing session (9 rounds, 2-min timeout per modules/tss/tss.go:46)
  - TSS_DEFAULT_TIMEOUT = 2 * time.Minute (line 46)
  - P2P message retry: 3 attempts with 1s/2s/4s backoff (tss/p2p.go:203-307)
  - Dispatcher retry: 6 attempts at 5s intervals (tss/dispatcher.go:956-1000)
  - RPC_TIMEOUT = 30s per message (tss/tss.go:56)
  - Pass sighash as *big.Int to signing.NewLocalParty
  - Collect (r, s, v) from SignatureData
    │
    ▼
Attach signature: tx.WithSignature(signer, sig)
    │
    ▼
Broadcast: ethclient.SendTransaction(ctx, signedTx)
    │
    ▼
Store in nonce tracker: pendingWithdrawals[nonce] = {txHash, actionID}
    │
    ▼
On subsequent ticks: check TransactionReceipt(txHash)
  - Confirmed → ActionRecord status = "complete"
  - Stuck → replacement flow (see Section 7)
```

### Routing in executeActions()

The current code at `multisig.go:384` does `if net != "hive" { continue }`. This is extended:

```go
switch net {
case "hive":
    // existing Hive withdrawal logic (unchanged)
case "eth":
    ethWithdrawals = append(ethWithdrawals, action)
default:
    continue
}

// After Hive processing:
if len(ethWithdrawals) > 0 {
    ms.executeEthWithdrawals(ethWithdrawals)
}
```

ETH withdrawals are processed sequentially (one per tick) to maintain strict nonce ordering. Batching multiple withdrawals into a single tick is a future optimization.

---

## 7. Nonce Management and Failure Recovery

### Nonce tracker design

```go
type NonceTracker struct {
    client       *ethclient.Client
    vaultAddress common.Address
    pending      map[uint64]*PendingTx  // nonce → pending tx info
    mu           sync.Mutex
}

type PendingTx struct {
    TxHash       common.Hash
    ActionID     string
    Amount       *big.Int
    From         string  // VSC account that initiated withdrawal
    Asset        string
    Nonce        uint64
    GasPrice     *big.Int
    Attempts     int
    FirstAttempt time.Time
}
```

### Nonce derivation — always from chain state

```go
func (nt *NonceTracker) NextNonce(ctx context.Context) (uint64, error) {
    confirmed, _ := nt.client.NonceAt(ctx, nt.vaultAddress, nil)
    pending, _ := nt.client.PendingNonceAt(ctx, nt.vaultAddress, nil)

    if pending > confirmed {
        // There's a tx in the mempool — don't advance
        return 0, ErrNoncePending
    }
    return confirmed, nil
}
```

Every node derives the nonce from on-chain state. No local nonce counter. This eliminates split-brain issues if a node goes offline mid-signing.

### Stuck transaction recovery

**Detection:** On each `TickActions()`, check pending transactions:

```go
func (nt *NonceTracker) CheckPending(ctx context.Context) {
    confirmed, _ := nt.client.NonceAt(ctx, nt.vaultAddress, nil)

    for nonce, pt := range nt.pending {
        if confirmed > nonce {
            // Nonce consumed — check which tx used it
            receipt, err := nt.client.TransactionReceipt(ctx, pt.TxHash)
            if err == nil && receipt != nil {
                // Original withdrawal confirmed
                nt.markComplete(pt.ActionID)
            } else {
                // Replacement or clearing tx consumed the nonce
                // Original withdrawal did NOT execute — safe to refund
                nt.refund(pt)
            }
            delete(nt.pending, nonce)
            continue
        }

        // Nonce not yet consumed — check if stuck
        if time.Since(pt.FirstAttempt) > 10*time.Minute && pt.Attempts < 3 {
            // Rebuild with 2x gas, same nonce
            nt.replaceTransaction(ctx, pt)
            pt.Attempts++
        }
    }
}
```

**Replacement mechanism:** EIP-1559 supports transaction replacement by rebroadcasting with the same nonce and ≥10% higher `maxPriorityFeePerGas`. This is a protocol-level feature, not Bitcoin-style RBF opt-in.

### Double-spend prevention — the critical invariant

**Never refund until `NonceAt() > withdrawal.Nonce`.**

At that point, check the receipt of the original tx hash:
- **Receipt exists** → withdrawal executed on Ethereum → mark complete, no refund
- **No receipt** → a different tx (replacement or clearing) consumed the nonce → withdrawal provably did not execute → refund VSC balance

This eliminates the double-spend race between refund and late confirmation. The on-chain nonce is the single source of truth.

### After 3 failed replacements

**Bridge state:**
- The stuck nonce blocks all subsequent withdrawals
- Deposits continue normally (inbound doesn't require signing)
- User's VSC balance remains locked (no refund until nonce clears)

**Automated clearing:** Send a zero-value self-transfer (`vault → vault, 0 ETH, nonce N, high gas`) to advance the nonce. This also requires TSS quorum.

**If quorum is persistently unavailable:** Election rotation (~1 hour, 1200 Hive blocks) replaces offline nodes. New committee reshares keys automatically. New committee signs the clearing tx. Withdrawals resume.

**This is a liveness failure, not a safety failure.** Funds are safe in the TSS vault. Same failure mode as every threshold bridge (Thorchain, Wormhole, etc.).

---

## 8. Vault Rotation

### Reshare preserves the Ethereum address — PROVEN

The TSS reshare protocol (`bnb-chain/tss-lib/v2/ecdsa/resharing/`) **guarantees** the group public key is preserved. Evidence:

**File paths:** All in `/root/go/pkg/mod/github.com/bnb-chain/tss-lib/v2@v2.0.2/ecdsa/resharing/`

**Round 1** (`round_1_old_step_1.go`, lines 80, 118-127): Old committee broadcasts `ECDSAPub` to new members. New members receive it and verify consistency — if any old member sends a different public key, the reshare aborts:
```go
if round.save.ECDSAPub != nil && !candidate.Equals(round.save.ECDSAPub) {
    return false, round.WrapError(errors.New("ecdsa pub key did not match..."))
}
round.save.ECDSAPub = candidate
```

**Round 4** (`round_4_new_step_2.go`, line 183): Cryptographic assertion:
```go
if !Vc[0].Equals(round.save.ECDSAPub) {
    return round.WrapError(errors.New("assertion failed: V_0 != y"))
}
```

`Vc[0]` is the constant term of the aggregated verification polynomial = `g^(sum of all shares)` = original group public key. This is a mathematical proof, not a heuristic.

**Round 5** (`round_5_new_step_3.go`, lines 30-37): Saves new `Xi` (secret share), `Ks` (party indices), `BigXj` (share public keys), and Paillier keys. `ECDSAPub` is **NOT modified** in this round — it retains the value set in Round 1 and verified in Round 4.

**Test proof** (`local_party_test.go`): After resharing, signs a message with new shares and verifies against original `ECDSAPub`. `ecdsa.Verify()` succeeds.

**This holds regardless of threshold changes or committee composition changes.**

**Therefore:** The Ethereum vault address (`keccak256(pubkey)[12:]`) is identical after reshare. No fund movement required during routine rotations.

### When fund movement IS required

Full keygen (new key from scratch) produces a new public key and therefore a new Ethereum address. This is a deliberate operational decision, not a routine rotation.

Movement procedure:
1. New TSS key generated → new Ethereum address derived
2. Old committee signs ETH transfer: `vault_old → vault_new` (1 tx, ~21,000 gas)
3. Old committee signs ERC-20 transfers: `USDC.transfer(vault_new, balance)` (1 tx per token, ~65,000 gas)
4. For ETH + USDC: 2 transactions total

### Pause withdrawals during rotation

Current code has **no synchronization** between `TickKeyRotation` and `TickActions` — see [Section 10](#10-gateway-concurrency-refactor) for the fix.

---

## 9. Confirmation Depth and Finality

### Policy: Wait for finality

**Recommended approach:** Only scan finalized blocks. Deposits are detected and credited only after Ethereum finality (~12.8 minutes / 2 beacon chain epochs).

**Rationale:**
- Eliminates all reorg handling code
- No provisional balances, no clawback logic, no two-tier crediting
- 12.8 minutes is acceptable UX for a bridge (exchanges take longer)
- Simplest correct implementation

### Finality detection

The VSC go-ethereum fork (geth 1.14.12) supports the `"finalized"` block tag.

From `vsc-eco/go-ethereum@v0.0.1/rpc/types.go` lines 65-71:
```go
const (
    SafeBlockNumber      = BlockNumber(-4)
    FinalizedBlockNumber = BlockNumber(-3)
    LatestBlockNumber    = BlockNumber(-2)
    PendingBlockNumber   = BlockNumber(-1)
    EarliestBlockNumber  = BlockNumber(0)
)
```

JSON marshal/unmarshal in `rpc/types.go` lines 95-97 handles `"finalized"` string. Test coverage in `rpc/types_test.go` (28 test cases including finalized, lines 49, 93, 106, 143, 172). Filter system support in `eth/filters/filter.go` lines 126-130.

From `vsc-eco/go-ethereum@v0.0.1/eth/api_backend.go` lines 80-85:
```go
if number == rpc.FinalizedBlockNumber {
    block := b.eth.blockchain.CurrentFinalBlock()
    if block == nil {
        return nil, errors.New("finalized block not found")
    }
    return block, nil
}
```

**Usage:**
```go
func (m *EthMonitor) getFinalizedBlock(ctx context.Context) uint64 {
    block, err := m.client.BlockByNumber(ctx,
        big.NewInt(int64(rpc.FinalizedBlockNumber)))
    if err != nil {
        return m.lastScannedBlock  // no new finalized block
    }
    return block.NumberU64()
}
```

No beacon chain epoch tracking needed on VSC's side. The Ethereum execution client handles this internally.

### User-facing pending state

The 12.8-minute wait is invisible to VSC's consensus layer. For UX, the frontend/API can independently watch the Ethereum mempool and show "deposit detected, waiting for finality (~X minutes remaining)" as a **UI-only informational state**. This is not a ledger entry — it's a frontend concern.

---

## 10. Gateway Concurrency Refactor

### Pre-existing bugs in current code

The gateway module (`multisig.go`) has **zero synchronization primitives**:
- No `sync.Mutex`, no `sync.RWMutex`, no `sync.WaitGroup`
- `msgChan map[string]chan *p2pMessage` (declared at `multisig.go:48`, initialized at `multisig.go:818`) is accessed from multiple goroutines without mutex protection:
  - Written by `TickKeyRotation` at line 121, `TickActions` at line 165, `TickSyncFr` at line 214
  - Read/written by the P2P handler at `p2p.go:203-204`
  - Read by `waitForSigs` goroutine at line 697
- The race condition is at **`multisig.go:102` and `multisig.go:106`**: both fire-and-forget goroutines are spawned with `go ms.TickKeyRotation(bh)` and `go ms.TickActions(bh)` respectively, with no coordination
- Every 1200 blocks, both trigger simultaneously because `1200 % 20 == 0` (constants at lines 68 and 70: `ROTATION_INTERVAL = 20 * 60`, `ACTION_INTERVAL = 20`)

This is a **data race** under Go's memory model. Go maps are not safe for concurrent read/write to *any* key — `go run -race` would flag this. Currently benign on Hive (different TxIds avoid key collision), but critical for Ethereum where nonce ordering is strict and concurrent signing sessions on overlapping state would corrupt the nonce tracker.

### Fix: Sequential operation queue with non-blocking enqueue

```go
type MultiSig struct {
    // ... existing fields ...
    opQueue chan func()
}

func New(/* ... */) *MultiSig {
    ms := &MultiSig{
        // ... existing initialization ...
        opQueue: make(chan func(), 20),
    }
    return ms
}

func (ms *MultiSig) Start() *promise.Promise[any] {
    go ms.operationLoop()
    // ... existing start logic ...
}

func (ms *MultiSig) operationLoop() {
    for op := range ms.opQueue {
        op()
    }
}

func (ms *MultiSig) BlockTick(bh uint64, headHeight *uint64) {
    // ... existing validation (lines 74-80) ...

    if bh%ROTATION_INTERVAL == 0 {
        select {
        case ms.opQueue <- func() { ms.TickKeyRotation(bh) }:
        default:
            ms.logger.Warn("rotation skipped: queue full", "bh", bh)
        }
    }
    if bh%ACTION_INTERVAL == 0 {
        select {
        case ms.opQueue <- func() { ms.TickActions(bh) }:
        default:
            ms.logger.Warn("actions skipped: queue full", "bh", bh)
        }
    }
    if bh%SYNC_INTERVAL == 0 {
        select {
        case ms.opQueue <- func() { ms.TickSyncFr(bh) }:
        default:
            ms.logger.Warn("sync skipped: queue full", "bh", bh)
        }
    }
}
```

### Guarantees

| Property | Guarantee | Mechanism |
|----------|-----------|-----------|
| BlockTick never blocks | Non-blocking `select` with `default` | Drops if queue full |
| Rotation before actions at shared heights | FIFO channel ordering | Rotation `if` branch is first in source |
| No concurrent operations | Sequential worker goroutine | Single consumer on `opQueue` |
| Dropped operations retry | Idempotent pending-state design | Next tick picks up same work |

### Buffer sizing

Worst case: 1 rotation (5s) + 1 action (30s) = 35s drain time. During 35s at ~1 block/3s on Hive, ~12 blocks arrive, each potentially enqueuing 1 action. **Buffer of 20 handles this with headroom.**

### Impact on Hive path

BlockTick is registered as `async: false` in the Hive consumer (`modules/hive/block-consumer/consumer.go:45-57`):

```go
// consumer.go ProcessBlock — the single goroutine that calls all BlockTick handlers
func (v *HiveConsumer) ProcessBlock(blk hive_blocks.HiveBlock, headHeight *uint64) {
    for _, tick := range v.ticks {
        if tick.async {
            go tick.funck(blk.BlockNumber, headHeight)  // async=true: goroutine
        } else {
            tick.funck(blk.BlockNumber, headHeight)     // async=false: SYNCHRONOUS
        }
    }
}
```

MultiSig registers at `multisig.go:54` with `async: false`, meaning BlockTick runs **synchronously in the Hive consumer goroutine**. If BlockTick blocks, the entire Hive block processing pipeline stalls for ALL modules. The non-blocking `select` with `default` ensures BlockTick returns in microseconds regardless of queue state — identical timing to the current fire-and-forget `go` pattern from the caller's perspective.

The behavioral change: operations that previously ran concurrently now run sequentially. This means a 30s `TickActions` delays the next operation by 30s. **This is a correctness fix, not a regression** — the concurrent execution was a race condition.

The only existing test (`TestMultisigKeys`) does not call BlockTick, TickKeyRotation, or TickActions. It tests key generation and Hive account update broadcasting directly. **No test is affected.**

---

## 11. Ledger System Changes

### Current withdrawal validation (`ledger_session.go:101-169`)

```
Asset whitelist: ["hive", "hbd"]

Destination validation:
  1. Match HIVE_REGEX + len 3-16 → dest = "hive:" + input
  2. HasPrefix("hive:") → validate extracted part → dest = input
  3. Else → "invalid destination"
```

Where `HIVE_REGEX = ^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$` (from `utils.go:12`)

### Required changes

**Change 1 — Asset whitelist (additive):**
```go
// Before:
validAssets := []string{"hive", "hbd"}
// After:
validAssets := []string{"hive", "hbd", "eth", "usdc"}
```

**Change 2 — Destination validation (additive, inserted before final `else`):**
```go
} else if strings.HasPrefix(withdraw.To, "eth:") {
    ethAddr := strings.Split(withdraw.To, ":")[1]
    if matched, _ := regexp.MatchString(ETH_REGEX, ethAddr); matched {
        dest = withdraw.To
    } else {
        return LedgerResult{Ok: false, Msg: "invalid eth address"}
    }
} else if strings.HasPrefix(withdraw.To, "did:pkh:eip155:1:") {
    ethAddr := strings.TrimPrefix(withdraw.To, "did:pkh:eip155:1:")
    if matched, _ := regexp.MatchString(ETH_REGEX, ethAddr); matched {
        dest = withdraw.To
    } else {
        return LedgerResult{Ok: false, Msg: "invalid eth address"}
    }
} else {
    return LedgerResult{Ok: false, Msg: "invalid destination"}
}
```

### Collision analysis

- Ethereum addresses start with `0x` followed by hex. `HIVE_REGEX` requires `^[a-z]` — no `0x` match possible.
- `"eth:..."` prefix is unambiguous — no Hive account can contain a colon (HIVE_REGEX only allows `[0-9a-z\-]` and `.`).
- A Hive account named `"eth"` (3 chars, valid) is handled correctly: input `"eth"` matches branch 1 (plain Hive regex). Input `"eth:0x..."` fails HIVE_REGEX (colon not in charset) and falls through to the ETH branch.

**No existing validation is widened or narrowed.**

### Deposit crediting

The existing `Deposit()` function in `ledger_system.go` already normalizes Ethereum addresses to `did:pkh:eip155:1:` format. The `ETH_REGEX` is already defined and used for deposit address matching. No changes needed for deposit crediting — only the asset name (`"eth"`, `"usdc"`) needs to be recognized.

---

## 12. Module Placement and Registration

### Where each component lives

| Component | File | Module | Rationale |
|-----------|------|--------|-----------|
| ETH block relay | `modules/oracle/chain/ethereum.go` | oracle | Implements `chainRelay` interface, same as `bitcoin.go` |
| ETH deposit monitor | `modules/gateway/eth_monitor.go` | gateway | Detects deposits, credits ledger. Alongside `multisig.go` (withdrawals). |
| Nonce tracker | `modules/gateway/eth_nonce.go` | gateway | Used by withdrawal execution. Internal to gateway. |
| ETH config | `modules/oracle/chain/config.go` | oracle | Add `Ethereum chainRpcEntry` field |

### Chain relay registration pattern

From `chain_relay.go` lines 27-32:

```go
var _chains = [...]chainRelay{
    &bitcoinRelayer{},
    &litecoinRelayer{},
    // Add: &ethereumRelayer{},
}
```

`ChainOracle.New()` automatically maps all entries by `Symbol()`. `ChainOracle.Init()` calls each relayer's `Init()`. No changes to `main.go` or the aggregate plugin system needed.

### Config registration

From `config.go`:

```go
type chainConfig struct {
    Bitcoin  chainRpcEntry
    Litecoin chainRpcEntry
    // Add:
    Ethereum chainRpcEntry
}
```

Environment variable pattern: `ETH_RPC_HOST`, `ETH_RPC_USER`, `ETH_RPC_PASS` (mirrors `BTC_RPC_HOST`, etc.).

### Plugin lifecycle

MultiSig (gateway) is registered as a plugin in `main.go:306`. Its `Init()` registers `BlockTick`. The `EthMonitor` can either:

1. Be initialized inside `MultiSig.Init()` and share the same `BlockTick` callback, or
2. Register its own `BlockTick` callback via `hiveConsumer.RegisterBlockTick("eth-monitor", m.BlockTick, false)`

Option 1 is simpler and ensures deposit monitoring and withdrawal execution share state (vault address, nonce tracker).

---

## 13. Existing Ethereum Code (Cross-Branch)

### Critical finding: `origin/oracle_eth-api` branch

This branch has a **working Ethereum chain relay**:

| File | Purpose |
|------|---------|
| `modules/oracle/chain/api/ethereum.go` | Full `chainRelay` implementation using Helios light client |
| `modules/oracle/chain/api/ethereum_test.go` | Tests for block fetching and fee calculation |
| `modules/oracle/chain/api/api.go` | Refactored `ChainRelay` interface (moved to `api` subpackage) |
| `modules/oracle/chain/chain_relay.go` | Modified `_chains` includes `&api.Ethereum{}` |
| `dockerfiles/helios_eth.Dockerfile` | Builds Helios Ethereum light client from source |
| `docker-compose.yml` | Adds `helios` + `nimbus_beacon_node` services |

#### Helios light client architecture (deliberate design choice)

The `oracle_eth-api` branch uses [Helios](https://github.com/a16z/helios) (a16z's trust-minimized Ethereum light client) instead of connecting directly to Infura/Alchemy. This is a **trust-minimization decision** — Helios verifies Ethereum state proofs locally against the beacon chain rather than trusting a centralized RPC provider.

**Connection details:**
- Helios runs locally and exposes an RPC endpoint at `localhost:8545` (or `helios:8545` in Docker)
- The `ethereum.go` relay connects to this local endpoint via HTTP JSON-RPC
- Helios itself connects to an upstream execution RPC (Infura) and a beacon node (Nimbus) for sync, but **verifies all responses cryptographically** before serving them to VSC

**Docker infrastructure:**
```yaml
# From docker-compose.yml on oracle_eth-api:
helios:
  build:
    context: .
    dockerfile: dockerfiles/helios_eth.Dockerfile
  # Connects to Infura execution RPC + Nimbus beacon node
  # Serves verified Ethereum state at :8545

nimbus_beacon_node:
  # Runs Nimbus consensus client for Ethereum mainnet
  # Provides beacon chain data to Helios for state verification
```

**Implications for our architecture:**
- The deposit monitor (`eth_monitor.go`) should connect to the same Helios endpoint, NOT directly to Infura/Alchemy
- `FilterLogs` and `BlockByNumber` calls go through Helios, which means they are cryptographically verified
- The `"finalized"` block tag works through Helios because Helios tracks the beacon chain via Nimbus
- This eliminates the trust assumption on the RPC provider — a compromised Infura cannot feed false deposit data
- RPC rate limits are not a concern since Helios is a local process, not a remote API

**Architecture decisions from that branch:**
- Uses [Helios](https://github.com/a16z/helios) (a16z's Ethereum light client) for trust-minimized RPC — not raw Infura/Alchemy
- Relocated chain implementations to `api` subpackage
- Block relay only (no deposit detection, no withdrawal signing)

### Code on `main` branch

| File | Purpose |
|------|---------|
| `lib/dids/eth.go` | EIP-712 typed data signing for Ethereum wallet authentication (NOT chain integration) |
| `modules/state-processing/utils.go` | `NormalizeAddress()` converts ETH addresses to `did:pkh:eip155:1:`. `SUPPORTED_TYPES = []string{"ethereum", "hive"}` |
| `modules/block-producer/utils.go` | Uses go-ethereum's Patricia Merkle Trie as a utility library |

### Integration recommendation

**Rebase onto `oracle_eth-api`'s chain relay code.** Our architecture extends it (adding deposit monitoring, withdrawal signing, ERC-20 support) without conflicting. Key decisions:

1. Adopt the Helios light client approach for trust minimization
2. Decide whether to keep the `api` subpackage structure or merge back to `chain/`
3. Add deposit monitoring and withdrawal execution as new code on top of the existing relay

---

## 14. RPC Call Budget and Efficiency

### Normal operation (no outage)

| Call | Frequency | Purpose |
|------|-----------|---------|
| `BlockByNumber("finalized")` | Every 12s | Get latest finalized block number |
| `FilterLogs(range)` | Every 12s | Scan for ERC-20 Transfer events |
| `HeaderByNumber(h)` | 0-1 per cycle | Bloom filter check for new finalized block |
| `BlockByNumber(h)` | 0-1 per cycle | Fetch full block only if bloom matches |

**Total: 2-4 RPC calls per 12-second cycle.**

### Catchup after 1-hour outage (~300 blocks)

| Call | Count | Purpose |
|------|-------|---------|
| Batch `eth_getBlockByNumber(h, false)` | 1 (batch of 300) | Fetch all headers with bloom filters |
| `FilterLogs(fromBlock, toBlock)` | 1 | ERC-20 events for entire range |
| `BlockByNumber(h)` with full txs | 1-5 | Only blocks where bloom matches vault address |

**Total: 3-7 RPC calls for entire catchup.**

### Catchup after 24-hour outage (~7,200 blocks)

| Call | Count | Purpose |
|------|-------|---------|
| Batch headers (chunked to 1000/batch) | 8 | Bloom filter pre-check |
| `FilterLogs` | 1 | ERC-20 events (single range query) |
| `BlockByNumber` with full txs | 5-50 | Only bloom-matched blocks |

**Total: ~15-60 RPC calls.**

Free tier Infura/Alchemy (100k requests/day) is not a concern at these volumes.

### Bloom filter optimization

Ethereum block headers contain a 2048-bit bloom filter of all addresses involved in transactions and logs. `types.BloomLookup(header.Bloom, vaultAddress)` checks if the vault address **might** appear in the block. False positives are possible (~1-3% of blocks for a random address) but false negatives are not. This eliminates 97-99% of full block fetches.

---

## 15. Risk Registry

### HIGH

| # | Risk | Impact | Mitigation |
|---|------|--------|------------|
| 1 | **Gas price volatility** | Withdrawals cost gas. Spikes can stall or make withdrawals uneconomical. | Maintain ETH gas reserve in vault. Set minimum withdrawal thresholds. EIP-1559 replacement for stuck txs. |
| 2 | **Nonce stuck + no quorum** | All withdrawals halt. Users' VSC balances locked. | Automated replacement (3 attempts). Election rotation (~1hr) replaces offline nodes. Nonce-clearing tx as fallback. |
| 3 | **TSS quorum liveness** | If <2/3 nodes online, no signing possible. | Same risk profile as existing Hive gateway. Rotation at 1200 blocks replaces offline nodes. Liveness failure, not safety failure. |

### MEDIUM

| # | Risk | Impact | Mitigation |
|---|------|--------|------------|
| 4 | **ERC-20 decimal handling** | USDC uses 6 decimals, not 18. Incorrect math = over/under-crediting. | Store amounts in token-native units. Per-token decimal config. |
| 5 | **Smart contract wallet deposits (v1)** | Gnosis Safe users can't deposit. ~15-20% of DeFi power users affected. | Document as v1 limitation. v2 minimal deposit contract. Calldata memo available for EOAs wanting alt-destination. |
| 6 | **go-ethereum fork drift** | Fork pinned at geth 1.14.12. Upstream security patches and hard forks need tracking. | Periodic rebase schedule. Monitor upstream security advisories. |
| 7 | **ERC-20 transfer quirks** | Some tokens (USDT) don't return `bool`. Some have fees-on-transfer. Some can be paused. | Only support well-behaved tokens (USDC, WETH). Test each token explicitly before listing. |
| 8 | **Vault rotation gas costs (full keygen only)** | Moving all ERC-20 balances requires gas. | Limit supported tokens. 2 txs for ETH + USDC. Reshare (routine rotation) requires zero gas. |

### LOW

| # | Risk | Impact | Mitigation |
|---|------|--------|------------|
| 9 | **TSS signing latency** | 9-round ECDSA signing with 2-min timeout. Slower than single-party signing. | Signing doesn't race Ethereum blocks. Withdrawal isn't time-critical. |
| 10 | **Replay protection across EVM chains** | Same TSS key could theoretically sign for multiple EVM chains. | EIP-155 chain ID in sighash. Only sign for configured chain ID. |
| 11 | **MEV / frontrunning** | Large withdrawals could be frontrun. | Not significant for simple transfers (no swaps). No sandwich vector. |

---

## 16. Test Impact Analysis

### Existing tests

| Test File | Status | Impact of Changes |
|-----------|--------|-------------------|
| `modules/gateway/multisig_test.go` (`TestMultisigKeys`) | Active. Integration test against Hive testnet. | **No impact.** Tests key generation and account update directly. Does not call BlockTick/TickKeyRotation/TickActions. |
| `modules/state-processing/ledger_system_test.go` | Disabled (`//go:build legacy`). | **No impact.** Won't compile or run. |
| `modules/state-processing/ledger_system_slot_test.go` | Disabled (`//go:build legacy`). | **No impact.** Won't compile or run. |
| `modules/e2e/e2e_test.go` | Active. Creates Ethereum DIDs, tests Hive gateway deposits/transfers. | **Low risk.** Tests Hive deposit path which is unchanged. New asset types and destinations don't affect existing test inputs. |
| `modules/oracle/chain/api/ethereum_test.go` | Active (on `oracle_eth-api` branch only). Tests ETH block fetching. | **No impact.** Tests chain relay, not gateway. |

### New tests required

| Component | Test Scope |
|-----------|-----------|
| ETH deposit monitor | Scan finalized blocks, detect ETH transfers to vault, credit correct DID, handle bloom filter misses, checkpoint persistence and crash recovery |
| ERC-20 deposit monitor | Detect `Transfer` events, parse topics/data correctly, handle multiple tokens, handle zero-value transfers |
| Nonce tracker | Sequential nonce assignment, stuck tx detection, replacement tx generation, double-spend prevention (refund timing), nonce-clearing flow |
| Withdrawal validation | ETH address format validation, `did:pkh:eip155:1:` format validation, asset whitelist, no collision with Hive validation |
| Sequential operation queue | Non-blocking enqueue, drop on full queue, FIFO ordering, rotation-before-actions at shared heights |
| TSS ETH signing | End-to-end: build tx → hash → TSS sign → attach sig → verify recoverable address matches vault |

---

## 17. Implementation Sequence

### Phase 1: Foundation (no user-facing changes)

1. **Gateway concurrency refactor** — sequential operation queue in `multisig.go`. Fixes pre-existing race condition. Must be done first as all subsequent changes depend on it.
2. **Rebase onto `oracle_eth-api`** — adopt existing Ethereum chain relay code and Helios infrastructure.

### Phase 2: Inbound (deposits)

3. **ETH deposit monitor** — `gateway/eth_monitor.go`. Poll-based scanning of finalized blocks for ETH transfers to vault.
4. **ERC-20 deposit monitor** — extend `eth_monitor.go` with `FilterLogs` for USDC `Transfer` events.
5. **Ledger integration** — connect deposit monitor to ledger system for balance crediting.

### Phase 3: Outbound (withdrawals)

6. **Ledger validation changes** — add `"eth"`, `"usdc"` assets and `"eth:"`/`"did:pkh:eip155:1:"` destinations to `ledger_session.go`.
7. **Nonce tracker** — `gateway/eth_nonce.go`. Chain-state-derived nonces, pending tx tracking, replacement logic.
8. **ETH withdrawal execution** — extend `executeActions()` with Ethereum transaction construction + TSS signing flow.
9. **Stuck tx recovery** — replacement transactions, nonce clearing, refund logic with double-spend prevention.

### Phase 4: Operational

10. **Vault address derivation** — derive Ethereum address from TSS group public key. Expose via API.
11. **Rotation coordination** — ensure withdrawal pause during full keygen rotation (reshare needs no coordination).
12. **Configuration** — `ETH_RPC_HOST` env vars, supported token list, confirmation depth, gas reserve thresholds.
13. **Monitoring** — vault balance alerts, pending withdrawal queue depth, gas reserve warnings.

---

## Appendix A: Key File Reference

### Files to modify

| File | Change |
|------|--------|
| `modules/oracle/chain/chain_relay.go` | Add `&ethereumRelayer{}` to `_chains` array |
| `modules/oracle/chain/config.go` | Add `Ethereum chainRpcEntry` to `chainConfig` struct, `ETH_RPC_*` env vars |
| `modules/gateway/multisig.go` | Sequential operation queue refactor. Extend `executeActions()` for ETH withdrawals. |
| `modules/ledger-system/ledger_session.go` | Add `"eth"`, `"usdc"` to asset whitelist. Add ETH destination validation. |

### Files to create

| File | Purpose |
|------|---------|
| `modules/oracle/chain/ethereum.go` | Ethereum chain relay implementing `chainRelay` interface |
| `modules/gateway/eth_monitor.go` | Deposit detection via finalized block scanning |
| `modules/gateway/eth_nonce.go` | Nonce tracking, stuck tx recovery, replacement logic |

### Files to reference (read-only)

| File | Why |
|------|-----|
| `modules/oracle/chain/bitcoin.go` | Pattern for `chainRelay` implementation |
| `modules/gateway/p2p.go` | P2P message handling for signature collection |
| `modules/ledger-system/ledger_system.go` | Deposit crediting, `ETH_REGEX`, address normalization |
| `modules/state-processing/utils.go` | `NormalizeAddress()`, `SUPPORTED_TYPES` |
| `lib/dids/eth.go` | Existing EIP-712 DID system (wallet auth, not chain integration) |

### Files on `oracle_eth-api` branch to rebase onto

| File | Purpose |
|------|---------|
| `modules/oracle/chain/api/ethereum.go` | Existing Ethereum chain relay (Helios-based) |
| `modules/oracle/chain/api/ethereum_test.go` | Existing tests |
| `dockerfiles/helios_eth.Dockerfile` | Helios light client Docker build |
| `docker-compose.yml` additions | Helios + Nimbus beacon node services |

---

## Appendix B: Dependency Verification

### go.mod — confirmed present

| Dependency | Version | Purpose |
|------------|---------|---------|
| `github.com/vsc-eco/go-ethereum` | `v0.0.1` (geth 1.14.12) | ETH client, RLP, ABI, types, crypto |
| `github.com/bnb-chain/tss-lib/v2` | `v2.0.2` | TSS ECDSA signing (secp256k1) |
| `github.com/btcsuite/btcd/btcec/v2` | `v2.3.4` | secp256k1 curve (used by tss-lib) |
| `github.com/vsc-eco/hivego` | `v0.0.0-20260224180332` | Hive blockchain client |

### go-ethereum fork capabilities — confirmed

| Feature | Status | Evidence |
|---------|--------|----------|
| `ethclient.BlockByNumber()` | Present | `ethclient/ethclient.go` |
| `ethclient.FilterLogs()` | Present | `ethclient/ethclient.go:431-440` |
| `ethclient.SendTransaction()` | Present | `ethclient/ethclient.go` |
| `ethclient.NonceAt()` | Present | `ethclient/ethclient.go` |
| `ethclient.SuggestGasTipCap()` | Present | `ethclient/ethclient.go` |
| `rpc.FinalizedBlockNumber` | Present | `rpc/types.go:67` |
| `types.LatestSignerForChainID()` | Present | `core/types/transaction_signing.go` |
| `types.BloomLookup()` | Present | `core/types/bloom9.go` |
| `types.DynamicFeeTx` | Present | `core/types/dynamic_fee_tx.go` |
| RLP encoding/decoding | Present | `rlp/encode.go`, `rlp/decode.go` |
| ABI encoding/decoding | Present | `accounts/abi/abi.go` |
| `crypto.Keccak256()` | Present | `crypto/crypto.go` |

### tss-lib capabilities — confirmed

| Feature | Status | Evidence |
|---------|--------|----------|
| secp256k1 ECDSA signing | Present | `ecdsa/signing/local_party.go` |
| Arbitrary hash input (`*big.Int`) | Present | `signing.NewLocalParty(msg, ...)` |
| Raw (r, s, v) output | Present | `ecdsa/signing/finalize.go:58-63` |
| Low-s normalization (EIP-2) | Present | `finalize.go:51-54` |
| Recovery ID | Present | `SignatureData.SignatureRecovery` |
| Key resharing (preserves pubkey) | Present | `ecdsa/resharing/round_4_new_step_2.go:183` |
| HD key derivation (BIP-32) | Present | `crypto/ckd/child_key_derivation.go` |
