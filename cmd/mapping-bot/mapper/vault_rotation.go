package mapper

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/btcvault"
)

// ---------------------------------------------------------------------------
// BTC vault-rotation-v2 migration driver.
//
// The contract is PULL-driven: it only BUILDS a migration sweep when someone
// calls migrateVault, and getMigrationInputs deliberately returns at most
// MaxMigrationInputs UTXOs per call ("the caller drains it in successive
// tranches"). Nothing in the bot drove that loop, so a rotated-out generation
// kept most of its BTC forever: the sweep never got built, the generation never
// drained, retireVault could never advance it (the fund-gate correctly refuses a
// still-funded generation), and the committee's bond stayed locked.
//
// Broadcasting and settling are already generic over pending spends
// (HandleUnmap → HandleConfirmations → confirmSpend), so a migration sweep rides
// that existing pipeline for free once it exists. This file supplies only the
// missing DRIVING side: build the next tranche, fall back to writeOffDust when a
// residual is uneconomic to sweep, and advance the lifecycle once drained.
// ---------------------------------------------------------------------------

// vaultOpMinInterval bounds how often the driver may issue a vault op. Every op
// costs RC, and both the "waiting for the purge grace" and "fee reserve is
// empty" states would otherwise re-issue a doomed call on every block.
const vaultOpMinInterval = 2 * time.Minute

// redriveStaleBlocks mirrors the contract's RedriveStaleBlocks (the L1-block age at
// which a never-confirming sweep may be re-driven). The contract enforces its own gate;
// the driver only avoids issuing a redrive it knows the contract will refuse. Kept one
// block ABOVE the contract value so the driver never races the contract's own boundary.
const redriveStaleBlocks = 13

// firstSeenSweep tracks the contract height at which the driver first observed each
// in-flight sweep txid, so it can measure staleness the same way the contract does
// (contract LastHeight minus the sweep's build height). Process-global, mutex-guarded.
var (
	sweepSeenMu    sync.Mutex
	firstSeenSweep = map[string]uint64{}
)

// noteSweepsInFlight records first-seen heights for the currently in-flight sweeps and
// forgets any that have settled (no longer in flight), then returns the txids that are
// now stale enough to re-drive (in flight since >= redriveStaleBlocks ago).
func noteSweepsInFlight(inFlight []string, height uint64) []string {
	sweepSeenMu.Lock()
	defer sweepSeenMu.Unlock()

	live := make(map[string]struct{}, len(inFlight))
	var stale []string
	for _, txId := range inFlight {
		live[txId] = struct{}{}
		seen, ok := firstSeenSweep[txId]
		if !ok {
			firstSeenSweep[txId] = height
			continue
		}
		if height >= seen && height-seen >= redriveStaleBlocks {
			stale = append(stale, txId)
		}
	}
	// Forget settled sweeps so a recycled txid can't inherit a stale first-seen.
	for txId := range firstSeenSweep {
		if _, ok := live[txId]; !ok {
			delete(firstSeenSweep, txId)
		}
	}
	return stale
}

// vaultOpRcLimit is the RC limit attached to the vault ops specifically.
//
// The bot ships one global RcLimit for every L2 tx (default 10_000). Vault ops are
// an order of magnitude heavier than the bot's ordinary calls — SPV verification +
// secp256k1 + per-generation P2WSH derivation — and the devnet harness has to raise
// rc_limit to ~8M for them ("map/SPV + secp256k1 + per-generation address derivation
// is gas-heavy and blows the 500k default"). At the default limit every migrateVault
// would abort with a cost-limit-exceeded and the rotation would never move.
//
// rc_limit is a CEILING, not a charge — the tx is billed for the gas it actually
// uses — so raising it for these ops costs nothing when they are cheap. It is scoped
// to the vault ops rather than raised globally on purpose: a high rc_limit reserves
// HBD against RC on the caller, which surfaces as a spurious insufficient-balance on
// ops that DO move HBD. The vault ops move none.
const vaultOpRcLimit uint = 8_000_000

// rcLimitFor returns the RC limit to attach to an action, raising it for the vault
// ops while leaving every existing action on the operator's configured value.
func (b *Bot) rcLimitFor(action string) uint {
	configured := b.BotConfig.RcLimit()
	switch action {
	case "migrateVault", "retireVault", "writeOffDust", "redriveSpend":
		if configured > vaultOpRcLimit {
			return configured
		}
		return vaultOpRcLimit
	default:
		return configured
	}
}

