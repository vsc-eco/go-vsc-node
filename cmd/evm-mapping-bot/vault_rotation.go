package main

// A01: the EVM bridge's vault rolls keys like the BTC vault, and this file
// drives it the way the BTC bot's vault_rotation.go drives that one.
//
// The bridge is pull-driven. It builds a migration sweep only when someone calls
// migrateVault (one sweep in flight per generation), settles it only on a
// confirmSweep proof, and advances a drained generation only on retireVault.
// The bot:
//   - watches every fund-holding generation's address (deposits to an old
//     address are still credited and swept);
//   - builds the next sweep (with an account proof of the retiring address
//     for the ETH step), fetches the retiring key's signature, broadcasts it,
//     re-drives it if it does not mine, and confirms it once its block's
//     header is stored;
//   - retires drained generations and writes off ETH dust.
//
// Every vault op is dry-run first and sized from the dry run (as the BTC bot,
// BOT-RC-1 / BOT-ERR-1): a refusal costs no RC, and the declared rc_limit is
// twice what the call used.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/btcvault"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// vaultGen is one generation of the bridge's vault as the bot needs it.
type vaultGen struct {
	Gen     uint32
	Status  btcvault.VaultStatus
	KeyID   string // node key id: "<contract>-primary" / "-primaryv<N>"
	Address string // 0x lowercase; "" while the generation has no key
}

// vaultView is the bridge's vault at one read. Registry false: a pre-rotation
// bridge whose one key is the vault.
type vaultView struct {
	Registry bool
	Gens     []vaultGen
	Active   *vaultGen
}

// fundHolding lists the addresses whose deposits the bridge credits: Active,
// Retiring, Draining, Inactive (the contract's isFundHoldingStatus).
func (v vaultView) fundHolding() []string {
	var out []string
	for _, g := range v.Gens {
		if g.Address != "" && btcvault.IsFundHoldingStatus(g.Status) {
			out = append(out, g.Address)
		}
	}
	return out
}

// decodeVaultView reads the registry blob ("v", hex) of contract.
func decodeVaultView(contract, rawHex string) (vaultView, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(rawHex, "0x"))
	if err != nil {
		return vaultView{}, fmt.Errorf("vault registry hex: %w", err)
	}
	if len(raw) == 0 {
		return vaultView{}, nil
	}
	vs, err := btcvault.UnmarshalVaultRegistry(raw)
	if err != nil {
		return vaultView{}, err
	}
	view := vaultView{Registry: true}
	for _, v := range vs {
		g := vaultGen{Gen: v.Generation, Status: v.Status,
			KeyID: contract + "-" + btcvault.VaultKeyNameFor(btcvault.EvmVaultKeyBase, v.Generation)}
		if addr, ok := tssPubKeyToAddress(hex.EncodeToString(v.Primary)); ok {
			g.Address = strings.ToLower(addr) // none while the generation has no key (zero bytes)
		}
		view.Gens = append(view.Gens, g)
	}
	for i := range view.Gens {
		if view.Gens[i].Status == btcvault.VaultStatusActive {
			view.Active = &view.Gens[i]
		}
	}
	return view, nil
}

func fetchVaultView(ctx context.Context, gql *vscGraphQL, contract string) (vaultView, error) {
	st, err := gql.fetchContractStateHex(ctx, contract, []string{"v"})
	if err != nil {
		return vaultView{}, err
	}
	return decodeVaultView(contract, st["v"])
}

// sweepVersion / sweepRecord mirror the bridge's SweepRecord (mapping/migration.go).
type sweepVersion struct {
	Value    string `json:"value"`
	Unsigned string `json:"unsigned"`
}

type sweepRecord struct {
	Gen      uint32         `json:"gen"`
	Nonce    uint64         `json:"nonce"`
	Kind     string         `json:"kind"`
	Asset    string         `json:"asset"`
	To       string         `json:"to"`
	BuiltL1  uint64         `json:"built_l1"`
	Versions []sweepVersion `json:"versions"`
}

