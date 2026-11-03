package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/btcvault"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// B6 — the bot reports what the bridge's own checks need proven: a vault
// transaction the bridge never authorized (reportUnauthorizedSpend, the theft
// halt "th") and the custody of every fund-holding generation: its ETH
// (proveCustody) and each token with a custody setting (proveTokenRoot, then
// proveTokenCustody per generation: the token contract's storage root, then
// each generation's balance word). A short asset sets its flag "sis-{asset}"
// and the halt "si". All are permissionless; the BTC bot's counterpart is its
// unauthorized-spend reporter. Each round (every REPORTER_EVERY, default 10m,
// "0" off) the bot proves every fund-holding address at the bridge's newest
// stored header and dry runs the reports; it sends one only when the dry run
// says it changes something: a theft report the bridge accepts, or an asset's
// custody verdict that differs from that asset's flag (short while clear sets
// it, held while set lifts it). A healthy bridge costs no RC. An RPC that
// cannot serve eth_getProof at that header (a pruned node, the header older
// than its window) skips the round (a token's failure skips only that token).

var (
	reporterEvery = 10 * time.Minute
	reporterMu    sync.Mutex
	lastReportAt  time.Time
	reporterNow   = time.Now // tests set it
)

func init() {
	if v := os.Getenv("REPORTER_EVERY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			reporterEvery = d
		}
	}
}

// reporterDue reports whether a round is due and, if so, starts it.
func reporterDue() bool {
	reporterMu.Lock()
	defer reporterMu.Unlock()
	if reporterEvery == 0 {
		return false
	}
	now := reporterNow()
	if !lastReportAt.IsZero() && now.Sub(lastReportAt) < reporterEvery {
		return false
	}
	lastReportAt = now
	return true
}

type l2Call struct {
	Action  string
	Payload json.RawMessage
}

// reporterDeps are a round's reads and writes, injectable for tests.
type reporterDeps struct {
	gens   func() ([]vaultGen, error)
	header func() (uint64, error)
	proof  func(address string, height uint64) (accountProof, error)
	// tokens: the registered tokens with a custody setting (nil: none).
	tokens func() ([]tokenCustody, error)
	// flags: "th" and each asset's short flag (symbols: the tokens above).
	flags func(symbols []string) (reporterFlags, error)
	// storage: eth_getProof of a token contract with storage keys.
	storage  func(token string, slotKeys []string, height uint64) (accountProof, []string, error)
	simulate func(calls []l2Call) ([]simulatedCall, error)
	submit   func(c l2Call, rcUsed int64) error
}

// tokenCustody is a token the bridge can prove custody of: where its contract
// keeps a holder's balance (the bridge's "tcc-{symbol}").
type tokenCustody struct {
	Symbol  string
	Address string // "0x..."
	Slot    uint64
}

// reporterFlags: the theft halt and which assets the bridge holds short.
type reporterFlags struct {
	theft bool
	short map[string]bool
}

// parseReporterFlags reads "th", "si" and each asset's "sis-{asset}". A bridge
// updated from the ETH-only custody proof may hold "si" with no asset flag:
// only ETH could have set it (the bridge reads it the same way).
func parseReporterFlags(st map[string]string, symbols []string) reporterFlags {
	f := reporterFlags{theft: st["th"] != "", short: map[string]bool{}}
	found := false
	for _, a := range append([]string{"eth"}, symbols...) {
		f.short[a] = st["sis-"+a] != ""
		found = found || f.short[a]
	}
	if !found && st["si"] != "" {
		f.short["eth"] = true
	}
	return f
}

// flagKeys are the state keys parseReporterFlags reads.
func flagKeys(symbols []string) []string {
	keys := []string{"th", "si", "sis-eth"}
	for _, s := range symbols {
		keys = append(keys, "sis-"+s)
	}
	return keys
}

// parseTokenCustody reads each symbol's "tcc-{symbol}" ("{slot}|{bits}|{gap}")
// and "ts-{symbol}" ("{chainId}-{address hex}"); a token without a setting
// is skipped.
func parseTokenCustody(st map[string]string, symbols []string) []tokenCustody {
	var out []tokenCustody
	for _, s := range symbols {
		cfg, ref := st["tcc-"+s], st["ts-"+s]
		f := strings.Split(cfg, "|")
		i := strings.LastIndex(ref, "-")
		if len(f) != 3 || i < 0 || len(ref)-i-1 != 40 {
			continue
		}
		slot, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, tokenCustody{Symbol: s, Address: "0x" + strings.ToLower(ref[i+1:]), Slot: slot})
	}
	return out
}

// tokenSlotKey is the storage key of holder's entry in a mapping at slot,
// keccak256(pad32(holder) ++ pad32(slot)), as eth_getProof takes it.
func tokenSlotKey(holder string, slot uint64) string {
	buf := make([]byte, 64)
	copy(buf[12:32], hexToBytes(holder))
	new(big.Int).SetUint64(slot).FillBytes(buf[32:64])
	return "0x" + hex.EncodeToString(ethCrypto.Keccak256(buf))
}

