//go:build evm_realwasm

package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// E23 on the bridge wasm: with the TSS key active but its check signature not
// yet attested, a deposit to the key's address is refused; requestVaultCheck
// asks the node to sign exactly the bot's vaultCheckDigest; a signature by
// another key is refused; the key's own signature puts the vault live and the
// same deposit is credited.
func TestRealWasm_E23_DepositsWaitForTheKeysCheckSignature(t *testing.T) {
	ct := rwContract(t)
	key, _ := ethCrypto.GenerateKey()
	pub := hex.EncodeToString(ethCrypto.CompressPubkey(&key.PublicKey))
	ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primary", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType, PublicKey: pub})
	vault := ethCrypto.PubkeyToAddress(key.PublicKey)

	userKey, _ := ethCrypto.GenerateKey()
	c := &fakeChain{}
	tx := c.addTx(t, userKey, &types.DynamicFeeTx{
		GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &vault, Value: big.NewInt(1e18),
	})
	rwHeader(ct, c)
	ct.StateSet(rwBridge, "rl-"+rwRelayer, "1")
	dep := detectedDeposit{BlockHeight: 101, TxIndex: 0, LogIndex: -1, TxHash: tx.Hash().Hex(), DepositType: "eth"}
	payload := buildMapPayloadFromRPC(context.Background(), c.rpc(t), dep, dep.BlockHeight)
	if r := rwCall(ct, rwRelayer, "map", payload); r.Success || !strings.Contains(r.ErrMsg, "vault address not configured") {
		t.Fatalf("map before the check signature: success=%v %s", r.Success, r.ErrMsg)
	}

	digest := vaultCheckDigest(rwBridge, wireChainID, pub)
	r := rwCall(ct, rwRelayer, "requestVaultCheck", []byte(`{}`))
	if !r.Success {
		t.Fatalf("requestVaultCheck: %s: %s", r.Err, r.ErrMsg)
	}
	asked := false
	for _, op := range r.Logs[rwBridge].TssOps {
		asked = asked || (op.Type == "sign" && op.KeyId == rwBridge+"-primary" && op.Args == hex.EncodeToString(digest))
	}
	if !asked {
		t.Fatalf("requestVaultCheck did not ask the key to sign %x: %+v", digest, r.Logs[rwBridge].TssOps)
	}

	other, _ := ethCrypto.GenerateKey()
	bad, _ := ethCrypto.Sign(digest, other)
	if r := rwCall(ct, rwRelayer, "attestVault", []byte(`{"signature":"`+hex.EncodeToString(bad)+`"}`)); r.Success {
		t.Fatal("attestVault accepted another key's signature")
	}
	good, _ := ethCrypto.Sign(digest, key)
	if r := rwCall(ct, rwRelayer, "attestVault", []byte(`{"signature":"`+hex.EncodeToString(good)+`"}`)); !r.Success {
		t.Fatalf("attestVault: %s: %s", r.Err, r.ErrMsg)
	}
	if r := rwCall(ct, rwRelayer, "map", payload); !r.Success {
		t.Fatalf("map after the check signature: %s: %s", r.Err, r.ErrMsg)
	}
	if got := ct.StateGet(rwBridge, "a-"+rwDID(ethCrypto.PubkeyToAddress(userKey.PublicKey))+"-eth"); got != "990000000000000000" {
		t.Fatalf("depositor balance %q after the vault went live", got)
	}
}