func fetchOpenSweeps(ctx context.Context, gql *vscGraphQL, contract string) ([]sweepRecord, error) {
	st, err := gql.fetchContractState(ctx, contract, []string{"msr"})
	if err != nil || st["msr"] == "" {
		return nil, err
	}
	keys := strings.Split(st["msr"], "|")
	recKeys := make([]string, len(keys))
	for i, k := range keys {
		recKeys[i] = "ms-" + k
	}
	recs, err := gql.fetchContractState(ctx, contract, recKeys)
	if err != nil {
		return nil, err
	}
	var out []sweepRecord
	for _, k := range recKeys {
		var r sweepRecord
		if json.Unmarshal([]byte(recs[k]), &r) == nil && len(r.Versions) > 0 {
			out = append(out, r)
		}
	}
	return out, nil
}

// signedSweep attaches the TSS signature (DER) over a version's sighash to
// the version's unsigned transaction.
func signedSweep(unsignedHex string, sig TssSignature, from string) (string, error) {
	der := sig.Bytes
	if len(der) == 0 {
		return "", errors.New("empty signature")
	}
	digest, err := computeSighash(unsignedHex)
	if err != nil {
		return "", err
	}
	rsv, err := vaultCheckRSV(der, digest, from)
	if err != nil {
		return "", err
	}
	return attachSignatureToTx(unsignedHex, rsv[64], rsv[:32], rsv[32:64])
}

// confirmSweepRequest is the bridge's ConfirmSweepRequest.
type confirmSweepRequest struct {
	Gen          uint32 `json:"gen"`
	Nonce        uint64 `json:"nonce"`
	BlockHeight  uint64 `json:"block_height"`
	TxIndex      uint64 `json:"tx_index"`
	TxProof      string `json:"tx_proof"`
	ReceiptProof string `json:"receipt_proof"`
}

// buildConfirmSweepPayload proves the sweep tx at txIndex of block height, the
// way confirmSpend's payload is built.
func buildConfirmSweepPayload(ctx context.Context, rpc *ethRPC, gen uint32, nonce, height uint64, txIndex int) (json.RawMessage, error) {
	bp, err := fetchBlockProofs(ctx, rpc, height, true)
	if err != nil {
		return nil, fmt.Errorf("confirmSweep: %w", err)
	}
	_, txNodes, err := bp.txProof(txIndex)
	if err != nil {
		return nil, fmt.Errorf("confirmSweep: %w", err)
	}
	_, rcNodes, err := bp.receiptProof(txIndex)
	if err != nil {
		return nil, fmt.Errorf("confirmSweep: %w", err)
	}
	return json.Marshal(confirmSweepRequest{Gen: gen, Nonce: nonce, BlockHeight: height, TxIndex: uint64(txIndex),
		TxProof:      base64.StdEncoding.EncodeToString(bytes.Join(txNodes, nil)),
		ReceiptProof: base64.StdEncoding.EncodeToString(bytes.Join(rcNodes, nil))})
}

// accountProof is the bridge's AccountProof: eth_getProof's account proof of
// address at block height.
type accountProof struct {
	BlockHeight uint64 `json:"block_height"`
	Proof       string `json:"account_proof"`
}

// fetchAccountProof returns address's proof, nonce and balance at height.
func fetchAccountProof(ctx context.Context, rpc *ethRPC, address string, height uint64) (accountProof, uint64, *big.Int, error) {
	res, err := rpc.callCtx(ctx, "eth_getProof", fmt.Sprintf(`"%s", [], "0x%x"`, address, height))
	if err != nil {
		return accountProof{}, 0, nil, fmt.Errorf("eth_getProof %s at %d: %w", address, height, err)
	}
	var p struct {
		Nonce        string   `json:"nonce"`
		Balance      string   `json:"balance"`
		AccountProof []string `json:"accountProof"`
	}
	if err := json.Unmarshal(res, &p); err != nil {
		return accountProof{}, 0, nil, fmt.Errorf("parse eth_getProof: %w", err)
	}
	var nodes []byte
	for _, n := range p.AccountProof {
		nodes = append(nodes, hexToBytes(n)...)
	}
	bal, _ := new(big.Int).SetString(strings.TrimPrefix(p.Balance, "0x"), 16)
	if bal == nil {
		bal = new(big.Int)
	}
	return accountProof{BlockHeight: height, Proof: base64.StdEncoding.EncodeToString(nodes)}, hexToUint64(p.Nonce), bal, nil
}

