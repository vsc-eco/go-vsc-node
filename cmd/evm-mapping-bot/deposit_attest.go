package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/btcvault"
)

// I-28's bot half (decided 11-05): the bridge's depositAddress() names a
// generation's MagiDeposit only once attestDepositContract has recorded, from
// an account proof at a stored header, that the CREATE2 address has code
// (MagiDeposit has no receive or flush: ETH sent to it before it exists is lost
// for good). Anyone may deploy it (the deterministic proxy is permissionless;
// the I-28 alarm names an undeployed one); the bot attests it as soon as the
// code is at the bridge's stored L1 height, for the active generation and the
// pending one (so the address is ready at activation). The op is permissionless,
// so it does not wait on the operator latch.

// depositAttestEvery bounds how often the bot looks (an eth_getCode and a state
// read per unattested generation).
var depositAttestEvery = 2 * time.Minute

var depositAttest struct {
	sync.Mutex
	at time.Time
}

// emptyCodeHashHex is keccak256 of empty code.
const emptyCodeHashHex = "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"

func depositAttestedKey(gen uint32) string { return "dca-" + strconv.FormatUint(uint64(gen), 10) }

// depositAttestCandidates are the generations whose MagiDeposit depositAddress()
// can name: the active one and a pending one with a key, at their key address.
func depositAttestCandidates(ctx context.Context, gql *vscGraphQL, view vaultView) []vaultGen {
	var out []vaultGen
	for _, g := range view.Gens {
		switch {
		case g.Status == btcvault.VaultStatusActive && g.Address != "":
			out = append(out, g)
		case g.Status == btcvault.VaultStatusPending && g.KeyID != "":
			if g.Address == "" {
				if addr, _, _, ok, err := fetchTssVault(ctx, gql, g.KeyID); err == nil && ok && addr != "" {
					g.Address = strings.ToLower(addr)
				}
			}
			if g.Address != "" {
				out = append(out, g)
			}
		}
	}
	return out
}

// codeHashAt reads addr's code hash and account proof at height (eth_getProof).
func codeHashAt(ctx context.Context, rpc *ethRPC, addr string, height uint64) (string, accountProof, error) {
	res, err := callRetryNoAnswer(ctx, rpc, "eth_getProof", fmt.Sprintf(`"%s", [], "0x%x"`, addr, height))
	if err != nil {
		return "", accountProof{}, fmt.Errorf("eth_getProof %s at %d: %w", addr, height, err)
	}
	var p struct {
		CodeHash     string   `json:"codeHash"`
		AccountProof []string `json:"accountProof"`
	}
	if err := json.Unmarshal(res, &p); err != nil {
		return "", accountProof{}, fmt.Errorf("parse eth_getProof: %w", err)
	}
	var nodes []byte
	for _, n := range p.AccountProof {
		nodes = append(nodes, hexToBytes(n)...)
	}
	return strings.ToLower(p.CodeHash), accountProof{BlockHeight: height, Proof: base64.StdEncoding.EncodeToString(nodes)}, nil
}

// EVM-ATTEST-NEED-1 (testnet 11-16 and 11-18): a new generation's MagiDeposit
// is deployed shortly before its activation, above the bridge's stored L1
// height, and can be attested only at a stored height at or above its deploy
// block. The bot asked for a proof only for deposits, withdrawals, purges and
// rotation ops on a stale pricing header, so on a quiet bridge the activation
// waited for an unrelated deposit (both nights one was sent by hand). Like the
// I-38 purge wait: once the MagiDeposit's code is at the FINALIZED block F (a
// proof of the latest finalized checkpoint then covers it; at "latest" the
// first proof could still land below the deploy block), cp.DepositAttestWantTip
// = F and writeNeedsFile asks for a proof until the stored height reaches F:
// one proof per deploy, never again once the height passed F (even if the
// attestation then fails for another reason), and no proof while the stored
// height is unreadable or the MagiDeposit is not deployed. Rebuilt on every
// pass, so a generation attested elsewhere or no longer a candidate drops out;
// a pass that could not read a candidate keeps the previous value.

