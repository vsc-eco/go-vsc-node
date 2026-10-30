package tss

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	evmScopeContract = "vsc1EvmBridge"
	// secp256k1 private keys 1, 2, 3: compressed public keys and addresses.
	evmPub1 = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	evmPub2 = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	evmPub3 = "02f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
	evmAdr2 = "0x2b5ad5c4795c026514f8317c7a215e218dccd6cf"
	// A token sweep exactly as the bridge contract builds it (account-mapping
	// TestA01_MigrationRefusals output): chain 1, nonce 0, tip 2 gwei, fee cap 4
	// gwei, 65,000 gas, to token 0x..c0, transfer(evmAdr2, 500000000).
	contractTokenSweep = "02f86c0180847735940084ee6b280082fde89400000000000000000000000000000000000000c080b844a9059cbb0000000000000000000000002b5ad5c4795c026514f8317c7a215e218dccd6cf000000000000000000000000000000000000000000000000000000001dcd6500c0"
)

type scopeVault struct {
	gen    uint32
	status btcvault.VaultStatus
}

func registryBytes(vs ...scopeVault) []byte {
	out := make([]byte, 0, len(vs)*btcvault.VaultEntrySize)
	for _, v := range vs {
		e := make([]byte, btcvault.VaultEntrySize)
		binary.BigEndian.PutUint32(e, v.gen)
		e[70] = byte(v.status)
		out = append(out, e...)
	}
	return out
}

func unsignedTx(chain uint64, to common.Address, value *big.Int, data []byte) []byte {
	b, _ := rlp.EncodeToBytes(evmUnsignedTx{ChainID: new(big.Int).SetUint64(chain), Nonce: 3, GasTipCap: big.NewInt(2e9),
		GasFeeCap: big.NewInt(4e9), Gas: 65000, To: &to, Value: value, Data: data})
	return append([]byte{0x02}, b...)
}

func transferData(to common.Address, amt int64) []byte {
	d := append([]byte{}, erc20TransferSelector...)
	d = append(d, common.LeftPadBytes(to.Bytes(), 32)...)
	return append(d, common.LeftPadBytes(big.NewInt(amt).Bytes(), 32)...)
}

type scopeWorld struct {
	state map[string][]byte
	keys  map[string]tss_db.TssKey
	err   error
}

func (w *scopeWorld) deps() evmScopeDeps {
	return evmScopeDeps{
		contractId: evmScopeContract,
		readKeyStrict: func(k string) ([]byte, bool, error) {
			if w.err != nil {
				return nil, false, w.err
			}
			v, ok := w.state[k]
			return v, ok, nil
		},
		findKey: func(id string) (tss_db.TssKey, error) {
			k, ok := w.keys[id]
			if !ok {
				return tss_db.TssKey{}, errors.New("no key")
			}
			return k, nil
		},
	}
}

func key(n uint32) string {
	return evmScopeContract + "-" + btcvault.VaultKeyNameFor(btcvault.EvmVaultKeyBase, n)
}

func activeKey(pub string) tss_db.TssKey {
	return tss_db.TssKey{PublicKey: pub, Status: tss_db.TssKeyActive}
}

// rotatedWorld: gen 0 Draining (key 1), gen 1 Active (key 2), gen 2 Pending
// (key 3), gen 3 Purged; one open sweep of gen 0 at nonce 3 carrying versions.
func rotatedWorld(versions ...[]byte) *scopeWorld {
	w := &scopeWorld{state: map[string][]byte{}, keys: map[string]tss_db.TssKey{
		key(0): activeKey(evmPub1), key(1): activeKey(evmPub2), key(2): activeKey(evmPub3),
	}}
	w.state["v"] = registryBytes(scopeVault{0, btcvault.VaultStatusDraining}, scopeVault{1, btcvault.VaultStatusActive},
		scopeVault{2, btcvault.VaultStatusPending}, scopeVault{3, btcvault.VaultStatusPurged})
	w.state["chainid"] = []byte("1")
	rec := evmSweepRecord{Gen: 0, Nonce: 3}
	for _, v := range versions {
		rec.Versions = append(rec.Versions, evmSweepVersion{Unsigned: hex.EncodeToString(v)})
	}
	raw, _ := json.Marshal(rec)
	w.state["ms-0-3"] = raw
	w.state["msr"] = []byte("0-3")
	return w
}

func TestA01_ContractBuiltSweepDecodes(t *testing.T) {
	raw, _ := hex.DecodeString(contractTokenSweep)
	if !evmTxPaysOnly(raw, 1, common.HexToAddress(evmAdr2)) {
		t.Fatal("the bridge's own token sweep does not decode as paying the successor")
	}
	if evmTxPaysOnly(raw, 1, common.HexToAddress("0x00000000000000000000000000000000000000ee")) {
		t.Fatal("it pays another successor")
	}
	if evmTxPaysOnly(raw, 11155111, common.HexToAddress(evmAdr2)) {
		t.Fatal("chain id not checked")
	}
}

