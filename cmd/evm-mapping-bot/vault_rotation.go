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
	"regexp"
	"sort"
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
	// InactiveHeight: the L1 height the generation went Inactive (its purge
	// grace anchor); 0 while it is not Inactive.
	InactiveHeight uint32
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
		g := vaultGen{Gen: v.Generation, Status: v.Status, InactiveHeight: v.InactiveHeight,
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

// fetchVaultView reads "v" hex-encoded, as decodeVaultView takes it
// (fetchContractStateHex would hand it the decoded bytes).
func fetchVaultView(ctx context.Context, gql *vscGraphQL, contract string) (vaultView, error) {
	st, err := gql.fetchContractStateEnc(ctx, contract, []string{"v"}, "hex")
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
	// I-36: a call no endpoint answered is retried (callRetryNoAnswer); one
	// cost a B6 custody check for 10 min (testnet 11-03..05: 23 of 156 rounds
	// skipped whole, 13 more their USDC part). An answer (an endpoint that
	// cannot serve it) stays final.
	res, err := callRetryNoAnswer(ctx, rpc, "eth_getProof", fmt.Sprintf(`"%s", [], "0x%x"`, address, height))
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

// fetchStorageProofs returns a contract's account proof and one storage proof
// per key (base64 concatenated nodes, in key order) at height. An empty
// storage trie has no nodes: each proof is "".
func fetchStorageProofs(ctx context.Context, rpc *ethRPC, address string, keys []string, height uint64) (accountProof, []string, error) {
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = `"` + k + `"`
	}
	res, err := callRetryNoAnswer(ctx, rpc, "eth_getProof", fmt.Sprintf(`"%s", [%s], "0x%x"`, address, strings.Join(quoted, ","), height)) // I-36
	if err != nil {
		return accountProof{}, nil, fmt.Errorf("eth_getProof %s at %d: %w", address, height, err)
	}
	var p struct {
		AccountProof []string `json:"accountProof"`
		StorageProof []struct {
			Key   string   `json:"key"`
			Proof []string `json:"proof"`
		} `json:"storageProof"`
	}
	if err := json.Unmarshal(res, &p); err != nil {
		return accountProof{}, nil, fmt.Errorf("parse eth_getProof: %w", err)
	}
	if len(p.StorageProof) != len(keys) {
		return accountProof{}, nil, fmt.Errorf("eth_getProof %s: %d storage proofs for %d keys", address, len(p.StorageProof), len(keys))
	}
	join := func(nodes []string) string {
		var b []byte
		for _, n := range nodes {
			b = append(b, hexToBytes(n)...)
		}
		return base64.StdEncoding.EncodeToString(b)
	}
	sps := make([]string, len(keys))
	for i, sp := range p.StorageProof {
		if new(big.Int).SetBytes(hexToBytes(padHex(sp.Key))).Cmp(new(big.Int).SetBytes(hexToBytes(padHex(keys[i])))) != 0 {
			return accountProof{}, nil, fmt.Errorf("eth_getProof %s: storage proof %d is for key %s, asked %s", address, i, sp.Key, keys[i])
		}
		sps[i] = join(sp.Proof)
	}
	return accountProof{BlockHeight: height, Proof: join(p.AccountProof)}, sps, nil
}

// padHex makes a hex quantity even-length for hexToBytes ("0x1" -> "01").
func padHex(s string) string {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	return s
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
	case vaultOpNoop(action, sim.Ret):
		return sim.Ret, errVaultOpNoop
	default:
		rc = rcFromDryRun(sim.RcUsed, vaultOpRcLimit)
	}
	if _, err := sub.callContractL2(ctx, contract, action, payload, rc); err != nil {
		noteRotationRC(action, err)
		return "", err
	}
	noteRotationRC(action, nil)
	return sim.Ret, nil
}

// rotationRCShort is the last vault op the node refused because the bot had
// less RC than the op declares (I-20), zero once a vault op goes through.
var rotationRCShort struct {
	sync.Mutex
	action     string
	have, need int64
	at         time.Time
}

// notEnoughRC matches the node's refusal "not enough RCS available: A < B"
// (transaction-pool.go).
var notEnoughRC = regexp.MustCompile(`not enough RCS available: (\d+) < (\d+)`)

