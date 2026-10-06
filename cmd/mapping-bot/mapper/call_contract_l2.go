package mapper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/contracts"
	transactionpool "vsc-node/modules/transaction-pool"
)

// errBotEthKeyMissing is returned when the bot is asked to submit an L2 tx but
// no signing key is configured.
var errBotEthKeyMissing = errors.New("L2 signing key not configured for this bot")

// callContractL2 submits a vsc.call contract invocation through the VSC L2
// transaction pool using the bot's did:pkh:eip155 identity.
//
// The bot's DID must have HBD balance to cover the RC cost — it has no free
// allotment (unlike hive: accounts). A funding error surfaces as
// "not enough RCS available" from the node.
func (b *Bot) callContractL2(
	ctx context.Context,
	contractInput json.RawMessage,
	action string,
) (string, error) {
	if b.botEthKey == nil {
		return "", errBotEthKeyMissing
	}

	did := b.botEthDID

	// Serialize concurrent L2 submissions — see Bot.l2SubmitMu.
	b.l2SubmitMu.Lock()
	defer b.l2SubmitMu.Unlock()

	nonce, err := b.gql().FetchAccountNonce(ctx, did.String())
	if err != nil {
		return "", fmt.Errorf("fetch L2 nonce: %w", err)
	}

	// Per-action: the vault-rotation ops need a far higher ceiling than the bot's
	// configured default, which every other action keeps unchanged (see rcLimitFor).
	rcLimit := b.rcLimitFor(action)

	// Vault ops are sized by a dry run first (BOT-RC-1, BOT-ERR-1).
	if isVaultOp(action) {
		sized, err := b.sizeVaultOpByDryRun(ctx, did.String(), action, string(contractInput), rcLimit)
		if err != nil {
			return "", err
		}
		rcLimit = sized
	}

	// VR2-04: refuse before submitting rather than stalling mid-cycle.
	//
	// A rotation costs roughly 28,000 RC across map, topUpFeeReserve, migrateVault
	// and confirmSpend, and there was no pre-flight at all. Running out partway
	// leaves a generation half-swept with an in-flight spend that the same account
	// can no longer finish -- a testnet confirmSpend aborted at RC 83 exactly this
	// way. Failing before the first op is recoverable; failing between them is the
	// state that needs manual recovery.
	//
	// Fails OPEN on an unreadable RC balance: a monitoring outage must not become a
	// rotation outage, and the node will still reject the op itself if the credits
	// really are missing.
	if available, known, rcErr := b.gql().FetchAccountRC(ctx, did.String()); rcErr != nil {
		b.L.Warn("RC pre-flight skipped: could not read available credits",
			"action", action, "err", rcErr)
	} else if known && available < int64(rcLimit) {
		return "", fmt.Errorf(
			"insufficient RC for %s: %d available, %d needed for this op alone; "+
				"fund the bot account before starting a rotation (a mid-cycle stall "+
				"leaves an in-flight spend this account cannot finish)",
			action, available, rcLimit)
	}
	call := &transactionpool.VscContractCall{
		ContractId: b.BotConfig.ContractId(),
		Action:     action,
		Payload:    string(contractInput),
		RcLimit:    rcLimit,
		Intents:    []contracts.Intent{},
		Caller:     did.String(),
		NetId:      b.SystemConfig.NetId(),
	}
	op, err := call.SerializeVSC()
	if err != nil {
		return "", fmt.Errorf("serialize L2 op: %w", err)
	}

	vscTx := transactionpool.VSCTransaction{
		Ops:     []transactionpool.VSCTransactionOp{op},
		Nonce:   nonce,
		NetId:   b.SystemConfig.NetId(),
		RcLimit: rcLimit,
	}

	crafter := transactionpool.TransactionCrafter{
		Identity: dids.NewEthProvider(b.botEthKey),
		Did:      did,
	}
	sTx, err := crafter.SignFinal(vscTx)
	if err != nil {
		return "", fmt.Errorf("sign L2 tx: %w", err)
	}

	if len(sTx.Tx) > transactionpool.MAX_TX_SIZE {
		b.L.Error("L2 transaction exceeds maximum size — cannot submit",
			"action", action,
			"cbor_size", len(sTx.Tx),
			"limit", transactionpool.MAX_TX_SIZE,
		)
		return "", fmt.Errorf("L2 tx too large: %d bytes (limit %d)", len(sTx.Tx), transactionpool.MAX_TX_SIZE)
	}

	txID, err := b.gql().SubmitTransactionV1(
		ctx,
		base64.URLEncoding.EncodeToString(sTx.Tx),
		base64.URLEncoding.EncodeToString(sTx.Sig),
	)
	if err != nil {
		return "", fmt.Errorf("broadcast L2 tx: %w", err)
	}

	b.L.Info("L2 tx broadcast",
		"id", txID,
		"action", action,
		"nonce", nonce,
		"cbor_size", len(sTx.Tx),
		"did", did.String(),
	)
	return txID, nil
}