func genProofPayload(gen uint32, p accountProof) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"gen": gen, "proof": p})
	return raw
}

// runReporter is one round; it returns what it did.
func runReporter(d reporterDeps) (string, error) {
	gens, err := d.gens()
	if err != nil {
		return "", err
	}
	if len(gens) == 0 {
		return "no fund-holding generation", nil
	}
	h, err := d.header()
	if err != nil {
		return "", fmt.Errorf("newest stored header: %w", err)
	}
	var tokens []tokenCustody
	if d.tokens != nil {
		if tokens, err = d.tokens(); err != nil {
			return "", fmt.Errorf("token custody settings: %w", err)
		}
	}
	symbols := make([]string, len(tokens))
	for i, tk := range tokens {
		symbols[i] = tk.Symbol
	}
	fl, err := d.flags(symbols)
	if err != nil {
		return "", fmt.Errorf("halt flags: %w", err)
	}
	calls := make([]l2Call, 0, len(gens))
	for _, g := range gens {
		p, err := d.proof(g.Address, h)
		if err != nil {
			return "", fmt.Errorf("account proof of generation %d at %d: %w", g.Gen, h, err)
		}
		calls = append(calls, l2Call{Action: "proveCustody", Payload: genProofPayload(g.Gen, p)})
	}
	if !fl.theft {
		for i, g := range gens {
			c := l2Call{Action: "reportUnauthorizedSpend", Payload: calls[i].Payload}
			sims, err := d.simulate([]l2Call{c})
			if err != nil {
				return "", err
			}
			if len(sims) == 1 && sims[0].Success {
				if err := d.submit(c, sims[0].RcUsed); err != nil {
					return "", err
				}
				return fmt.Sprintf("theft reported: generation %d at block %d", g.Gen, h), nil
			}
		}
	}
	out, err := custodyRound(d, "", calls, fl.short["eth"])
	if err != nil {
		return "", err
	}
	for _, tk := range tokens {
		o, err := tokenCustodyRound(d, tk, gens, h, fl.short[tk.Symbol])
		if err != nil {
			o = tk.Symbol + " custody round skipped: " + err.Error()
		}
		out += "; " + o
	}
	return out, nil
}

// custodyRound dry runs one asset's custody calls and sends them only when
// the verdict differs from the asset's flag. tag prefixes a token's outcome.
func custodyRound(d reporterDeps, tag string, calls []l2Call, short bool) (string, error) {
	sims, err := d.simulate(calls)
	if err != nil {
		return "", err
	}
	if len(sims) != len(calls) {
		return "", fmt.Errorf("custody dry run: %d results for %d calls", len(sims), len(calls))
	}
	last := sims[len(sims)-1]
	if !last.Success {
		return tag + "custody proof not accepted now: " + last.ErrMsg, nil
	}
	isShort := strings.Contains(last.Ret, "custody short")
	if isShort == short {
		return tag + "nothing to report: " + last.Ret, nil
	}
	for i, c := range calls {
		if err := d.submit(c, sims[i].RcUsed); err != nil {
			return "", err
		}
	}
	if isShort {
		return tag + "custody short reported: " + last.Ret, nil
	}
	return tag + "custody restored reported: " + last.Ret, nil
}

// tokenCustodyRound proves a token's custody at h: its contract's storage
// root, then each generation's balance word.
func tokenCustodyRound(d reporterDeps, tk tokenCustody, gens []vaultGen, h uint64, short bool) (string, error) {
	if d.storage == nil {
		return "", fmt.Errorf("no storage proof source")
	}
	keys := make([]string, len(gens))
	for i, g := range gens {
		keys[i] = tokenSlotKey(g.Address, tk.Slot)
	}
	acct, sps, err := d.storage(tk.Address, keys, h)
	if err != nil {
		return "", fmt.Errorf("storage proof of %s at %d: %w", tk.Address, h, err)
	}
	if len(sps) != len(gens) {
		return "", fmt.Errorf("storage proof of %s: %d proofs for %d keys", tk.Address, len(sps), len(gens))
	}
	return custodyRound(d, tk.Symbol+" ", tokenCustodyCalls(tk, gens, h, acct, sps), short)
}

// tokenCustodyCalls: the token's storage root, then each generation's
// balance word (sps in gens order), all at h.
func tokenCustodyCalls(tk tokenCustody, gens []vaultGen, h uint64, acct accountProof, sps []string) []l2Call {
	root, _ := json.Marshal(map[string]any{"asset": tk.Symbol, "proof": acct})
	calls := []l2Call{{Action: "proveTokenRoot", Payload: root}}
	for i, g := range gens {
		p, _ := json.Marshal(map[string]any{"asset": tk.Symbol, "gen": g.Gen, "block_height": h, "storage_proof": sps[i]})
		calls = append(calls, l2Call{Action: "proveTokenCustody", Payload: p})
	}
	return calls
}