// All driver state is process-global and touched from the block-loop goroutine,
// so it is mutex-guarded rather than assumed single-threaded.
var (
	vaultOpMu        sync.Mutex
	lastVaultOpAt    = map[string]time.Time{}
	feeReserveWarned = map[string]bool{}
	// notOwner latches per contract: the vault ops are owner-only, so if this bot's
	// identity is not the owner, NO retry will ever succeed. Keep issuing them and we
	// just burn RC on guaranteed rejections.
	notOwner = map[string]bool{}
)

// markNotOwner latches the not-owner condition and reports whether this is the first
// time we have seen it (so the operator gets one loud message, not one per block).
func markNotOwner(contractId string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	if notOwner[contractId] {
		return false
	}
	notOwner[contractId] = true
	return true
}

func isNotOwnerLatched(contractId string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	return notOwner[contractId]
}

// mayIssueVaultOp rate-limits vault ops per contract.
func mayIssueVaultOp(contractId string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	if last, ok := lastVaultOpAt[contractId]; ok && time.Since(last) < vaultOpMinInterval {
		return false
	}
	lastVaultOpAt[contractId] = time.Now()
	return true
}

// warnFeeReserveOnce returns true the first time it is called for a contract
// after a successful migrate, so a stuck fee reserve is reported loudly but not
// on every single cycle.
func warnFeeReserveOnce(contractId string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	if feeReserveWarned[contractId] {
		return false
	}
	feeReserveWarned[contractId] = true
	return true
}

// clearFeeReserveWarning re-arms the warning once a migrate succeeds again.
func clearFeeReserveWarning(contractId string) {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	delete(feeReserveWarned, contractId)
}

// isUneconomicResidual reports whether migrateVault refused because what's left
// of the retiring generation cannot pay for its own sweep. That is NOT a
// transient failure: retrying forever would never drain it. The escape hatch is
// writeOffDust, which force-retires a provably un-sweepable residual.
func isUneconomicResidual(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "too small to cover the sweep fee") ||
		strings.Contains(msg, "fee exceeds half the tranche value")
}

// isNothingToMigrate reports the benign "there is no retiring generation" case.
func isNothingToMigrate(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no retiring or draining vault to migrate")
}

// isNotOwner reports that the contract refused the op because the caller is not the
// contract owner.
//
// migrateVault / retireVault / writeOffDust are ALL owner-only (checkOwner), but the
// bot submits L2 transactions as its own did:pkh identity (BotEthPrivKey). Unless the
// contract's owner IS that DID, every vault op the driver issues is rejected — the
// rotation silently never progresses while the bot burns RC retrying. This is a
// deployment-level precondition, not something the bot can fix at runtime, so the
// driver latches and reports it instead of hammering the chain.
func isNotOwner(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "must be performed by the contract owner")
}

// vaultAction is what one driver cycle decided to do.
type vaultAction int

const (
	// vaultNoop: no vault registry, no superseded generation, or a sweep is
	// already in flight and being settled by the spend pipeline.
	vaultNoop vaultAction = iota
	// vaultMigrate: a superseded generation still holds UTXOs → build the next tranche.
	vaultMigrate
	// vaultRetire: every superseded generation is drained → advance draining →
	// inactive → purged.
	vaultRetire
)

// supersededFundHolding returns the generations whose BTC must be migrated to the
// successor before they can retire. Mirrors the contract's isFundHoldingStatus
// minus Active — INACTIVE is included deliberately: a late/reorg deposit can
// re-fund an emptied generation, and it must be swept again rather than escape.
func supersededFundHolding(vaults []btcvault.Vault) []btcvault.Vault {
	out := make([]btcvault.Vault, 0, len(vaults))
	for _, v := range vaults {
		if v.Status == btcvault.VaultStatusRetiring ||
			v.Status == btcvault.VaultStatusDraining ||
			v.Status == btcvault.VaultStatusInactive {
			out = append(out, v)
		}
	}
	return out
}