const (
	// simulateRcCap is the most simulateContractCalls accepts per call.
	simulateRcCap uint64 = 100_000
	// dryRunRcFloor keeps a sized limit from being cut too fine for a call whose
	// cost moves a little between the dry run and the block it lands in.
	dryRunRcFloor uint64 = 20_000
)

// isVaultOp reports the rotation ops that get vaultOpRcLimit (see rcLimitFor).
func isVaultOp(action string) bool {
	switch action {
	case "migrateVault", "retireVault", "writeOffDust", "redriveSpend":
		return true
	}
	return false
}

// sizeVaultOpByDryRun dry-runs a vault op as the bot's own DID and returns the
// rc_limit to declare.
//
// BOT-RC-1 (testnet 2026-10-06): every vault op declared the 8,000,000 ceiling and
// the pre-flight refused unless the DID held that much RC, so a real operator DID
// (mainnet's holds ~466,000) could never issue one. A one-input legacy migrateVault
// dry-runs at ~10,700 and a two-input one at ~13,100 (testnet 2026-10-06). Declaring
// twice the dry run's use (at least dryRunRcFloor) keeps the pre-flight honest at a
// realistic number.
//
// BOT-ERR-1: the rotation driver chooses its next step from the contract's refusal
// text (nothing to migrate, uneconomic residual -> writeOffDust, not the owner),
// but an on-chain FAILED status carries no text, so those branches never fired and
// each refused op cost RC. A dry-run refusal is returned with the contract's
// reason and nothing is submitted.
//
// Falls back to the ceiling when the dry run is unavailable: the node still enforces
// its own limits and the next cycle dry-runs again. An op that needs more than the
// simulate cap (a tranche of up to MaxMigrationInputs = 100 inputs can) declares
// what the DID holds instead: the ceiling would fail the pre-flight on every cycle
// and the same tranche is re-selected each time, so the rotation would never move.
func (b *Bot) sizeVaultOpByDryRun(ctx context.Context, caller, action, payload string, ceiling uint64) (uint64, error) {
	sim, err := b.gql().SimulateContractCall(ctx, caller, b.BotConfig.ContractId(), action, payload, simulateRcCap)
	if err != nil {
		b.L.Warn("vault op dry run unavailable; declaring the rc ceiling", "action", action, "error", err)
		return ceiling, nil
	}
	if !sim.Success {
		if sim.Err == "gas_limit_hit" {
			return b.sizeAboveDryRunCap(ctx, caller, action, ceiling)
		}
		return 0, fmt.Errorf("%s refused in dry run (%s): %s", action, sim.Err, sim.ErrMsg)
	}
	if action == "retireVault" && sim.Ret == retireNoTransitions {
		return 0, errRetireNoTransition
	}
	sized := uint64(0)
	if sim.RcUsed > 0 {
		sized = uint64(sim.RcUsed) * 2
	}
	if sized < dryRunRcFloor {
		sized = dryRunRcFloor
	}
	if sized > ceiling {
		sized = ceiling
	}
	return sized, nil
}

// sizeAboveDryRunCap sizes a vault op the dry run could not measure because it
// needs more than simulateRcCap: it declares what the DID holds (at most the
// ceiling). The node charges only what the op uses.
func (b *Bot) sizeAboveDryRunCap(ctx context.Context, caller, action string, ceiling uint64) (uint64, error) {
	available, known, err := b.gql().FetchAccountRC(ctx, caller)
	if err != nil || !known {
		b.L.Warn("vault op needs more than the dry-run cap and RC is unreadable; declaring the rc ceiling", "action", action)
		return ceiling, nil
	}
	if available <= int64(simulateRcCap) {
		return 0, fmt.Errorf("%s needs more than %d RC (the dry-run cap) and the bot holds %d: fund the bot account", action, simulateRcCap, available)
	}
	if uint64(available) > ceiling {
		return ceiling, nil
	}
	b.L.Warn("vault op needs more than the dry-run cap; declaring the RC the bot holds", "action", action, "rc", available)
	return uint64(available), nil
}