// fundHoldingGens is the view's fund-holding generations with an address; a
// pre-rotation bridge's one vault is generation 0.
func fundHoldingGens(view vaultView, cfg EVMBotConfig) []vaultGen {
	if !view.Registry {
		if cfg.VaultAddress == "" {
			return nil
		}
		return []vaultGen{{Gen: 0, Status: btcvault.VaultStatusActive, Address: strings.ToLower(cfg.VaultAddress)}}
	}
	var out []vaultGen
	for _, g := range view.Gens {
		if g.Address != "" && btcvault.IsFundHoldingStatus(g.Status) {
			out = append(out, g)
		}
	}
	return out
}

// simulateContractCalls dry runs calls in sequence (each sees the ones before).
func (g *vscGraphQL) simulateContractCalls(ctx context.Context, caller, contract string, calls []l2Call) ([]simulatedCall, error) {
	in := make([]map[string]interface{}, len(calls))
	for i, c := range calls {
		in[i] = map[string]interface{}{"contract_id": contract, "action": c.Action, "payload": string(c.Payload), "rc_limit": simulateRcCap}
	}
	data, err := g.query(ctx,
		`query($i: SimulateContractCallsInput!){ simulateContractCalls(input: $i){ success err err_msg ret rc_used } }`,
		map[string]interface{}{"i": map[string]interface{}{
			"tx_id":          fmt.Sprintf("evm-bot-report-%d", time.Now().UnixNano()),
			"required_auths": []string{caller},
			"calls":          in,
		}})
	if err != nil {
		return nil, err
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
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	res := make([]simulatedCall, len(out.Sim))
	for i, r := range out.Sim {
		res[i] = simulatedCall{Success: r.Success, RcUsed: r.RcUsed}
		if r.Err != nil {
			res[i].Err = *r.Err
		}
		if r.ErrMsg != nil {
			res[i].ErrMsg = *r.ErrMsg
		}
		if r.Ret != nil {
			res[i].Ret = *r.Ret
		}
	}
	return res, nil
}

// driveReporterSafe runs a due round with the bot's own reads and submitter.
func driveReporterSafe(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cfg EVMBotConfig, view vaultView) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("B6 reporter panicked", "panic", r)
		}
	}()
	if sub == nil || !reporterDue() {
		return
	}
	d := reporterDeps{
		gens:   func() ([]vaultGen, error) { return fundHoldingGens(view, cfg), nil },
		header: func() (uint64, error) { return fetchContractLastHeight(ctx, gql, cfg.ContractID) },
		proof: func(address string, h uint64) (accountProof, error) {
			p, _, _, err := fetchAccountProof(ctx, rpc, address, h)
			return p, err
		},
		tokens: func() ([]tokenCustody, error) {
			st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"tsyms"})
			if err != nil {
				return nil, err
			}
			var syms, keys []string
			for _, s := range strings.Split(st["tsyms"], "|") {
				if s != "" {
					syms = append(syms, s)
					keys = append(keys, "tcc-"+s, "ts-"+s)
				}
			}
			if len(syms) == 0 {
				return nil, nil
			}
			if st, err = gql.fetchContractState(ctx, cfg.ContractID, keys); err != nil {
				return nil, err
			}
			return parseTokenCustody(st, syms), nil
		},
		flags: func(symbols []string) (reporterFlags, error) {
			st, err := gql.fetchContractState(ctx, cfg.ContractID, flagKeys(symbols))
			if err != nil {
				return reporterFlags{}, err
			}
			return parseReporterFlags(st, symbols), nil
		},
		storage: func(token string, keys []string, h uint64) (accountProof, []string, error) {
			return fetchStorageProofs(ctx, rpc, token, keys, h)
		},
		simulate: func(calls []l2Call) ([]simulatedCall, error) {
			return gql.simulateContractCalls(ctx, sub.did.String(), cfg.ContractID, calls)
		},
		submit: func(c l2Call, rcUsed int64) error {
			_, err := sub.callContractL2(ctx, cfg.ContractID, c.Action, c.Payload, rcFromDryRun(rcUsed, vaultOpRcLimit))
			return err
		},
	}
	out, err := runReporter(d)
	switch {
	case err != nil:
		slog.Warn("B6 reporter round skipped", "err", err)
	case strings.HasPrefix(out, "theft") || strings.Contains(out, "custody short reported"):
		slog.Error("B6 reporter: "+out, "contract", cfg.ContractID)
	case strings.Contains(out, "round skipped"):
		slog.Warn("B6 reporter: "+out, "contract", cfg.ContractID)
	default:
		slog.Info("B6 reporter: "+out, "contract", cfg.ContractID)
	}
}