func TestA01_EvmScopeVerdicts(t *testing.T) {
	succ := common.HexToAddress(evmAdr2)
	other := common.HexToAddress("0x00000000000000000000000000000000000000ee")
	token := common.HexToAddress("0x00000000000000000000000000000000000000c0")
	ethSweep := unsignedTx(1, succ, big.NewInt(5e17), nil)
	tokSweep := unsignedTx(1, token, big.NewInt(0), transferData(succ, 500))

	// Honest sweeps of the retiring generation are issued.
	for name, tx := range map[string][]byte{"eth": ethSweep, "token": tokSweep} {
		w := rotatedWorld(tx)
		if v := w.deps().evaluate(key(0), crypto.Keccak256(tx)); v != scopeSuccessorSweep {
			t.Fatalf("%s sweep: verdict %v", name, v)
		}
	}
	// Re-driven: the second version's digest is accepted too.
	w := rotatedWorld(ethSweep, tokSweep)
	if v := w.deps().evaluate(key(0), crypto.Keccak256(tokSweep)); v != scopeSuccessorSweep {
		t.Fatalf("second version: %v", v)
	}

	approve := append([]byte{0x09, 0x5e, 0xa7, 0xb3}, transferData(succ, 500)[4:]...)
	attacks := map[string][]byte{
		"eth to another address":     unsignedTx(1, other, big.NewInt(5e17), nil),
		"token to another address":   unsignedTx(1, token, big.NewInt(0), transferData(other, 500)),
		"approve":                    unsignedTx(1, token, big.NewInt(0), approve),
		"value with calldata":        unsignedTx(1, succ, big.NewInt(1), transferData(succ, 1)),
		"another chain":              unsignedTx(11155111, succ, big.NewInt(5e17), nil),
		"type 1 transaction":         append([]byte{0x01}, ethSweep[1:]...),
		"trailing bytes":             append(append([]byte{}, ethSweep...), 0),
		"transfer calldata too long": unsignedTx(1, token, big.NewInt(0), append(transferData(succ, 500), 0)),
	}
	for name, tx := range attacks {
		w := rotatedWorld(tx) // the attacker's tx even sits in the sweep record
		if v := w.deps().evaluate(key(0), crypto.Keccak256(tx)); v != scopeRefuse {
			t.Errorf("%s: verdict %v, want refused", name, v)
		}
	}

	// A digest of no open sweep version.
	w = rotatedWorld(ethSweep)
	if v := w.deps().evaluate(key(0), crypto.Keccak256(tokSweep)); v != scopeRefuse {
		t.Fatalf("digest outside the records: %v", v)
	}
	// A record filed under gen 0 that names another generation.
	w = rotatedWorld(ethSweep)
	var rec evmSweepRecord
	json.Unmarshal(w.state["ms-0-3"], &rec)
	rec.Gen = 1
	w.state["ms-0-3"], _ = json.Marshal(rec)
	if v := w.deps().evaluate(key(0), crypto.Keccak256(ethSweep)); v != scopeRefuse {
		t.Fatalf("record of another generation: %v", v)
	}
	// The successor's address comes from the committed key: not committed, refused.
	w = rotatedWorld(ethSweep)
	delete(w.keys, key(1))
	if v := w.deps().evaluate(key(0), crypto.Keccak256(ethSweep)); v != scopeRefuse {
		t.Fatalf("uncommitted successor key: %v", v)
	}
	w = rotatedWorld(ethSweep)
	w.keys[key(1)] = tss_db.TssKey{PublicKey: evmPub2, Status: tss_db.TssKeyDeprecated}
	if v := w.deps().evaluate(key(0), crypto.Keccak256(ethSweep)); v != scopeRefuse {
		t.Fatalf("deprecated successor key: %v", v)
	}
	// No chain id in the contract state: refused.
	w = rotatedWorld(ethSweep)
	delete(w.state, "chainid")
	if v := w.deps().evaluate(key(0), crypto.Keccak256(ethSweep)); v != scopeRefuse {
		t.Fatalf("no chain id: %v", v)
	}

	// The active generation signs withdrawals, unscoped.
	w = rotatedWorld()
	if v := w.deps().evaluate(key(1), []byte{1, 2, 3}); v != scopeAllow {
		t.Fatalf("active generation: %v", v)
	}
	// Pending: its check message (either form) and nothing else.
	pub3, _ := hex.DecodeString(evmPub3)
	if v := w.deps().evaluate(key(2), btcvault.CheckSigMessage(key(2), 2, pub3)); v != scopeCheckSig {
		t.Fatalf("pending check message: %v", v)
	}
	if v := w.deps().evaluate(key(2), evmVaultCheckDigest(evmScopeContract, 1, evmPub3)); v != scopeCheckSig {
		t.Fatalf("pending EIP-191 vault check: %v", v)
	}
	if v := w.deps().evaluate(key(2), crypto.Keccak256(ethSweep)); v != scopeRefuse {
		t.Fatalf("pending signing a transfer: %v", v)
	}
	// Purged, unknown: refused.
	for _, k := range []string{key(3), key(9)} {
		if v := w.deps().evaluate(k, []byte{1}); v != scopeRefuse {
			t.Fatalf("%s: %v", k, v)
		}
	}
}

