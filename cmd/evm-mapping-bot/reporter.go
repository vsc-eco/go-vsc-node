package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/btcvault"
)

// B6 — the bot reports what the bridge's own checks need proven: a vault
// transaction the bridge never authorized (reportUnauthorizedSpend, the theft
// halt "th") and the ETH custody of every fund-holding generation
// (proveCustody, the custody halt "si"). Both are permissionless; the BTC
// bot's counterpart is its unauthorized-spend reporter. Each round (every
// REPORTER_EVERY, default 10m, "0" off) the bot proves every fund-holding
// address at the bridge's newest stored header and dry runs the reports; it
// sends one only when the dry run says it changes something: a theft report
// the bridge accepts, or a custody verdict that differs from the current
// "si" (short while clear sets it, held while set lifts it). A healthy bridge
// costs no RC. An RPC that cannot serve eth_getProof at that header (a pruned
// node, the header older than its window) skips the round.

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
	gens     func() ([]vaultGen, error)
	header   func() (uint64, error)
	proof    func(address string, height uint64) (accountProof, error)
	flags    func() (theft, short bool, err error)
	simulate func(calls []l2Call) ([]simulatedCall, error)
	submit   func(c l2Call, rcUsed int64) error
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
	theft, short, err := d.flags()
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
	if !theft {
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
	sims, err := d.simulate(calls)
	if err != nil {
		return "", err
	}
	if len(sims) != len(calls) {
		return "", fmt.Errorf("custody dry run: %d results for %d calls", len(sims), len(calls))
	}
	last := sims[len(sims)-1]
	if !last.Success {
		return "custody proof not accepted now: " + last.ErrMsg, nil
	}
	isShort := strings.Contains(last.Ret, "custody short")
	if isShort == short {
		return "nothing to report: " + last.Ret, nil
	}
	for i, c := range calls {
		if err := d.submit(c, sims[i].RcUsed); err != nil {
			return "", err
		}
	}
	if isShort {
		return "custody short reported: " + last.Ret, nil
	}
	return "custody restored reported: " + last.Ret, nil
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
		flags: func() (bool, bool, error) {
			st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"th", "si"})
			if err != nil {
				return false, false, err
			}
			return st["th"] != "", st["si"] != "", nil
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
	case strings.HasPrefix(out, "theft") || strings.HasPrefix(out, "custody short"):
		slog.Error("B6 reporter: "+out, "contract", cfg.ContractID)
	default:
		slog.Info("B6 reporter: "+out, "contract", cfg.ContractID)
	}
}
