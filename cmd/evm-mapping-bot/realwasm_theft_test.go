//go:build evm_realwasm

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// rwOwnerAction proposes and executes an owner action on the bridge wasm.
func rwOwnerAction(t *testing.T, c rwChain, action, inner string) (string, bool) {
	t.Helper()
	c.ct.Deposit("hive:owner", 1_000_000, ledgerDb.AssetHbd)
	r := rwCall(c.ct, "hive:owner", "propose", []byte(`{"action":"`+action+`","payload_hex":"`+hex.EncodeToString([]byte(inner))+`"}`))
	if !r.Success {
		t.Fatalf("propose %s: %s: %s", action, r.Err, r.ErrMsg)
	}
	var resp struct {
		ProposalId uint64 `json:"proposal_id"`
		ExecHeight uint64 `json:"exec_height"`
	}
	if err := json.Unmarshal([]byte(r.Ret), &resp); err != nil {
		t.Fatalf("propose response %q: %v", r.Ret, err)
	}
	if resp.ExecHeight > c.ct.BlockHeight {
		c.ct.IncrementBlocks(resp.ExecHeight - c.ct.BlockHeight)
	}
	r = rwCall(c.ct, "hive:owner", "execute", []byte(`{"proposal_id":`+jsonNum(resp.ProposalId)+`}`))
	return r.ErrMsg, r.Success
}

// P1 (BTC reportUnauthorizedSpend parity) on the bridge wasm, with the bot's
// own account-proof payload: a proof that the vault address sent no more
// transactions than the bridge authorized is refused and writes nothing; a
// proof of one more sets the theft halt "th" the nodes read, from any caller;
// the owner's clearTheftHalt deletes it.
func TestRealWasm_P1_UnauthorizedSpendSetsTheTheftHalt(t *testing.T) {
	c, _ := rwUSDCBridge(t) // generation 0 active, no withdrawal issued: 0 authorized
	report := func(height, nonce uint64) (string, bool, int64) {
		t.Helper()
		proofRPC, root := rwStateProof(t, c.vault, nonce, big.NewInt(1e18))
		rwHeaderState(c.ct, height, root, &fakeChain{}, rwBlockTime)
		proof, got, _, err := fetchAccountProof(context.Background(), proofRPC, c.vault.Hex(), height)
		if err != nil || got != nonce {
			t.Fatalf("fetchAccountProof: nonce %d %v", got, err)
		}
		p, _ := json.Marshal(map[string]any{"gen": 0, "proof": proof})
		r := rwCallLimit(c.ct, "hive:anyone", "reportUnauthorizedSpend", p, 50_000)
		return r.ErrMsg, r.Success, r.RcUsed
	}

	msg, ok, _ := report(105, 0)
	if ok || !strings.Contains(msg, "no unauthorized spend") {
		t.Fatalf("an honest vault (nonce 0, 0 authorized) was reported: ok=%v %q", ok, msg)
	}
	if th := c.ct.StateGet(rwBridge, "th"); th != "" {
		t.Fatalf("a refused report wrote th=%q", th)
	}

	msg, ok, rc := report(106, 1)
	if !ok {
		t.Fatalf("a nonce above the authorized count was refused: %s", msg)
	}
	t.Logf("reportUnauthorizedSpend: %d RC", rc)
	if th := c.ct.StateGet(rwBridge, "th"); th == "" {
		t.Fatal("the theft halt th is not set after a proven unauthorized spend")
	}

	if msg, ok := rwOwnerAction(t, c, "clearTheftHalt", ""); !ok {
		t.Fatalf("clearTheftHalt: %s", msg)
	}
	if th := c.ct.StateGet(rwBridge, "th"); th != "" {
		t.Fatalf("th=%q after clearTheftHalt; the node reads any value as a halt, so it must be deleted", th)
	}
}

// C2 on the bridge wasm with the bot's account-proof payload: the custody
// proof of the bridge's one vault at a header after its deposits; holding the
// booked custody passes, 0.06 ETH short sets "si" (which the nodes read as a
// keysign halt), a full custody again lifts it.
func TestRealWasm_C2_CustodyProof(t *testing.T) {
	c, _ := rwUSDCBridge(t)
	fields := strings.Split(c.ct.StateGet(rwBridge, "s-eth"), "|")
	book, ok := new(big.Int).SetString(fields[0], 10)
	if !ok || book.Sign() <= 0 {
		t.Fatalf("booked eth custody %q", fields[0])
	}
	prove := func(height uint64, balance *big.Int) (string, bool, int64) {
		t.Helper()
		proofRPC, root := rwStateProof(t, c.vault, 0, balance)
		rwHeaderState(c.ct, height, root, &fakeChain{}, rwBlockTime)
		proof, _, _, err := fetchAccountProof(context.Background(), proofRPC, c.vault.Hex(), height)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := json.Marshal(map[string]any{"gen": 0, "proof": proof})
		r := rwCallLimit(c.ct, "hive:anyone", "proveCustody", p, 50_000)
		return r.Ret + r.ErrMsg, r.Success, r.RcUsed
	}
	if out, ok, rc := prove(110, book); !ok || !strings.Contains(out, "custody held") || c.ct.StateGet(rwBridge, "si") != "" {
		t.Fatalf("full custody: %q ok=%v", out, ok)
	} else {
		t.Logf("proveCustody (one generation, completes): %d RC", rc)
	}
	short := new(big.Int).Sub(book, big.NewInt(6e16))
	if out, ok, _ := prove(111, short); !ok || !strings.Contains(out, "short") || c.ct.StateGet(rwBridge, "si") == "" {
		t.Fatalf("0.06 ETH short: %q ok=%v si=%q", out, ok, c.ct.StateGet(rwBridge, "si"))
	}
	if out, ok, _ := prove(112, book); !ok || c.ct.StateGet(rwBridge, "si") != "" {
		t.Fatalf("custody back: %q ok=%v si=%q", out, ok, c.ct.StateGet(rwBridge, "si"))
	}
	if out, ok, _ := prove(50, book); ok || !strings.Contains(out, "credited deposit") {
		t.Fatalf("a header older than the bridge's deposits: %q ok=%v", out, ok)
	}
}