// ---------------------------------------------------------------------------
// Dry run and RC sizing, as the BTC bot (call_contract_l2.go).

type simulatedCall struct {
	Success bool
	Err     string
	ErrMsg  string
	Ret     string
	RcUsed  int64
}

const (
	simulateRcCap  uint64 = 100_000
	dryRunRcFloor  uint64 = 20_000
	vaultOpRcLimit uint64 = 1_000_000
)

func (g *vscGraphQL) simulateContractCall(ctx context.Context, caller, contract, action, payload string) (simulatedCall, error) {
	data, err := g.query(ctx,
		`query($i: SimulateContractCallsInput!){ simulateContractCalls(input: $i){ success err err_msg ret rc_used } }`,
		map[string]interface{}{"i": map[string]interface{}{
			"tx_id":          fmt.Sprintf("evm-bot-dryrun-%d", time.Now().UnixNano()),
			"required_auths": []string{caller},
			"calls": []map[string]interface{}{{
				"contract_id": contract, "action": action, "payload": payload, "rc_limit": simulateRcCap,
			}},
		}})
	if err != nil {
		return simulatedCall{}, err
	}
	var out struct {
		Sim []struct {
			Success bool    `json:"success"`
			Err     *string `json:"err"`
			ErrMsg  *string `json:"err_msg"`
			Ret     *string `json:"ret"`
			RcUsed  int64   `json:"rc_used"`
		} `json:"simulateContractCalls"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.Sim) != 1 {
		return simulatedCall{}, fmt.Errorf("simulate %s: bad response", action)
	}
	r := out.Sim[0]
	s := simulatedCall{Success: r.Success, RcUsed: r.RcUsed}
	if r.Err != nil {
		s.Err = *r.Err
	}
	if r.ErrMsg != nil {
		s.ErrMsg = *r.ErrMsg
	}
	if r.Ret != nil {
		s.Ret = *r.Ret
	}
	return s, nil
}

// rcFromDryRun is the rc_limit to declare for a call that used rcUsed in its dry
// run: twice that, at least dryRunRcFloor, at most ceiling (BOT-RC-1).
func rcFromDryRun(rcUsed int64, ceiling uint64) uint64 {
	sized := uint64(0)
	if rcUsed > 0 {
		sized = uint64(rcUsed) * 2
	}
	if sized < dryRunRcFloor {
		sized = dryRunRcFloor
	}
	if sized > ceiling {
		sized = ceiling
	}
	return sized
}

// errDryRunRefused carries the contract's reason; nothing is submitted.
var errDryRunRefused = errors.New("refused in dry run")

// vaultOp dry-runs action as the bot and, if it would succeed, submits it with
// an rc_limit sized from the dry run. It returns the contract's return value.
func vaultOp(ctx context.Context, gql *vscGraphQL, sub *l2Submitter, contract, action string, payload json.RawMessage) (string, error) {
	sim, err := gql.simulateContractCall(ctx, sub.did.String(), contract, action, string(payload))
	rc := vaultOpRcLimit
	switch {
	case err != nil:
		slog.Warn("vault op dry run unavailable; declaring the ceiling", "action", action, "err", err)
	case !sim.Success:
		return "", fmt.Errorf("%w: %s %s: %s", errDryRunRefused, action, sim.Err, sim.ErrMsg)
	default:
		rc = rcFromDryRun(sim.RcUsed, vaultOpRcLimit)
	}
	if _, err := sub.callContractL2(ctx, contract, action, payload, rc); err != nil {
		return "", err
	}
	return sim.Ret, nil
}

// ---------------------------------------------------------------------------
// The driver.

// vaultOpMinInterval bounds how often one kind of vault op is issued: every op
// costs RC, and a waiting state ("gas reserve below...", the purge grace)
// would otherwise re-issue a doomed call every tick (BTC bot).
const vaultOpMinInterval = 2 * time.Minute

// sweepRedriveL1Blocks mirrors the bridge's SweepRedriveL1Blocks, one block
// above it so the bot never races the contract's own boundary.
const sweepRedriveL1Blocks = 33

var (
	vaultOpMu     sync.Mutex
	lastVaultOpAt = map[string]time.Time{}
	notOperator   bool
)

func vaultOpDue(kind string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	if notOperator {
		return false
	}
	return time.Since(lastVaultOpAt[kind]) >= vaultOpMinInterval
}

func noteVaultOp(kind string) {
	vaultOpMu.Lock()
	lastVaultOpAt[kind] = time.Now()
	vaultOpMu.Unlock()
}

// noteRefusal latches "this bot is not the vault operator": no retry of an
// operator op can succeed, so stop issuing them (BTC bot notOwner).
func noteRefusal(err error) {
	if err != nil && strings.Contains(err.Error(), "owner or the vault operator") {
		vaultOpMu.Lock()
		if !notOperator {
			slog.Error("this bot is not the bridge's vault operator: rotation ops stop (setVaultOperator to this DID)")
		}
		notOperator = true
		vaultOpMu.Unlock()
	}
}

// driveVaultRotationSafe is driveVaultRotation behind a recover, like
// runOnceSafe: a panic in the driver must not stop the bot.
func driveVaultRotationSafe(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, view vaultView) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("recovered from panic in the vault rotation driver", "panic", r)
		}
	}()
	driveVaultRotation(ctx, rpc, gql, sub, cp, cfg, view)
}

// driveVaultRotation runs one pass of the rotation driver.
func driveVaultRotation(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, view vaultView) {
	if !view.Registry || view.Active == nil {
		return
	}
	sweeps, err := fetchOpenSweeps(ctx, gql, cfg.ContractID)
	if err != nil {
		slog.Debug("open sweeps unreadable", "err", err)
		return
	}
	for _, r := range sweeps {
		driveSweep(ctx, rpc, gql, sub, cp, cfg, view, r, r.Nonce == lowestOpenNonce(sweeps, r.Gen))
	}
	superseded := false
	for _, g := range view.Gens {
		switch g.Status {
		case btcvault.VaultStatusRetiring, btcvault.VaultStatusDraining, btcvault.VaultStatusInactive:
			superseded = true
		}
	}
	if !superseded {
		return
	}
	if len(sweeps) == 0 && vaultOpDue("migrate") {
		migrateNext(ctx, rpc, gql, sub, cp, cfg, view)
	}
	if vaultOpDue("retire") {
		noteVaultOp("retire")
		ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "retireVault", json.RawMessage(`{}`))
		noteRefusal(err)
		if err == nil && ret != "retire: no generation transitions" {
			slog.Info("vault retire", "result", ret)
		}
	}
}

// migrateNext builds the next sweep: a token, or with an account proof the ETH.
func migrateNext(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, view vaultView) {
	noteVaultOp("migrate")
	ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "migrateVault", json.RawMessage(`{}`))
	noteRefusal(err)
	if err == nil {
		logMigrate(ret)
		if strings.Contains(ret, "dust") {
			writeOffDust(ctx, gql, sub, cfg)
		}
		return
	}
	if !strings.Contains(err.Error(), "eth_proof") {
		slog.Warn("migrateVault", "err", err)
		return
	}
	// The ETH sweep is next: prove the retiring address's balance at a stored
	// header where its nonce is the next sweep's.
	target := ethSweepTarget(view)
	if target == nil {
		return
	}
	info, err := fetchVaultInfo(ctx, gql, sub, cfg.ContractID)
	if err != nil {
		slog.Warn("vaultInfo unreadable", "err", err)
		return
	}
	nonce, booked := info.sweepNonce(target.Gen), info.lastBooked(ctx, gql, cfg.ContractID, target.Gen)
	proof, ok := balanceProofAt(ctx, rpc, gql, cp, cfg, target.Address, nonce, booked)
	if !ok {
		return
	}
	payload, _ := json.Marshal(map[string]any{"eth_proof": proof})
	ret, err = vaultOp(ctx, gql, sub, cfg.ContractID, "migrateVault", payload)
	noteRefusal(err)
	if err != nil {
		slog.Warn("migrateVault (ETH)", "err", err)
		return
	}
	logMigrate(ret)
	if strings.Contains(ret, "dust") {
		writeOffDust(ctx, gql, sub, cfg)
	}
}

func logMigrate(ret string) {
	if strings.HasPrefix(ret, "sweep ") {
		slog.Info("migration sweep built", "result", ret)
		return
	}
	slog.Info("migrateVault: nothing built", "result", ret)
}

func writeOffDust(ctx context.Context, gql *vscGraphQL, sub *l2Submitter, cfg EVMBotConfig) {
	ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "writeOffDust", json.RawMessage(`{}`))
	noteRefusal(err)
	if err == nil {
		slog.Info("dust write-off", "result", ret)
	}
}

// ethSweepTarget is the generation migrateVault targets: the first Retiring,
// else Draining, generation.
func ethSweepTarget(view vaultView) *vaultGen {
	for _, want := range []btcvault.VaultStatus{btcvault.VaultStatusRetiring, btcvault.VaultStatusDraining} {
		for i := range view.Gens {
			if view.Gens[i].Status == want && view.Gens[i].Address != "" {
				return &view.Gens[i]
			}
		}
	}
	return nil
}

// balanceProofAt returns an account proof of address at a header the bridge
// has stored, where address's nonce is nonce and which is not older than the
// generation's newest booked deposit. It tries the newest stored header first
// (no new proof to pay for) and otherwise asks for the finalized head's.
func balanceProofAt(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig, address string, nonce, booked uint64) (accountProof, bool) {
	if h, err := fetchContractLastHeight(ctx, gql, cfg.ContractID); err == nil && h >= booked {
		if p, n, _, err := fetchAccountProof(ctx, rpc, address, h); err == nil && n == nonce {
			return p, true
		}
	}
	fin, err := rpc.getFinalizedBlock()
	if err != nil {
		return accountProof{}, false
	}
	if st, err := headersStored(ctx, gql, cfg.ContractID, []uint64{fin}); err == nil && st[fin] {
		if p, n, _, err := fetchAccountProof(ctx, rpc, address, fin); err == nil && n == nonce {
			return p, true
		}
		return accountProof{}, false
	}
	noteHeaderNeed(cp, fin)
	slog.Info("ETH sweep waits for a stored header to prove the retiring address's balance", "block", fin)
	return accountProof{}, false
}

type vaultInfoView struct {
	Generations []struct {
		Generation uint32  `json:"generation"`
		SweepNonce *uint64 `json:"sweep_nonce"`
	} `json:"generations"`
}

func (v vaultInfoView) sweepNonce(gen uint32) uint64 {
	for _, g := range v.Generations {
		if g.Generation == gen && g.SweepNonce != nil {
			return *g.SweepNonce
		}
	}
	return 0
}

func (v vaultInfoView) lastBooked(ctx context.Context, gql *vscGraphQL, contract string, gen uint32) uint64 {
	k := "vbh-" + strconv.FormatUint(uint64(gen), 10)
	st, err := gql.fetchContractState(ctx, contract, []string{k})
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(st[k], 10, 64)
	return n
}

func fetchVaultInfo(ctx context.Context, gql *vscGraphQL, sub *l2Submitter, contract string) (vaultInfoView, error) {
	sim, err := gql.simulateContractCall(ctx, sub.did.String(), contract, "vaultInfo", "")
	if err != nil {
		return vaultInfoView{}, err
	}
	if !sim.Success {
		return vaultInfoView{}, fmt.Errorf("vaultInfo: %s %s", sim.Err, sim.ErrMsg)
	}
	var v vaultInfoView
	return v, json.Unmarshal([]byte(sim.Ret), &v)
}

// SentSweep is a sweep the bot broadcast: every signed version it sent (any of
// them may mine) and the confirmSweep submitted for it.
type SentSweep struct {
	TxHashes    []string `json:"tx_hashes"`
	SentAt      int64    `json:"sent_at"`
	ConfirmTxId string   `json:"confirm_tx_id,omitempty"`
	NextTryAt   int64    `json:"next_try_at,omitempty"`
}

// lowestOpenNonce is the lowest nonce among gen's open sweeps.
func lowestOpenNonce(sweeps []sweepRecord, gen uint32) uint64 {
	low, found := uint64(0), false
	for _, s := range sweeps {
		if s.Gen == gen && (!found || s.Nonce < low) {
			low, found = s.Nonce, true
		}
	}
	return low
}

// driveSweep broadcasts an open sweep's signed versions, confirms it once mined
// and its header is stored, and re-drives it when it stays unmined. Only the
// lowest open nonce of a generation is re-driven (redrive): the batch's later
// sweeps cannot mine before it, and re-pricing them would spend the gas kept
// back for real fee rises.
func driveSweep(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, view vaultView, r sweepRecord, redrive bool) {
	var gen *vaultGen
	for i := range view.Gens {
		if view.Gens[i].Gen == r.Gen {
			gen = &view.Gens[i]
		}
	}
	if gen == nil || gen.Address == "" {
		return
	}
	key := strconv.FormatUint(uint64(r.Gen), 10) + "-" + strconv.FormatUint(r.Nonce, 10)
	cp.mu.Lock()
	if cp.SentSweeps == nil {
		cp.SentSweeps = map[string]SentSweep{}
	}
	ss := cp.SentSweeps[key]
	cp.mu.Unlock()
	save := func() {
		cp.mu.Lock()
		cp.SentSweeps[key] = ss
		cp.mu.Unlock()
	}

	// A confirmSweep already submitted settles it (or failed: retry later).
	if ss.ConfirmTxId != "" {
		status, err := gql.fetchTransactionStatus(ctx, ss.ConfirmTxId)
		if err != nil || status == "CONFIRMED" {
			return // closed: the record disappears from msr
		}
		if status == "FAILED" {
			ss.ConfirmTxId, ss.NextTryAt = "", time.Now().Add(vaultOpMinInterval).Unix()
			save()
		}
		return
	}

	// Broadcast each signed version not yet sent.
	digests := make([]string, len(r.Versions))
	for i, v := range r.Versions {
		digests[i], _ = computeSighash(v.Unsigned)
	}
	sigs, err := gql.fetchTssSignatures(ctx, gen.KeyID, digests)
	if err != nil {
		return
	}
	for i, v := range r.Versions {
		sig, ok := sigs[digests[i]]
		if !ok || len(sig.Bytes) == 0 {
			continue
		}
		if !sweepTxPaysOnly(v.Unsigned, common.HexToAddress(view.Active.Address)) {
			slog.Error("REFUSING to broadcast a sweep that does not pay only the active vault generation", "sweep", key, "version", i)
			continue
		}
		signed, err := signedSweep(v.Unsigned, sig, gen.Address)
		if err != nil {
			slog.Error("sweep signature does not recover to the retiring address", "sweep", key, "err", err)
			continue
		}
		hash := strings.ToLower(ethCrypto.Keccak256Hash(hexToBytes(signed)).Hex())
		if !containsStr(ss.TxHashes, hash) {
			if _, err := rpc.broadcastTx(signed); err != nil && !strings.Contains(strings.ToLower(err.Error()), "known") {
				slog.Warn("sweep broadcast failed", "sweep", key, "err", err)
				continue
			}
			ss.TxHashes = append(ss.TxHashes, hash)
			if ss.SentAt == 0 {
				ss.SentAt = time.Now().Unix()
			}
			save()
			slog.Info("sweep broadcast", "sweep", key, "asset", r.Asset, "tx", hash)
		}
	}

	// Mined? Confirm once final and its header is stored.
	for _, h := range ss.TxHashes {
		rc, err := rpc.getReceipt(h)
		if err != nil || rc == nil || string(rc) == "null" {
			continue
		}
		var receipt struct {
			BlockNumber      string `json:"blockNumber"`
			TransactionIndex string `json:"transactionIndex"`
		}
		if json.Unmarshal(rc, &receipt) != nil {
			continue
		}
		block, idx := hexToUint64(receipt.BlockNumber), int(hexToUint64(receipt.TransactionIndex))
		fin, err := rpc.getFinalizedBlock()
		if err != nil || block > fin {
			return
		}
		if st, err := headersStored(ctx, gql, cfg.ContractID, []uint64{block}); err != nil || !st[block] {
			if err == nil {
				noteHeaderNeed(cp, block)
			}
			return
		}
		if ss.NextTryAt != 0 && time.Now().Unix() < ss.NextTryAt {
			return
		}
		payload, err := buildConfirmSweepPayload(ctx, rpc, r.Gen, r.Nonce, block, idx)
		if err != nil {
			slog.Warn("confirmSweep payload", "sweep", key, "err", err)
			return
		}
		sim, serr := gql.simulateContractCall(ctx, sub.did.String(), cfg.ContractID, "confirmSweep", string(payload))
		rcLimit := cfg.RcLimit
		if serr == nil && !sim.Success {
			slog.Warn("confirmSweep refused in dry run", "sweep", key, "err", sim.Err+" "+sim.ErrMsg)
			ss.NextTryAt = time.Now().Add(vaultOpMinInterval).Unix()
			save()
			return
		}
		if serr == nil {
			if need := rcFromDryRun(sim.RcUsed, vaultOpRcLimit); need > rcLimit {
				rcLimit = need
			}
		}
		txId, err := sub.callContractL2(ctx, cfg.ContractID, "confirmSweep", payload, rcLimit)
		if err != nil {
			slog.Warn("confirmSweep submit", "sweep", key, "err", err)
			return
		}
		ss.ConfirmTxId = txId
		save()
		slog.Info("confirmSweep submitted", "sweep", key, "l2Tx", txId)
		return
	}

	// Not mined: re-drive once the contract allows it.
	if redrive && len(ss.TxHashes) > 0 && vaultOpDue("redrive-"+key) {
		if h, err := fetchContractLastHeight(ctx, gql, cfg.ContractID); err == nil && h >= r.BuiltL1+sweepRedriveL1Blocks {
			noteVaultOp("redrive-" + key)
			payload, _ := json.Marshal(map[string]any{"gen": r.Gen, "nonce": r.Nonce})
			ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "redriveSweep", payload)
			noteRefusal(err)
			if err == nil {
				slog.Info("sweep re-driven", "sweep", key, "result", ret)
			}
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sweepTxPaysOnly mirrors the node's evmTxPaysOnly: the bot refuses to
// broadcast a signed sweep that could pay anyone but the active generation
// (defense in depth: the node would not have signed it).
func sweepTxPaysOnly(unsignedHex string, successor common.Address) bool {
	raw := hexToBytes(unsignedHex)
	if len(raw) < 2 || raw[0] != types.DynamicFeeTxType {
		return false
	}
	var tx struct {
		ChainID    *big.Int
		Nonce      uint64
		GasTipCap  *big.Int
		GasFeeCap  *big.Int
		Gas        uint64
		To         *common.Address `rlp:"nil"`
		Value      *big.Int
		Data       []byte
		AccessList types.AccessList
	}
	if gethrlp.DecodeBytes(raw[1:], &tx) != nil || tx.To == nil || tx.Value == nil {
		return false
	}
	if tx.Value.Sign() > 0 {
		return *tx.To == successor && len(tx.Data) == 0
	}
	return len(tx.Data) == 68 && bytes.Equal(tx.Data[:4], []byte{0xa9, 0x05, 0x9c, 0xbb}) &&
		bytes.Equal(tx.Data[4:36], common.LeftPadBytes(successor.Bytes(), 32))
}