// decideVaultAction is the whole policy of the driver, kept pure so it can be
// tested without a chain: given the vault registry, how many UTXOs each
// generation still holds, and whether a sweep is already in flight, decide the
// single next step. Returns the still-funded generations for logging.
func decideVaultAction(vaults []btcvault.Vault, counts map[uint32]int, sweepInFlight bool) (vaultAction, []btcvault.Vault) {
	superseded := supersededFundHolding(vaults)
	if len(superseded) == 0 {
		return vaultNoop, nil
	}

	stillFunded := make([]btcvault.Vault, 0, len(superseded))
	for _, v := range superseded {
		if counts[v.Generation] > 0 {
			stillFunded = append(stillFunded, v)
		}
	}
	if len(stillFunded) == 0 {
		// Drained — the contract's fund-gate will now let retireVault advance it.
		return vaultRetire, nil
	}
	if sweepInFlight {
		// Never stack tranches: a second sweep while one is unconfirmed draws the fee
		// reserve down twice and races the same UTXO set.
		return vaultNoop, stillFunded
	}
	return vaultMigrate, stillFunded
}

// reportNotOwner latches and explains the deployment-level precondition that stops
// the driver dead. It is deliberately loud: the symptom otherwise is a rotation that
// simply never finishes, with the committee's bond locked and the retiring
// generation's BTC still sitting in an abandoned vault.
func (b *Bot) reportNotOwner(contractId, action string) {
	if !markNotOwner(contractId) {
		return
	}
	b.L.Error("vault rotation DISABLED: this bot is not the contract owner, so the vault ops are rejected",
		"action", action, "contract", contractId)
	b.L.Error("vault rotation: migrateVault/retireVault/writeOffDust are owner-only (checkOwner), but the bot " +
		"submits L2 transactions as its own did:pkh identity. Nothing will drain a rotated-out generation until " +
		"either (a) the contract owner is set to this bot's DID, or (b) these three ops are made permissionless " +
		"like confirmSpend (they are deterministic and self-validating: the sweep's outputs must pay the committed " +
		"successor, retire is fund-gated, and the dust write-off is gated on provable un-sweepability). Until then " +
		"a rotation stalls half-drained and the committee's consensus bond stays locked.")
}