func TestA01_EvmScopeRegistryStates(t *testing.T) {
	// No registry (a pre-rotation bridge): its one key is the vault.
	w := &scopeWorld{state: map[string][]byte{}, keys: map[string]tss_db.TssKey{}}
	if v := w.deps().evaluate(key(0), []byte{1}); v != scopeAllow {
		t.Fatalf("pre-rotation primary: %v", v)
	}
	if v := w.deps().evaluate(key(1), []byte{1}); v != scopeRefuse {
		t.Fatalf("a generation key with no registry: %v", v)
	}
	// Unreadable: refuse even the primary.
	w.err = errors.New("blockstore")
	if v := w.deps().evaluate(key(0), []byte{1}); v != scopeRefuse {
		t.Fatalf("unreadable registry: %v", v)
	}
	// Corrupt, duplicate generation, two active: refuse.
	for name, reg := range map[string][]byte{
		"corrupt":    {1, 2, 3},
		"duplicate":  registryBytes(scopeVault{0, btcvault.VaultStatusActive}, scopeVault{0, btcvault.VaultStatusRetiring}),
		"two active": registryBytes(scopeVault{0, btcvault.VaultStatusActive}, scopeVault{1, btcvault.VaultStatusActive}),
	} {
		w := &scopeWorld{state: map[string][]byte{"v": reg}, keys: map[string]tss_db.TssKey{}}
		if v := w.deps().evaluate(key(0), []byte{1}); v != scopeRefuse {
			t.Fatalf("%s registry: %v", name, v)
		}
	}
	// Fresh genesis: one Pending generation signs only its check message.
	w = &scopeWorld{state: map[string][]byte{"v": registryBytes(scopeVault{0, btcvault.VaultStatusPending}), "chainid": []byte("1")},
		keys: map[string]tss_db.TssKey{key(0): activeKey(evmPub1)}}
	pub1, _ := hex.DecodeString(evmPub1)
	if v := w.deps().evaluate(key(0), btcvault.CheckSigMessage(key(0), 0, pub1)); v != scopeCheckSig {
		t.Fatalf("genesis check message: %v", v)
	}
	if v := w.deps().evaluate(key(0), []byte{1}); v != scopeRefuse {
		t.Fatalf("genesis signing anything else: %v", v)
	}
}

// A duplicate generation can hide a retiring entry behind an active one; an
// unknown generation key is no vault key at all. Both are refused, even for a
// digest that would otherwise pass.
func TestA01_EvmScopeCorruptShapes(t *testing.T) {
	w := &scopeWorld{state: map[string][]byte{"chainid": []byte("1"),
		"v": registryBytes(scopeVault{0, btcvault.VaultStatusRetiring}, scopeVault{0, btcvault.VaultStatusActive})},
		keys: map[string]tss_db.TssKey{key(0): activeKey(evmPub1)}}
	if v := w.deps().evaluate(key(0), []byte{1}); v != scopeRefuse {
		t.Fatalf("retiring gen 0 shadowed by an active duplicate: %v", v)
	}
	w = rotatedWorld()
	w.keys[key(9)] = activeKey(evmPub3)
	pub3, _ := hex.DecodeString(evmPub3)
	for _, gen := range []uint32{9, 0} {
		if v := w.deps().evaluate(key(9), btcvault.CheckSigMessage(key(9), gen, pub3)); v != scopeRefuse {
			t.Fatalf("a generation the registry does not list (check message for gen %d): %v", gen, v)
		}
	}
}

