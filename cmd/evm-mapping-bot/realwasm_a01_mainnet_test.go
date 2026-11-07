//go:build evm_realwasm

package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

// A01 on mainnet-shaped data: migrateVault verifies a real Ethereum mainnet
// account proof (testdata/mainnet_account_proof.json: eth_getProof of the
// address of private key 1 at a 2026-10-30 head, 9 trie nodes, 3,817 bytes)
// against its state root. Its RC is compared with the same call on a shallow
// (3-node) proof; the payload is the size the bot sends on mainnet.
func TestRealWasm_A01_MainnetDepthProofRC(t *testing.T) {
	raw, err := os.ReadFile("testdata/mainnet_account_proof.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Result struct {
			Nonce        string   `json:"nonce"`
			Balance      string   `json:"balance"`
			AccountProof []string `json:"accountProof"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Result.AccountProof) != 9 {
		t.Fatalf("fixture: %v (%d nodes)", err, len(f.Result.AccountProof))
	}
	var nodes []byte
	for _, n := range f.Result.AccountProof {
		nodes = append(nodes, hexutil.MustDecode(n)...)
	}
	mainnetRoot := common.BytesToHash(ethCrypto.Keccak256(hexutil.MustDecode(f.Result.AccountProof[0])))
	nonce := hexToUint64(f.Result.Nonce)
	key0, _ := ethCrypto.ToECDSA(common.LeftPadBytes([]byte{1}, 32))
	vault0 := ethCrypto.PubkeyToAddress(key0.PublicKey)

	measure := func(proofB64 string, root common.Hash) (int64, string, int) {
		t.Helper()
		ct := rwContract(t)
		ct.VaultRotationV2, ct.KeyReshared = true, true
		pub0 := hex.EncodeToString(ethCrypto.CompressPubkey(&key0.PublicKey))
		ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primary", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType, PublicKey: pub0})
		ct.Deposit(rwRelayer, 1_000_000, "hbd")
		ct.Deposit("hive:owner", 1_000_000, "hbd")
		sig, _ := ethCrypto.Sign(vaultCheckDigest(rwBridge, wireChainID, pub0), key0)
		if r := rwCall(ct, rwRelayer, "attestVault", []byte(`{"signature":"`+hex.EncodeToString(sig)+`"}`)); !r.Success {
			t.Fatalf("attestVault: %s", r.ErrMsg)
		}
		if r := rwCallLimit(ct, "hive:owner", "setVaultOperator", []byte(rwRelayer), 1_000_000); !r.Success {
			t.Fatalf("setVaultOperator: %s", r.ErrMsg)
		}
		ct.StateSet(rwBridge, "s-eth", "1000000000000000000|1000000000000000000|0|0")
		rwHeaderAt(ct, 101, &fakeChain{}, rwBlockTime)
		if r := rwCallLimit(ct, "hive:owner", "createKey", nil, 1_000_000); !r.Success {
			t.Fatalf("createKey: %s", r.ErrMsg)
		}
		key1, _ := ethCrypto.GenerateKey()
		pub1 := hex.EncodeToString(ethCrypto.CompressPubkey(&key1.PublicKey))
		ct.Tss.Keys.SetKey(tss_db.TssKey{Id: rwBridge + "-primaryv1", Status: tss_db.TssKeyActive, Algo: tss_db.EcdsaType, PublicKey: pub1, SignatureVerified: true, Epoch: 2})
		ct.Tss.Commitments.SetCommitmentData(tss_db.TssCommitment{Type: "keygen", KeyId: rwBridge + "-primaryv1", Epoch: 1, PublicKey: &pub1})
		if r := rwCallLimit(ct, "hive:owner", "activateKey", nil, 1_000_000); !r.Success {
			t.Fatalf("activateKey: %s", r.ErrMsg)
		}
		ct.StateSet(rwBridge, "msn-0", jsonNum(nonce)) // the address's mainnet nonce is the next sweep's
		rwHeaderState(ct, 102, root, &fakeChain{}, rwBlockTime)
		p, _ := json.Marshal(map[string]any{"eth_proof": accountProof{BlockHeight: 102, Proof: proofB64}})
		r := rwCallLimit(ct, rwRelayer, "migrateVault", p, 1_000_000)
		if !r.Success {
			t.Fatalf("migrateVault: %s: %s", r.Err, r.ErrMsg)
		}
		return r.RcUsed, r.Ret, len(p)
	}
	deep, ret, size := measure(base64.StdEncoding.EncodeToString(nodes), mainnetRoot)
	if !strings.Contains(ret, "dust") {
		t.Fatalf("mainnet proof (balance %s): %q, want it read (the address is empty: dust)", f.Result.Balance, ret)
	}
	shallowRPC, shallowRoot := rwStateProof(t, vault0, nonce, new(big.Int))
	sp, _, _, err := fetchAccountProof(t.Context(), shallowRPC, vault0.Hex(), 102)
	if err != nil {
		t.Fatal(err)
	}
	shallow, _, _ := measure(sp.Proof, shallowRoot)
	t.Logf("migrateVault with the 9-node mainnet proof: %d RC, payload %d B; with a shallow proof: %d RC", deep, size, shallow)
	if size > 8192 {
		t.Fatalf("payload %d B is over the node's 8,192-byte call cap", size)
	}
	if uint64(deep)*2 > vaultOpRcLimit {
		t.Fatalf("%d RC: twice that is over the bot's %d ceiling", deep, vaultOpRcLimit)
	}
}