// HandleVaultRotation drives one step of the BTC vault-rotation lifecycle. It is
// a no-op on any contract without a vault registry (dash/ltc/... and a
// pre-rotation BTC deploy), so it is safe to call unconditionally from the block
// loop.
func (b *Bot) HandleVaultRotation() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	contractId := b.BotConfig.ContractId()

	// The vault ops are owner-only. If this bot is not the owner, no amount of
	// retrying will ever succeed — stay out of the way rather than burn RC.
	if isNotOwnerLatched(contractId) {
		return
	}

	vaults, err := b.FetchVaultRegistry(ctx)
	if err != nil {
		// Fail CLOSED and loudly: an unreadable registry means we cannot tell
		// whether a generation is waiting to be drained. Do NOT treat it as "nothing
		// to do".
		b.L.Warn("vault rotation: cannot read the vault registry — skipping this cycle", "error", err)
		return
	}
	if len(vaults) == 0 {
		return // not a vault contract, or no rotation has ever happened
	}

	if len(supersededFundHolding(vaults)) == 0 {
		return // single active generation — nothing to drain
	}

	counts, err := b.FetchGenUtxoCounts(ctx)
	if err != nil {
		b.L.Warn("vault rotation: cannot read the utxo registry — skipping this cycle", "error", err)
		return
	}

	// A sweep already in flight is being broadcast + settled by the existing
	// HandleUnmap/HandleConfirmations pipeline; we must not stack another tranche
	// on top of it.
	txSpends, err := b.gql().FetchTxSpends(ctx)
	if err != nil {
		b.L.Warn("vault rotation: cannot read pending spends — skipping this cycle", "error", err)
		return
	}
	pending := make([]string, 0, len(txSpends))
	for txId := range txSpends {
		pending = append(pending, txId)
	}
	inFlightSweeps, err := b.InFlightSweepTxIds(ctx, pending)
	if err != nil {
		b.L.Warn("vault rotation: cannot check for an in-flight sweep — skipping this cycle", "error", err)
		return
	}
	inFlight := len(inFlightSweeps) > 0

	// A sweep that was broadcast but never confirms (built at too low a fee) would keep
	// inFlight true forever and wedge the drain. Re-drive any that have gone stale — the
	// contract's own staleness gate is the final arbiter, so an early call is simply
	// refused. Do this BEFORE building a new tranche.
	if inFlight {
		if h, herr := b.FetchContractHeight(ctx); herr == nil {
			for _, txId := range noteSweepsInFlight(inFlightSweeps, h) {
				if !mayIssueVaultOp(contractId) {
					break
				}
				b.L.Warn("vault rotation: a migration sweep is stuck (unconfirmed past the stale window) — re-driving at a higher fee", "txId", txId)
				if _, rerr := b.callWithRetry(ctx, json.RawMessage(txId), "redriveSpend", broadcastRetryAttempts); rerr != nil {
					if isNotOwner(rerr) {
						b.reportNotOwner(contractId, "redriveSpend")
					} else {
						b.L.Warn("vault rotation: redriveSpend of a stuck sweep failed (will retry next cycle)", "txId", txId, "error", rerr)
					}
				}
			}
		} else {
			b.L.Debug("vault rotation: cannot read contract height for stuck-sweep detection", "error", herr)
		}
	}

	action, stillFunded := decideVaultAction(vaults, counts, inFlight)

	switch action {
	case vaultNoop:
		if inFlight {
			b.L.Debug("vault rotation: a migration sweep is already in flight — waiting for it to settle")
		}
		return

	case vaultRetire:
		// retireVault is a no-op-tolerant sweeper, so re-issuing is safe; the rate
		// limit keeps it from burning RC while the 144-block purge grace elapses.
		if !mayIssueVaultOp(contractId) {
			return
		}
		if _, err := b.callWithRetry(ctx, json.RawMessage(`{}`), "retireVault", broadcastRetryAttempts); err != nil {
			if isNotOwner(err) {
				b.reportNotOwner(contractId, "retireVault")
				return
			}
			b.L.Debug("vault rotation: retireVault made no transition (expected while the purge grace elapses)", "error", err)
			return
		}
		b.L.Info("vault rotation: retireVault issued — superseded generation(s) drained, advancing lifecycle")
		return
	}

	// vaultMigrate
	if !mayIssueVaultOp(contractId) {
		return
	}
	for _, v := range stillFunded {
		b.L.Info("vault rotation: superseded generation still holds funds — building the next migration tranche",
			"generation", v.Generation, "utxos", counts[v.Generation], "status", v.Status)
	}

	_, err = b.callWithRetry(ctx, json.RawMessage(`{}`), "migrateVault", broadcastRetryAttempts)
	if err == nil {
		b.L.Info("vault rotation: migrateVault issued — the sweep will be TSS-signed, broadcast and settled by the spend pipeline")
		clearFeeReserveWarning(contractId)
		return
	}

	if isNothingToMigrate(err) {
		return // raced a concurrent settle; nothing to do
	}

	if isNotOwner(err) {
		b.reportNotOwner(contractId, "migrateVault")
		return
	}

	// The residual cannot pay for its own sweep. Retrying migrateVault forever
	// would never drain it and the generation could never retire — force-retire the
	// provably un-sweepable dust instead.
	if isUneconomicResidual(err) {
		b.L.Warn("vault rotation: residual is uneconomic to sweep — writing off the dust so the drain can complete", "error", err)
		if _, werr := b.callWithRetry(ctx, json.RawMessage(`{}`), "writeOffDust", broadcastRetryAttempts); werr != nil {
			b.L.Error("vault rotation: writeOffDust FAILED — the generation cannot drain and rotation is stuck; operator action required", "error", werr)
		}
		return
	}

	// Anything else (most commonly: the fee reserve is empty, which only a BTC
	// deposit to the active vault via topUpFeeReserve can fix — the bot cannot).
	b.L.Error("vault rotation: migrateVault failed — the rotation cannot progress until this is resolved", "error", err)
	if warnFeeReserveOnce(contractId) {
		b.L.Error("vault rotation: if this is a fee-reserve failure, the operator must fund the FeeSupply " +
			"(a BTC deposit to the ACTIVE vault's untagged address, then topUpFeeReserve) — no sweep can be built without it")
	}
}