// noteRotationRC records (or, after a vault op went through, clears) an RC
// refusal of a rotation op. I-20, testnet 11-04 07:19-07:27: migrateVault was
// refused "not enough RCS available: 13059 < 41914" for as long as the bot's RC
// stayed under 41,914 (projected ~12 h at its net refill), and the only alarm
// was B11's "below 20000". The health check now names the op and what it needs.
func noteRotationRC(action string, err error) {
	rotationRCShort.Lock()
	defer rotationRCShort.Unlock()
	if err == nil {
		rotationRCShort.action, rotationRCShort.have, rotationRCShort.need, rotationRCShort.at = "", 0, 0, time.Time{}
		return
	}
	m := notEnoughRC.FindStringSubmatch(err.Error())
	if m == nil {
		return
	}
	have, _ := strconv.ParseInt(m[1], 10, 64)
	need, _ := strconv.ParseInt(m[2], 10, 64)
	rotationRCShort.action, rotationRCShort.have, rotationRCShort.need, rotationRCShort.at = action, have, need, vaultOpNow()
	slog.Error("I-20: a vault rotation op is refused for RC: the rotation waits until the bot holds what it needs (fund the bot's L2 identity)",
		"action", action, "rc_available", have, "rc_needed", need)
}

// rotationRCAlarmFor: a refusal this recent is still reported (the driver
// retries a refused op every vaultOpMinInterval, which refreshes it).
const rotationRCAlarmFor = 15 * time.Minute

// rotationRCAlarm is the health alarm for a recent RC refusal of a rotation
// op, or "".
func rotationRCAlarm(now time.Time) string {
	rotationRCShort.Lock()
	defer rotationRCShort.Unlock()
	if rotationRCShort.action == "" || now.Sub(rotationRCShort.at) > rotationRCAlarmFor {
		return ""
	}
	return fmt.Sprintf("I-20: %s is refused for RC (%d available, %d needed, at %s): the vault rotation waits; fund the bot's L2 identity",
		rotationRCShort.action, rotationRCShort.have, rotationRCShort.need, rotationRCShort.at.Format(time.RFC3339))
}

// errVaultOpNoop: the dry run succeeds but the op would change nothing, so
// nothing is submitted (BTC bot BOT-RETIRE-1 and F8). A superseded generation
// waits out the purge grace (216,000 L1 blocks on mainnet, about 30 days)
// while retireVault and migrateVault succeed as no-ops; submitted every
// vaultOpMinInterval they cost about 865 + 1,000 RC every 2 minutes (devnet
// FS6 11-03: two no-op retireVault and a no-op migrateVault confirmed in one
// rotation), far past what a bot regenerates, so its real calls would starve.
var errVaultOpNoop = errors.New("the vault op would change nothing yet")

// vaultOpNoop reports a dry-run result that changes nothing (the bridge's own
// return strings: vault_retire.go, migration.go).
func vaultOpNoop(action, ret string) bool {
	switch action {
	case "retireVault":
		return ret == "retire: no generation transitions"
	case "migrateVault":
		return ret == "nothing to migrate"
	case "writeOffDust":
		return ret == "no dust to write off"
	case "attestDepositContract":
		return strings.HasSuffix(ret, "already attested")
	}
	return false
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
	// notOperatorAt: when the bridge last refused an operator op because this
	// bot is not its vault operator (zero: not latched).
	notOperatorAt time.Time
	vaultOpNow    = time.Now // tests set it
)

// notOperatorRecheck is how long the not-operator latch holds before the
// driver tries again (BTC bot BOT-LATCH-1, mapper/vault_rotation.go
// notOwnerRecheck: a latch for the life of the process left a bot appointed
// operator after it started idle until someone restarted it). The ops are dry
// run first, so a re-check refused again costs no RC.
const notOperatorRecheck = 10 * time.Minute

// notOperatorLatched reports whether the latch is holding. Caller holds
// vaultOpMu.
func notOperatorLatched() bool {
	return !notOperatorAt.IsZero() && vaultOpNow().Sub(notOperatorAt) < notOperatorRecheck
}

func vaultOpDue(kind string) bool {
	vaultOpMu.Lock()
	defer vaultOpMu.Unlock()
	if notOperatorLatched() {
		return false
	}
	return vaultOpNow().Sub(lastVaultOpAt[kind]) >= vaultOpMinInterval
}

