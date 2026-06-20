//go:build evm_devnet

// W6Neg — admin-bounds (W6.3) + negative-matrix (W6.5) validation for the
// account-mapping EVM bridge. NO anvil, NO TSS: pure contract-call validation
// that the MED admin/validation fixes REJECT bad inputs at runtime on a real
// 5-node devnet (local Go unit tests don't exercise the WASM-host + state path).
//
// Each case: perform the call, read the resulting tx's contract-output (ok,err),
// and assert the expected outcome (reject with a specific error substring, or
// accept with the expected state mutation).
//
// Cases asserted:
//   1. pause gate      — pause -> unmapETH "contract is paused"; unpause ->
//                        unmapETH "invalid amount" (gate lifted, calls flow).
//   2. registerToken   — empty symbol / pipe symbol / min<0 / min>max REJECTED;
//                        a valid registration ACCEPTED (token state key written).
//   3. setGasReserve   — non-numeric / out-of-range REJECTED; valid ACCEPTED
//                        (gr state key written).
//   4. unmapETH degen  — amount 0 "invalid amount"; below-min "below minimum
//                        ETH withdrawal"; zero-address "withdrawal to zero
//                        address rejected". (All reject BEFORE the header/balance
//                        checks — the validation guards fire first.)
//   5. map degen       — bad deposit_type "deposit_type must be 'eth' or 'erc20'";
//                        non-whitelisted-relayer eth "caller is not a whitelisted
//                        relayer".
//
// Run: go test -tags evm_devnet -run TestEVMBridge_W6Neg ./tests/devnet/ -timeout 40m -v
package devnet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEVMBridge_W6Neg_AdminAndNegativeMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("devnet")
	}
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)

	e := setupEvmBridgeDevnet(t, ctx)

	// Track each asserted case so the final summary is honest about coverage.
	asserted := 0
	total := 0
	assertCase := func(name string, gotOk bool, gotErr string, wantOk bool, wantSub string) {
		total++
		if gotOk != wantOk {
			t.Fatalf("[%s] expected ok=%v, got ok=%v (err=%q)", name, wantOk, gotOk, gotErr)
		}
		if !wantOk && wantSub != "" && !strings.Contains(gotErr, wantSub) {
			t.Fatalf("[%s] expected reject containing %q, got ok=%v err=%q", name, wantSub, gotOk, gotErr)
		}
		asserted++
		if wantOk {
			t.Logf("[%s] ACCEPTED (ok=true) as expected", name)
		} else {
			t.Logf("[%s] REJECTED %q (contains %q) as expected", name, gotErr, wantSub)
		}
	}

	// -------------------------------------------------------------------------
	// Vault must be configured so the `map` deposit_type test reaches the type
	// switch (HandleMap rejects an unset vault with "vault address not
	// configured" before the switch). Use the W1 vault setter (short timelock).
	// -------------------------------------------------------------------------
	const vaultAddr = "0x00000000000000000000000000000000000000aa"
	if err := e.setVault(vaultAddr); err != nil {
		t.Fatalf("setVault: %v", err)
	}
	t.Logf("vault configured: %s", vaultAddr)

	// =========================================================================
	// CASE 1 — pause gate (Immediate timelock).
	// =========================================================================
	if err := e.proposeExecute("pause", "{}"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := pollUntil(ctx, 90*time.Second, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"paused"})
		return m != nil && fmt.Sprintf("%v", m["paused"]) == "1"
	}); err != nil {
		t.Fatalf("pause did not set paused=1")
	}
	// A well-formed unmapETH (valid amount, valid non-zero to) must now be
	// rejected at the FIRST gate — "contract is paused" — proving the pause
	// short-circuits before any amount/address validation.
	{
		payload := fmt.Sprintf(`{"amount":"20000000","to":"%s"}`, vaultAddr)
		tx, err := e.callOnce(e.ownerNode, e.amID, "unmapETH", payload)
		if err != nil {
			t.Fatalf("unmapETH(paused) broadcast: %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("pause/unmapETH-blocked", ok, em, false, "contract is paused")
	}
	if err := e.proposeExecute("unpause", "{}"); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	if err := pollUntil(ctx, 90*time.Second, func() bool {
		m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"paused"})
		// StateDeleteObject -> key absent -> read returns nil/empty.
		return m == nil || m["paused"] == nil || fmt.Sprintf("%v", m["paused"]) == ""
	}); err != nil {
		t.Fatalf("unpause did not clear paused")
	}
	// After unpause, the SAME surface no longer hits the pause gate: an amount=0
	// unmapETH now reaches (and is rejected by) the amount validator, proving
	// calls flow again.
	{
		payload := fmt.Sprintf(`{"amount":"0","to":"%s"}`, vaultAddr)
		tx, err := e.callOnce(e.ownerNode, e.amID, "unmapETH", payload)
		if err != nil {
			t.Fatalf("unmapETH(unpaused) broadcast: %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		if strings.Contains(em, "contract is paused") {
			t.Fatalf("pause gate not lifted: %q", em)
		}
		assertCase("unpause/unmapETH-flows", ok, em, false, "invalid amount")
	}

	// =========================================================================
	// CASE 2 — registerToken bounds (Operational timelock, validated in
	// dispatchAdmin "registerToken").
	// =========================================================================
	const tokAddr = "0x1111111111111111111111111111111111111111"
	tokKey := "t-1-" + strings.ToLower(strings.TrimPrefix(tokAddr, "0x")) // chainId()=1

	// Reject: empty symbol.
	{
		inner := fmt.Sprintf(`{"address":"%s","symbol":"","decimals":6,"min_withdrawal":1000000}`, tokAddr)
		ok, em := e.proposeExecuteResult("registerToken", inner)
		assertCase("registerToken/empty-symbol", ok, em, false, "symbol empty or too long")
	}
	// Reject: symbol contains '|' (corrupts the pipe-delimited registry record).
	{
		inner := fmt.Sprintf(`{"address":"%s","symbol":"BA|D","decimals":6,"min_withdrawal":1000000}`, tokAddr)
		ok, em := e.proposeExecuteResult("registerToken", inner)
		assertCase("registerToken/pipe-symbol", ok, em, false, "symbol must not contain '|'")
	}
	// Reject: min_withdrawal negative.
	{
		inner := fmt.Sprintf(`{"address":"%s","symbol":"USDC","decimals":6,"min_withdrawal":-1}`, tokAddr)
		ok, em := e.proposeExecuteResult("registerToken", inner)
		assertCase("registerToken/min-negative", ok, em, false, "min_withdrawal out of range")
	}
	// Reject: min_withdrawal above MaxTokenMinWithdrawal (1e15) — admin DoS.
	{
		inner := fmt.Sprintf(`{"address":"%s","symbol":"USDC","decimals":6,"min_withdrawal":2000000000000000}`, tokAddr)
		ok, em := e.proposeExecuteResult("registerToken", inner)
		assertCase("registerToken/min-too-large", ok, em, false, "min_withdrawal out of range")
	}
	// Accept: a valid registration — assert the token state key is written
	// (symbol|decimals|minWithdrawal).
	{
		inner := fmt.Sprintf(`{"address":"%s","symbol":"USDC","decimals":6,"min_withdrawal":1000000}`, tokAddr)
		if err := e.proposeExecute("registerToken", inner); err != nil {
			t.Fatalf("registerToken(valid): %v", err)
		}
		const wantRec = "USDC|6|1000000"
		if err := pollUntil(ctx, 60*time.Second, func() bool {
			m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{tokKey})
			return m != nil && m[tokKey] != nil && fmt.Sprintf("%v", m[tokKey]) == wantRec
		}); err != nil {
			m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{tokKey})
			t.Fatalf("registerToken(valid) not stored: %s=%v want=%q", tokKey, m[tokKey], wantRec)
		}
		total++
		asserted++
		t.Logf("[registerToken/valid] ACCEPTED — %s=%q", tokKey, wantRec)
	}

	// =========================================================================
	// CASE 3 — setGasReserve bounds. The payload is the RAW decimal value
	// string (dispatchAdmin does strconv.ParseInt(string(payload)...)), so the
	// inner "JSON" is just the literal digits (no quotes, no braces).
	// =========================================================================
	// Reject: non-numeric value.
	{
		ok, em := e.proposeExecuteResult("setGasReserve", "not-a-number")
		assertCase("setGasReserve/non-numeric", ok, em, false, "value must be a base-10 int64")
	}
	// Reject: above MaxGasReserve (1e18).
	{
		ok, em := e.proposeExecuteResult("setGasReserve", "2000000000000000000")
		assertCase("setGasReserve/out-of-range", ok, em, false, "value out of range")
	}
	// Accept: a valid in-range gwei reserve — assert the gr state key is written.
	{
		const valid = "60000000" // 0.06 ETH in gwei, > MinGasReserve(5e7), < MaxGasReserve
		if err := e.proposeExecute("setGasReserve", valid); err != nil {
			t.Fatalf("setGasReserve(valid): %v", err)
		}
		if err := pollUntil(ctx, 60*time.Second, func() bool {
			m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"gr"})
			return m != nil && m["gr"] != nil && fmt.Sprintf("%v", m["gr"]) == valid
		}); err != nil {
			m, _ := e.d.GetStateByKeys(ctx, e.gqlNode, e.amID, []string{"gr"})
			t.Fatalf("setGasReserve(valid) not stored: gr=%v want=%q", m["gr"], valid)
		}
		total++
		asserted++
		t.Logf("[setGasReserve/valid] ACCEPTED — gr=%q", valid)
	}

	// =========================================================================
	// CASE 4 — unmapETH degenerate inputs (reject at the validation guards,
	// which all fire BEFORE the "no block headers available"/balance checks).
	// Owner-signed single broadcast (reverts before any state mutation).
	// =========================================================================
	// amount "0" -> "invalid amount".
	{
		payload := fmt.Sprintf(`{"amount":"0","to":"%s"}`, vaultAddr)
		tx, err := e.callOnce(e.ownerNode, e.amID, "unmapETH", payload)
		if err != nil {
			t.Fatalf("unmapETH(amount=0): %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("unmapETH/amount-zero", ok, em, false, "invalid amount")
	}
	// amount below MinETHWithdrawal (10_000_000 gwei) -> "below minimum ETH withdrawal".
	{
		payload := fmt.Sprintf(`{"amount":"5","to":"%s"}`, vaultAddr)
		tx, err := e.callOnce(e.ownerNode, e.amID, "unmapETH", payload)
		if err != nil {
			t.Fatalf("unmapETH(below-min): %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("unmapETH/below-minimum", ok, em, false, "below minimum ETH withdrawal")
	}
	// to == zero address (amount valid + >= min so we reach the zero-addr guard).
	{
		const zeroAddr = "0x0000000000000000000000000000000000000000"
		payload := fmt.Sprintf(`{"amount":"20000000","to":"%s"}`, zeroAddr)
		tx, err := e.callOnce(e.ownerNode, e.amID, "unmapETH", payload)
		if err != nil {
			t.Fatalf("unmapETH(zero-addr): %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("unmapETH/zero-address", ok, em, false, "withdrawal to zero address rejected")
	}

	// =========================================================================
	// CASE 5 — map degenerate inputs.
	// =========================================================================
	// Unknown deposit_type -> switch default. Vault is set (above) so HandleMap
	// reaches the type switch; a non-relayer caller still reaches default
	// because the relayer gate lives only inside the "eth" case.
	{
		payload := `{"tx_data":{"block_height":1,"tx_index":0,"raw_hex":"00","merkle_proof_hex":"00","deposit_type":"bogus"},"instructions":[]}`
		tx, err := e.callOnce(e.ownerNode, e.amID, "map", payload)
		if err != nil {
			t.Fatalf("map(bad-type): %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("map/bad-deposit-type", ok, em, false, "deposit_type must be")
	}
	// eth deposit by a NON-whitelisted relayer (we never registerRelayer the
	// owner) -> rejected at the relayer gate, BEFORE any proof verification.
	{
		payload := `{"tx_data":{"block_height":1,"tx_index":0,"raw_hex":"00","merkle_proof_hex":"00","deposit_type":"eth"},"instructions":[]}`
		tx, err := e.callOnce(e.ownerNode, e.amID, "map", payload)
		if err != nil {
			t.Fatalf("map(non-relayer-eth): %v", err)
		}
		ok, em := e.outResult(ctx, tx)
		assertCase("map/non-relayer-eth", ok, em, false, "not a whitelisted relayer")
	}

	t.Logf("W6Neg PASS — admin bounds + negative matrix: %d/%d cases asserted "+
		"[pause-blocked, unpause-flows, registerToken{empty,pipe,min<0,min>max,valid}, "+
		"setGasReserve{non-numeric,out-of-range,valid}, unmapETH{amount0,below-min,zero-addr}, "+
		"map{bad-type,non-relayer-eth}]", asserted, total)
}

// outResult reads a tx's contract-output (ok, errMsg). It first reads the
// indexed findContractOutput{ret,ok} (the abort message lands in `ret`), and
// falls back to the raw output DAG (getDagByCID -> results[].errMsg) per the
// dumpMapErr pattern so the specific reject string is recoverable even if the
// indexed `ret` is empty. Polls until an output is observed (the contract call
// is a separate L1 tx that lands a block or two later).
func (e *evmBridgeEnv) outResult(ctx context.Context, txID string) (bool, string) {
	var ok, found bool
	var msg string
	// The raw DAG (getDagByCID) is the ground truth: results[].ret is written
	// atomically with the block and carries the revert/abort message (host
	// `revert` import → WasmResultStruct.Error → transactions.go Ret). The
	// indexed `findContractOutput.ret` resolver lags/empties for reverts under
	// parallel load, so we PREFER the DAG and only fall back to the index. For a
	// reject (ok=false) we keep polling until the message is non-empty, so a
	// transient empty read never wins (which previously made a rejected call look
	// like it had no reason string).
	_ = pollUntil(ctx, 5*time.Minute, func() bool {
		// Primary: raw DAG — has both ret and errMsg for the stored result.
		if o, e2 := e.outResultViaDAG(ctx, txID); e2 {
			ok, found = o.ok, true
			if o.msg != "" {
				msg = o.msg
			}
			// Accept once we have a usable signal: success, or a reject WITH a
			// reason. Keep polling a reject that still has no reason (indexing
			// race) until the message lands or we time out.
			if ok || msg != "" {
				return true
			}
		}
		// Fallback: indexed contract output keyed by the input tx id.
		const q = `query($id:String!){findContractOutput(filterOptions:{byInput:$id}){results{ret ok}}}`
		var out struct {
			FindContractOutput []struct {
				Results []struct {
					Ret string `json:"ret"`
					Ok  bool   `json:"ok"`
				} `json:"results"`
			} `json:"findContractOutput"`
		}
		if err := e.d.gqlQuery(ctx, e.gqlNode, q, map[string]any{"id": txID}, &out); err == nil {
			for _, o := range out.FindContractOutput {
				for _, r := range o.Results {
					ok, found = r.Ok, true
					if r.Ret != "" {
						msg = r.Ret
					}
					if ok || msg != "" {
						return true
					}
				}
			}
		}
		return false
	})
	if !found {
		e.t.Logf("WARN: no contract output observed for tx %s within timeout", txID)
	} else if !ok && msg == "" {
		e.t.Logf("WARN: tx %s rejected (ok=false) but no reason string observed within timeout", txID)
	}
	return ok, msg
}

type dagOut struct {
	ok  bool
	msg string
}

// outResultViaDAG reads the raw contract-output DAG for a tx and returns the
// first result's (ok, message). The getDagByCID resolver may hand back the
// output either as a JSON object OR as a JSON-encoded string, so we capture it
// as `any` (like the proven txOutputMsg), re-marshal to bytes, and parse a
// typed view. message = errMsg, else ret; if BOTH are empty (e.g. a panic-path
// abort that lands the text in an unexpected field, or a different contract's
// output shape), we fall back to the WHOLE stringified blob so a downstream
// strings.Contains can still find the reason string. This is what made the ZK
// verifier reject paths (a separate contract) extractable.
func (e *evmBridgeEnv) outResultViaDAG(ctx context.Context, txID string) (dagOut, bool) {
	const q = `query($id:String!){findTransaction(filterOptions:{byId:$id}){output{id}}}`
	var out struct {
		FindTransaction []struct {
			Output []struct {
				Id string `json:"id"`
			} `json:"output"`
		} `json:"findTransaction"`
	}
	if err := e.d.gqlQuery(ctx, e.gqlNode, q, map[string]any{"id": txID}, &out); err != nil {
		return dagOut{}, false
	}
	for _, tx := range out.FindTransaction {
		for _, o := range tx.Output {
			const dq = `query($c:String!){getDagByCID(cidString:$c)}`
			var dout struct {
				GetDagByCID any `json:"getDagByCID"`
			}
			if err := e.d.gqlQuery(ctx, e.gqlNode, dq, map[string]any{"c": o.Id}, &dout); err != nil {
				continue
			}
			if dout.GetDagByCID == nil {
				continue
			}
			// Re-marshal the untyped value, then parse a typed view. If the value
			// was a JSON string, unmarshal it once more to reach the object.
			rawBytes, _ := json.Marshal(dout.GetDagByCID)
			var parsed struct {
				Results []struct {
					Ok     bool   `json:"ok"`
					Ret    string `json:"ret"`
					ErrMsg string `json:"errMsg"`
					Err    string `json:"err"`
				} `json:"results"`
			}
			if json.Unmarshal(rawBytes, &parsed); len(parsed.Results) == 0 {
				// value may itself be a quoted JSON string
				var inner string
				if json.Unmarshal(rawBytes, &inner) == nil && inner != "" {
					_ = json.Unmarshal([]byte(inner), &parsed)
				}
			}
			rawBlob := fmt.Sprintf("%v", dout.GetDagByCID) + " " + string(rawBytes)
			if len(parsed.Results) > 0 {
				r := parsed.Results[0]
				msg := r.ErrMsg
				if msg == "" {
					msg = r.Ret
				}
				if msg == "" && !r.Ok {
					msg = rawBlob // fallback: whole blob, substring-matchable
				}
				return dagOut{ok: r.Ok, msg: msg}, true
			}
			// Couldn't parse results at all — return the raw blob so the caller
			// can still substring-match; treat as a reject (ok=false) only if the
			// blob shows a failure marker.
			lc := strings.ToLower(rawBlob)
			ok := !strings.Contains(lc, `"ok":false`) && !strings.Contains(lc, `"err"`)
			return dagOut{ok: ok, msg: rawBlob}, true
		}
	}
	return dagOut{}, false
}

// proposeExecuteResult runs an owner admin action through the short (devnet)
// timelock like proposeExecute, but captures the EXECUTE tx id and returns its
// contract-output (ok, errMsg) so a dispatchAdmin reject (the bounds checks
// fire inside execute()) can be asserted. payloadJSON is the action's inner
// payload, hex-encoded into payload_hex.
func (e *evmBridgeEnv) proposeExecuteResult(action, payloadJSON string) (bool, string) {
	d := e.d
	payloadHex := hex.EncodeToString([]byte(payloadJSON))
	if _, err := d.CallContract(e.ctx, e.ownerNode, e.amID, "propose",
		fmt.Sprintf(`{"action":"%s","payload_hex":"%s"}`, action, payloadHex)); err != nil {
		e.t.Fatalf("propose %s: %v", action, err)
	}
	id := e.propID
	e.propID++
	lb, _, err := d.LocalNodeInfo(e.ctx, e.gqlNode)
	if err != nil {
		e.t.Fatalf("node info: %v", err)
	}
	if err := d.WaitForBlockProcessing(e.ctx, e.gqlNode, lb+12, 6*time.Minute); err != nil {
		e.t.Fatalf("await timelock %s: %v", action, err)
	}
	execTx, err := d.CallContract(e.ctx, e.ownerNode, e.amID, "execute", fmt.Sprintf(`{"proposal_id":%d}`, id))
	if err != nil {
		e.t.Fatalf("execute %s broadcast: %v", action, err)
	}
	return e.outResult(e.ctx, execTx)
}