// The gate binds from 0.13.0 on the configured bridge only; below it, or for
// another contract's key, it passes the sign through untouched.
func TestA01_EvmSignRefusedGate(t *testing.T) {
	bridges := map[string]string{"ETH": evmBridge}
	pre, _ := evmOnlyManager(t, consensusversion.V0_12_0, bridges)
	if pre.evmSignRefused(evmBridge+"-primaryv1", make([]byte, 32), 1000) {
		t.Fatal("0.12.0: the EVM gate must be inert")
	}
	mgr, _ := evmOnlyManager(t, consensusversion.V0_13_0, bridges)
	mgr.tssKeys = stubTssKeys{}
	mgr.contractState = noStateOutputs{} // a deployed bridge with no registry yet
	// No registry: a generation key that cannot exist yet is refused...
	if !mgr.evmSignRefused(evmBridge+"-primaryv1", make([]byte, 32), 1000) {
		t.Fatal("0.13.0: a generation key with no registry was signed")
	}
	// ...the pre-rotation key is not, and other contracts' keys are untouched.
	if mgr.evmSignRefused(evmBridge+"-primary", make([]byte, 32), 1000) {
		t.Fatal("0.13.0: the pre-rotation primary key was refused")
	}
	if mgr.evmSignRefused("vsc1Other-primaryv1", make([]byte, 32), 1000) {
		t.Fatal("another contract's key was scoped")
	}
}

// noStateOutputs is a contract whose output carries no state: every key reads
// as genuinely absent.
type noStateOutputs struct{ contracts.ContractState }

func (noStateOutputs) GetLastOutput(string, uint64) (contracts.ContractOutput, error) {
	return contracts.ContractOutput{}, nil
}

// spendAt is the bridge's d-{nonce} record of an ETH withdrawal of 1 ETH to
// 0x..e2 at nonce n on chain, issued from vault.
func spendAt(chain, n uint64, vault string) ([]byte, []byte) {
	to := common.HexToAddress("0x00000000000000000000000000000000000000e2")
	b, _ := rlp.EncodeToBytes(evmUnsignedTx{ChainID: new(big.Int).SetUint64(chain), Nonce: n, GasTipCap: big.NewInt(2e9),
		GasFeeCap: big.NewInt(4e9), Gas: 21000, To: &to, Value: big.NewInt(1e18)})
	unsigned := append([]byte{0x02}, b...)
	raw, _ := json.Marshal(map[string]any{"nonce": n, "unsigned_tx_hex": hex.EncodeToString(unsigned), "vault_at_queue": vault})
	return raw, crypto.Keccak256(unsigned)
}

// A01 ROT-HALT-1: a retiring generation re-signs its own withdrawals still
// open from before its cut-over (the contract's records, below its ceiling,
// from its own address), and nothing else of the kind.
func TestA01_EvmRetiringSignsItsOwnInFlightWithdrawal(t *testing.T) {
	gen0 := "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf" // key 1, as the bridge stores it
	w := rotatedWorld()
	w.state["wnc-0"] = []byte("5")
	rec, sh := spendAt(1, 4, gen0)
	w.state["d-4"] = rec
	if got := w.deps().evaluate(key(0), sh); got != scopeOwnWithdrawal {
		t.Fatalf("own in-flight withdrawal: verdict %v", got)
	}
	cases := map[string]func(w *scopeWorld) []byte{
		"no ceiling": func(w *scopeWorld) []byte { delete(w.state, "wnc-0"); return sh },
		"at the ceiling": func(w *scopeWorld) []byte {
			r, h := spendAt(1, 5, gen0)
			w.state["d-5"] = r
			return h
		},
		"issued from the new generation": func(w *scopeWorld) []byte {
			r, h := spendAt(1, 4, evmAdr2)
			w.state["d-4"] = r
			return h
		},
		"record nonce differs from the tx": func(w *scopeWorld) []byte {
			r, h := spendAt(1, 3, gen0)
			w.state["d-4"] = r
			return h
		},
		"another chain": func(w *scopeWorld) []byte {
			r, h := spendAt(11155111, 4, gen0)
			w.state["d-4"] = r
			return h
		},
		"below the in-flight window": func(w *scopeWorld) []byte {
			w.state["wnc-0"] = []byte("20")
			return sh
		},
		"not a recorded tx":          func(w *scopeWorld) []byte { return crypto.Keccak256([]byte("x")) },
		"retiring key not committed": func(w *scopeWorld) []byte { delete(w.keys, key(0)); return sh },
	}
	for name, mut := range cases {
		w := rotatedWorld()
		w.state["wnc-0"] = []byte("5")
		w.state["d-4"] = rec
		h := mut(w)
		if got := w.deps().evaluate(key(0), h); got != scopeRefuse {
			t.Errorf("%s: verdict %v, want refuse", name, got)
		}
	}
	// A Purged generation signs nothing, records or not.
	w = rotatedWorld()
	w.state["wnc-3"] = []byte("5")
	w.keys[key(3)] = activeKey(evmPub1)
	w.state["d-4"] = rec
	if got := w.deps().evaluate(key(3), sh); got != scopeRefuse {
		t.Fatalf("purged generation: verdict %v", got)
	}
}