func noteVaultOp(kind string) {
	vaultOpMu.Lock()
	lastVaultOpAt[kind] = vaultOpNow()
	vaultOpMu.Unlock()
}

// noteRefusal latches "this bot is not the vault operator" for
// notOperatorRecheck: until the owner appoints it no operator op can succeed,
// so the driver stops issuing them and tries again after the latch (BTC bot
// notOwner + BOT-LATCH-1). Reported once per latch, not once per block.
func noteRefusal(err error) {
	if err != nil && strings.Contains(err.Error(), "owner or the vault operator") {
		vaultOpMu.Lock()
		if !notOperatorLatched() {
			slog.Error("this bot is not the bridge's vault operator: rotation ops stop, re-checked every 10 min (setVaultOperator to this DID)")
		}
		notOperatorAt = vaultOpNow()
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
	attestDepositContracts(ctx, rpc, gql, sub, cfg, view) // I-28: depositAddress() names a MagiDeposit only once attested
	sweeps, err := fetchOpenSweeps(ctx, gql, cfg.ContractID)
	if err != nil {
		slog.Debug("open sweeps unreadable", "err", err)
		return
	}
	open := make([]string, 0, len(sweeps))
	for _, r := range sweeps {
		open = append(open, sweepKey(r))
		noteSweepVersions(sweepKey(r), len(r.Versions))
		driveSweep(ctx, rpc, gql, sub, cp, cfg, view, r, r.Nonce == lowestOpenNonce(sweeps, r.Gen))
	}
	pruneStuckSweeps(open)
	superseded := false
	for _, g := range view.Gens {
		switch g.Status {
		case btcvault.VaultStatusRetiring, btcvault.VaultStatusDraining, btcvault.VaultStatusInactive:
			superseded = true
		}
	}
	notePurgeWait(ctx, gql, cp, cfg, view) // I-38: a purge that waits only for the stored height asks for a proof
	if !superseded {
		return
	}
	if len(sweeps) == 0 && vaultOpDue("migrate") {
		migrateNext(ctx, rpc, gql, sub, cp, cfg, view)
	}
	if vaultOpDue("retire") {
		if why := retireWait(ctx, gql, cp, cfg, view); why != "" {
			slog.Info("I-26: retireVault waits", "reason", why)
			return
		}
		noteVaultOp("retire")
		ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "retireVault", json.RawMessage(`{}`))
		noteRefusal(err)
		if err == nil && ret != "retire: no generation transitions" {
			slog.Info("vault retire submitted", "dry_run", ret)
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
	if errors.Is(err, errVaultOpNoop) {
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
	if errors.Is(err, errVaultOpNoop) {
		return
	}
	if err != nil {
		slog.Warn("migrateVault (ETH)", "err", err)
		return
	}
	logMigrate(ret)
	if strings.Contains(ret, "dust") {
		writeOffDust(ctx, gql, sub, cfg)
	}
}

// logMigrate logs a submitted migrateVault by its dry run's answer: the call
// itself runs a block or more later, on the state of that moment.
func logMigrate(ret string) {
	if strings.HasPrefix(ret, "sweep ") {
		slog.Info("migration sweep submitted", "dry_run", ret)
		return
	}
	slog.Info("migrateVault: nothing built", "dry_run", ret)
}

func writeOffDust(ctx context.Context, gql *vscGraphQL, sub *l2Submitter, cfg EVMBotConfig) {
	ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "writeOffDust", json.RawMessage(`{}`))
	noteRefusal(err)
	if err == nil {
		slog.Info("dust write-off submitted", "dry_run", ret)
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
	TxHashes        []string `json:"tx_hashes"`
	SentAt          int64    `json:"sent_at"`
	LastBroadcastAt int64    `json:"last_broadcast_at,omitempty"`
	ConfirmTxId     string   `json:"confirm_tx_id,omitempty"`
	NextTryAt       int64    `json:"next_try_at,omitempty"`
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

	// Broadcast each signed version not yet sent; while none has mined, re-post
	// them every rebroadcastInterval, as an unmined withdrawal (B8): a shallow
	// reorg or a mempool that dropped the tx would otherwise leave the sweep
	// waiting for a re-drive (32 L1 blocks and a new signature).
	repost := len(ss.TxHashes) > 0 && time.Since(time.Unix(ss.LastBroadcastAt, 0)) >= rebroadcastInterval && !sweepMined(rpc, ss.TxHashes)
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
		if known := containsStr(ss.TxHashes, hash); !known || repost {
			if _, err := rpc.broadcastTx(signed); err != nil && !strings.Contains(strings.ToLower(err.Error()), "known") {
				slog.Warn("sweep broadcast failed", "sweep", key, "err", err)
				continue
			}
			if !known {
				ss.TxHashes = append(ss.TxHashes, hash)
			}
			if ss.SentAt == 0 {
				ss.SentAt = time.Now().Unix()
			}
			ss.LastBroadcastAt = time.Now().Unix()
			save()
			slog.Info("sweep broadcast", "sweep", key, "asset", r.Asset, "tx", hash, "repost", known)
		}
	}

	// Mined? Confirm once final and its header is stored.
	unread := false // PS-2: a receipt that could not be read this cycle
	for _, h := range ss.TxHashes {
		rc, err := rpc.getReceipt(h)
		if err != nil {
			// a null result is the RPC's "no receipt" (not mined); any other
			// error leaves the version's state unknown
			if !strings.Contains(err.Error(), "null result from") {
				unread = true
			}
			continue
		}
		if rc == nil || string(rc) == "null" {
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

	// Not mined: re-drive once the contract allows it. PS-2 (BTC bot parity,
	// mapper/vault_rotation.go: an unreadable status skips the cycle): only
	// when every version's receipt read answered "none"; a version that may
	// have mined is never re-driven (the new version could never mine, and
	// its TSS signature, RC and booked gas would be spent for nothing).
	if unread {
		slog.Debug("sweep receipt unreadable; not re-driving this cycle", "sweep", key)
		return
	}
	if redrive && len(ss.TxHashes) > 0 && vaultOpDue("redrive-"+key) {
		if h, err := fetchContractLastHeight(ctx, gql, cfg.ContractID); err == nil && h >= r.BuiltL1+sweepRedriveL1Blocks {
			noteVaultOp("redrive-" + key)
			payload, _ := json.Marshal(map[string]any{"gen": r.Gen, "nonce": r.Nonce})
			ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "redriveSweep", payload)
			noteRefusal(err)
			noteSweepRedrive(key, err)
			if err == nil {
				slog.Info("sweep re-drive submitted", "sweep", key, "dry_run", ret)
			}
		}
	}
}

// PS-1 (11-04): a sweep that stays unmined for a reason a higher fee cannot
// fix (its signature, a gap in the vault's L1 nonces) is re-driven every
// sweepRedriveL1Blocks, a version per re-drive, until the bridge refuses past
// MaxSweepVersions; then the bot dry-runs the refused re-drive every
// vaultOpMinInterval and nothing else happens. Neither showed anywhere but the
// log. /health names a sweep with stuckSweepVersions versions or more (eight
// versions is about 264 L1 blocks unmined) and a sweep whose re-drive the
// bridge refused, until it is confirmed.
const stuckSweepVersions = 8

var stuckSweeps = struct {
	sync.Mutex
	versions map[string]int
	refused  map[string]string
}{versions: map[string]int{}, refused: map[string]string{}}

func sweepKey(r sweepRecord) string {
	return strconv.FormatUint(uint64(r.Gen), 10) + "-" + strconv.FormatUint(r.Nonce, 10)
}

func noteSweepVersions(key string, n int) {
	stuckSweeps.Lock()
	stuckSweeps.versions[key] = n
	stuckSweeps.Unlock()
}

// noteSweepRedrive records the outcome of a re-drive: a dry-run refusal is kept
// (with the bridge's reason), anything else clears it.
func noteSweepRedrive(key string, err error) {
	stuckSweeps.Lock()
	defer stuckSweeps.Unlock()
	if err != nil && errors.Is(err, errDryRunRefused) {
		stuckSweeps.refused[key] = err.Error()
		return
	}
	delete(stuckSweeps.refused, key)
}

// pruneStuckSweeps forgets sweeps that are no longer open (confirmed).
func pruneStuckSweeps(open []string) {
	stuckSweeps.Lock()
	defer stuckSweeps.Unlock()
	for k := range stuckSweeps.versions {
		if !containsStr(open, k) {
			delete(stuckSweeps.versions, k)
			delete(stuckSweeps.refused, k)
		}
	}
	for k := range stuckSweeps.refused {
		if !containsStr(open, k) {
			delete(stuckSweeps.refused, k)
		}
	}
}

// stuckSweepAlarms is the PS-1 /health text, sorted by sweep key.
func stuckSweepAlarms() []string {
	stuckSweeps.Lock()
	defer stuckSweeps.Unlock()
	keys := make([]string, 0, len(stuckSweeps.versions)+len(stuckSweeps.refused))
	seen := map[string]bool{}
	for k := range stuckSweeps.versions {
		keys, seen[k] = append(keys, k), true
	}
	for k := range stuckSweeps.refused {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if msg, ok := stuckSweeps.refused[k]; ok {
			out = append(out, fmt.Sprintf("PS-1: sweep %s cannot be re-driven (%s): it is unmined and no higher fee will fix it; check its signature and the vault's L1 nonce", k, msg))
		} else if n := stuckSweeps.versions[k]; n >= stuckSweepVersions {
			out = append(out, fmt.Sprintf("PS-1: sweep %s is on its version %d (re-driven %d times without mining): check its signature and the vault's L1 nonce", k, n, n-1))
		}
	}
	return out
}

// sweepMined reports whether any of hashes has a receipt.
func sweepMined(rpc *ethRPC, hashes []string) bool {
	for _, h := range hashes {
		if rc, err := rpc.getReceipt(h); err == nil && rc != nil && string(rc) != "null" {
			return true
		}
	}
	return false
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

// depositVaults is every generation's address whose MagiDeposit the bridge
// names or credits: the fund-holding ones, and a pending one only once the
// registry holds its key (the bridge records it at activation). For a pending
// generation before activation see depositVaultsWithPending.
func (c EVMBotConfig) depositVaults() []string {
	var out []string
	for _, g := range c.VaultGens {
		if g.Address != "" && (btcvault.IsFundHoldingStatus(g.Status) || g.Status == btcvault.VaultStatusPending) {
			out = append(out, g.Address)
		}
	}
	if len(out) == 0 && c.VaultAddress != "" {
		return []string{c.VaultAddress}
	}
	return out
}

// depositVaultsWithPending is depositVaults plus each pending generation the
// registry does not yet give an address, at its TSS key's address (as A18
// derives a vault from its key). The bridge records a generation's key only at
// activation (vault_lifecycle.go), so without this the I-28 alarm could name an
// undeployed MagiDeposit only once depositAddress() already named it to
// depositors; this names it while the generation waits to activate, in time to
// deploy it first (live 11-04: gen 2 keyed and pending, registry address "",
// the check saw only gen 1). A pending generation with no TSS key yet adds
// nothing.
func (c EVMBotConfig) depositVaultsWithPending(ctx context.Context, gql *vscGraphQL) []string {
	out := c.depositVaults()
	for _, g := range c.VaultGens {
		if g.Status != btcvault.VaultStatusPending || g.Address != "" || g.KeyID == "" {
			continue
		}
		if addr, _, _, ok, err := fetchTssVault(ctx, gql, g.KeyID); err == nil && ok && addr != "" {
			out = append(out, strings.ToLower(addr))
		}
	}
	return out
}

// healthKeys is the TSS key of every generation that holds or may receive
// funds, or waits to activate (A01), else the one key.
func (c EVMBotConfig) healthKeys(fallback string) []string {
	var keys []string
	for _, g := range c.VaultGens {
		if g.Address != "" && (btcvault.IsFundHoldingStatus(g.Status) || g.Status == btcvault.VaultStatusPending) {
			keys = append(keys, g.KeyID)
		}
	}
	if len(keys) == 0 {
		return []string{fallback}
	}
	return keys
}

// retireWait says why retireVault must not go out this tick, or "" (I-26,
// testnet 11-04). retireVault purges an Inactive generation once the bridge's
// stored L1 height is past its grace, and a purged generation's deposits are
// refused ("transaction is not a deposit to vault"). Live: a 0.02 ETH deposit
// to the Inactive gen 0 sat parked for its header (RP-9); the proof that let
// the header be attested also crossed the grace, retireVault went out at once
// (09:10:12), the map 39 s later was refused and the ETH was stranded at an
// address the bridge never signs from again. So while a generation is
// Inactive the bot does not retire while a deposit it found to one is parked
// or its map is unresolved, or while its scan is behind the stored L1 height
// (blocks the contract counts that the bot has not looked at). A deposit the
// bot cannot see yet (not finalized) is not covered here; the contract's
// purge check is the fix for that. An unreadable stored height waits.
func retireWait(ctx context.Context, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig, view vaultView) string {
	inactive := map[string]bool{}
	for _, g := range view.Gens {
		if g.Status == btcvault.VaultStatusInactive && g.Address != "" {
			inactive[strings.ToLower(g.Address)] = true
		}
	}
	if len(inactive) == 0 {
		return "" // nothing this call can purge
	}
	// an entry from before the Vault field may be to an Inactive generation
	toInactive := func(vault string) bool { return vault == "" || inactive[strings.ToLower(vault)] }
	cp.mu.Lock()
	scanned := cp.LastScannedBlock
	var why string
	for h, deps := range cp.WaitingDeposits {
		for _, d := range deps {
			if toInactive(d.Vault) {
				why = fmt.Sprintf("deposit %s (block %d) to an Inactive generation is not mapped yet", d.TxHash, h)
			}
		}
	}
	for id, pm := range cp.PendingMaps {
		if why == "" && toInactive(pm.Vault) {
			why = fmt.Sprintf("map %s of deposit %s (block %d) to an Inactive generation has no result yet", id, pm.DepositTx, pm.Block)
		}
	}
	cp.mu.Unlock()
	if why != "" {
		return why
	}
	_, vst, err := verifierState(ctx, gql, cfg.ContractID, []string{"h"})
	if err != nil {
		return "stored L1 height unreadable: " + err.Error()
	}
	stored, err := strconv.ParseUint(vst["h"], 10, 64)
	if err != nil {
		return fmt.Sprintf("stored L1 height %q unreadable", vst["h"])
	}
	if scanned < stored {
		return fmt.Sprintf("deposit scan at block %d, behind the stored L1 height %d", scanned, stored)
	}
	return ""
}

// purgeWaitBlocks mirrors the bridge's purge wait past a generation's Inactive
// height: VaultPurgeGraceL1Blocks + VaultPurgeMapWindowL1Blocks (I-26): about
// 30 days + a day on mainnet, 64 + 64 L1 blocks elsewhere. A bridge without the
// map window purges 64 blocks sooner; asking for its proof later is harmless.
func purgeWaitBlocks(chainID uint64) uint64 {
	if chainID == 1 {
		return 216_000 + 7_200
	}
	return 64 + 64
}

// bridgeChainID is the bridge's L1 chain id (state "chainid", the contract's
// default 1), read once.
var bridgeChainID struct {
	sync.Mutex
	id uint64
}

func readBridgeChainID(ctx context.Context, gql *vscGraphQL, contract string) uint64 {
	bridgeChainID.Lock()
	defer bridgeChainID.Unlock()
	if bridgeChainID.id != 0 {
		return bridgeChainID.id
	}
	st, err := gql.fetchContractState(ctx, contract, []string{"chainid"})
	if err != nil {
		return 0
	}
	id, _ := strconv.ParseUint(st["chainid"], 10, 64)
	if id == 0 {
		id = 1
	}
	bridgeChainID.id = id
	return id
}

// notePurgeWait is I-38 (testnet 11-04/05): on a quiet bridge the last purge of
// a rotation waits for the stored L1 height to pass the grace, and nothing asks
// for the proof that moves it (BTC never waits: its headers are relayed all the
// time; EVM proofs are on demand). Once the bot has scanned past the purge
// height of an Inactive generation, cp.PurgeWantTip asks the operator for a
// proof (writeNeedsFile) until the stored height reaches it: about one paid
// proof per rotation, and none while L1 itself is not that far yet.
func notePurgeWait(ctx context.Context, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig, view vaultView) {
	var want uint64
	chainID := uint64(0)
	for _, g := range view.Gens {
		if g.Status != btcvault.VaultStatusInactive || g.InactiveHeight == 0 {
			continue
		}
		if chainID == 0 {
			if chainID = readBridgeChainID(ctx, gql, cfg.ContractID); chainID == 0 {
				return
			}
		}
		at := uint64(g.InactiveHeight) + purgeWaitBlocks(chainID)
		cp.mu.Lock()
		scanned := cp.LastScannedBlock
		cp.mu.Unlock()
		if scanned >= at && at > want {
			want = at
		}
	}
	cp.mu.Lock()
	cp.PurgeWantTip = want
	cp.mu.Unlock()
}