// finalizedCodeBlock returns the finalized block number when addr has code at
// the finalized block, else 0 (code read first, so the number is at or above
// the block the code was seen in).
func finalizedCodeBlock(ctx context.Context, rpc *ethRPC, addr string) (uint64, error) {
	res, err := callRetryNoAnswer(ctx, rpc, "eth_getCode", fmt.Sprintf(`"%s", "finalized"`, addr))
	if err != nil {
		return 0, err
	}
	var code string
	if err := json.Unmarshal(res, &code); err != nil {
		return 0, err
	}
	if strings.Trim(strings.TrimPrefix(strings.ToLower(code), "0x"), "0") == "" {
		return 0, nil
	}
	return rpc.getFinalizedBlock()
}

// attestDepositContracts attests each candidate generation's MagiDeposit once
// its code is at the stored L1 height (at most every depositAttestEvery), and
// sets cp.DepositAttestWantTip (EVM-ATTEST-NEED-1).
func attestDepositContracts(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, sub *l2Submitter, cp *Checkpoint, cfg EVMBotConfig, view vaultView) {
	depositAttest.Lock()
	if time.Since(depositAttest.at) < depositAttestEvery {
		depositAttest.Unlock()
		return
	}
	depositAttest.at = time.Now()
	depositAttest.Unlock()

	var want uint64
	unread := false // a candidate could not be read: keep the previous want
	defer func() {
		cp.mu.Lock()
		if !unread || want > cp.DepositAttestWantTip {
			cp.DepositAttestWantTip = want
		}
		cp.mu.Unlock()
	}()
	for _, g := range depositAttestCandidates(ctx, gql, view) {
		st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{depositAttestedKey(g.Gen)})
		if err != nil {
			unread = true
			continue
		}
		if st[depositAttestedKey(g.Gen)] != "" {
			continue
		}
		dc := depositContractAddress(g.Address)
		if dc == "" {
			continue
		}
		tip, err := fetchContractLastHeight(ctx, gql, cfg.ContractID)
		if err != nil || tip == 0 {
			unread = true
			continue
		}
		codeHash, proof, err := codeHashAt(ctx, rpc, dc, tip)
		if err != nil {
			slog.Warn("I-28: cannot read the MagiDeposit's account at the stored height", "gen", g.Gen, "magideposit", dc, "height", tip, "err", err)
			unread = true
			continue
		}
		if codeHash == "" || codeHash == emptyCodeHashHex || codeHash == "0x0000000000000000000000000000000000000000000000000000000000000000" {
			// Not deployed at the stored height yet (the I-28 alarm says so while
			// it is not deployed at all); deployed and final above it: want a proof.
			f, err := finalizedCodeBlock(ctx, rpc, dc)
			if err != nil {
				unread = true
				continue
			}
			if f > tip && f > want {
				want = f
				slog.Info("I-28: the MagiDeposit is deployed above the stored L1 height; a proof to it is wanted so it can be attested (EVM-ATTEST-NEED-1)", "gen", g.Gen, "magideposit", dc, "stored", tip, "finalized", f)
			}
			continue
		}
		payload, _ := json.Marshal(map[string]any{"gen": g.Gen, "block_height": proof.BlockHeight, "account_proof": proof.Proof})
		ret, err := vaultOp(ctx, gql, sub, cfg.ContractID, "attestDepositContract", payload)
		if err != nil {
			slog.Warn("I-28: attestDepositContract", "gen", g.Gen, "magideposit", dc, "err", err)
			continue
		}
		slog.Info("I-28: MagiDeposit attested; depositAddress() names it now", "gen", g.Gen, "magideposit", dc, "height", tip, "dry_run", ret)
	}
}
