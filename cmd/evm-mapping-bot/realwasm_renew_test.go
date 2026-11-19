//go:build evm_realwasm

package main

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"

	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// D7 on the bridge wasm: renewKey is proposed and executed in the same block,
// also while the bridge is paused, and renews the primary key.
func TestRealWasm_D7_RenewKeyIsImmediate(t *testing.T) {
	for _, paused := range []bool{false, true} {
		ct := rwContract(t)
		key, _ := ethCrypto.GenerateKey()
		ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primary", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType,
			PublicKey: hex.EncodeToString(ethCrypto.CompressPubkey(&key.PublicKey)), Epoch: 1, Epochs: 365, ExpiryEpoch: 366})
		if paused {
			ct.StateSet(rwBridge, "paused", "1")
		}
		r := rwCall(ct, "hive:owner", "propose", []byte(`{"action":"renewKey","payload_hex":""}`))
		if !r.Success {
			t.Fatalf("paused=%v: propose renewKey: %s: %s", paused, r.Err, r.ErrMsg)
		}
		var resp struct {
			ProposalId uint64 `json:"proposal_id"`
		}
		_ = json.Unmarshal([]byte(r.Ret), &resp)
		r = rwCall(ct, "hive:owner", "execute", []byte(`{"proposal_id":`+jsonNum(resp.ProposalId)+`}`))
		if !r.Success {
			t.Fatalf("paused=%v: execute renewKey in the same block: %s: %s", paused, r.Err, r.ErrMsg)
		}
		renewed := false
		for _, op := range r.Logs[rwBridge].TssOps {
			renewed = renewed || (op.Type == "renew" && op.KeyId == rwBridge+"-primary")
		}
		if !renewed {
			t.Fatalf("paused=%v: no renew op: %+v", paused, r.Logs[rwBridge].TssOps)
		}
	}
}

func jsonNum(v uint64) string { b, _ := json.Marshal(v); return string(b) }
