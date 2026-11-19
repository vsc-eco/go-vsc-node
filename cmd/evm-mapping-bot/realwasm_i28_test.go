//go:build evm_realwasm

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// rwAttestDeposit proves the vault's MagiDeposit has code (I-28): a state trie
// with code at the CREATE2 address (among other accounts), planted as block n,
// then attestDepositContract for generation gen. The verifier's newest height
// is put back afterwards so the rest of the test runs as before. Returns the RC
// the attestation used.
func rwAttestDeposit(t *testing.T, ct *test_utils.ContractTest, vault common.Address, gen uint32, n uint64) int64 {
	t.Helper()
	dc := common.HexToAddress(depositContractAddress(vault.Hex()))
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	put := func(a common.Address, codeHash []byte) {
		enc, _ := gethrlp.EncodeToBytes(&types.StateAccount{Nonce: 1, Balance: uint256.NewInt(0), Root: types.EmptyRootHash, CodeHash: codeHash})
		tr.MustUpdate(ethCrypto.Keccak256(a.Bytes()), enc)
	}
	for i := 1; i <= 40; i++ {
		put(common.BigToAddress(big.NewInt(int64(i*7919))), types.EmptyCodeHash.Bytes())
	}
	put(dc, ethCrypto.Keccak256([]byte("MagiDeposit runtime")))
	root := tr.Hash()
	var proof proofList
	if err := tr.Prove(ethCrypto.Keccak256(dc.Bytes()), &proof); err != nil {
		t.Fatal(err)
	}
	prevH := ct.StateGet(rwVerifier, "h")
	rwHeaderState(ct, n, root, &fakeChain{}, 1_700_000_000)
	payload, _ := json.Marshal(map[string]any{"gen": gen, "block_height": n, "account_proof": base64.StdEncoding.EncodeToString(bytes.Join(proof, nil))})
	r := rwCallLimit(ct, "hive:anyone", "attestDepositContract", payload, 1_000_000)
	if !r.Success || !strings.Contains(r.Ret, "attested MagiDeposit") {
		t.Fatalf("attestDepositContract: %v %q %s", r.Success, r.Ret, r.ErrMsg)
	}
	ct.StateSet(rwVerifier, "h", prevH)
	t.Logf("attestDepositContract (I-28): %d RC", r.RcUsed)
	return r.RcUsed
}
