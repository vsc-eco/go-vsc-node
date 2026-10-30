package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"vsc-node/lib/btcvault"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// rotKey is secp256k1 private key n.
func rotKey(n byte) *ecdsa.PrivateKey {
	k, _ := ethCrypto.ToECDSA(common.LeftPadBytes([]byte{n}, 32))
	return k
}

func rotAddr(n byte) string {
	return strings.ToLower(ethCrypto.PubkeyToAddress(rotKey(n).PublicKey).Hex())
}

type rotEntry struct {
	gen    uint32
	key    byte // 0: no key yet
	status btcvault.VaultStatus
}

// rotRegistry is the bridge's "v" blob (91-byte entries).
func rotRegistry(es ...rotEntry) []byte {
	var out []byte
	for _, e := range es {
		b := make([]byte, btcvault.VaultEntrySize)
		binary.BigEndian.PutUint32(b, e.gen)
		if e.key != 0 {
			copy(b[4:37], ethCrypto.CompressPubkey(&rotKey(e.key).PublicKey))
		}
		b[70] = byte(e.status)
		out = append(out, b...)
	}
	return out
}

func TestA01Bot_DecodesTheVaultRegistry(t *testing.T) {
	raw := rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive},
		rotEntry{2, 0, btcvault.VaultStatusPending}, rotEntry{3, 3, btcvault.VaultStatusPurged})
	v, err := decodeVaultView("vsc1b", hex.EncodeToString(raw))
	if err != nil || !v.Registry || len(v.Gens) != 4 || v.Active == nil || v.Active.Gen != 1 {
		t.Fatalf("view %+v %v", v, err)
	}
	if v.Active.Address != rotAddr(2) || v.Active.KeyID != "vsc1b-primaryv1" || v.Gens[0].KeyID != "vsc1b-primary" {
		t.Fatalf("active %+v, gen 0 key %s", v.Active, v.Gens[0].KeyID)
	}
	if v.Gens[2].Address != "" {
		t.Fatal("a pending generation with no key has an address")
	}
	got := v.fundHolding()
	if len(got) != 2 || got[0] != rotAddr(1) || got[1] != rotAddr(2) {
		t.Fatalf("fund-holding %v: want generations 0 and 1, not the pending or purged", got)
	}
	if v, err := decodeVaultView("vsc1b", ""); err != nil || v.Registry {
		t.Fatalf("empty registry: %+v %v", v, err)
	}
	if _, err := decodeVaultView("vsc1b", "zz"); err == nil {
		t.Fatal("bad hex accepted")
	}
}

// BOT-RC-1, as the BTC bot: twice the dry run, at least the floor, at most the
// ceiling.
func TestA01Bot_RcFromDryRun(t *testing.T) {
	for _, c := range []struct {
		used int64
		want uint64
	}{{0, dryRunRcFloor}, {-5, dryRunRcFloor}, {9_000, dryRunRcFloor}, {15_000, 30_000}, {600_000, vaultOpRcLimit}} {
		if got := rcFromDryRun(c.used, vaultOpRcLimit); got != c.want {
			t.Errorf("rc_used %d: rc_limit %d, want %d", c.used, got, c.want)
		}
	}
}

// rotUnsigned is 0x02 || rlp(the nine unsigned EIP-1559 fields).
func rotUnsigned(to common.Address, value *big.Int, data []byte) string {
	b, _ := gethrlp.EncodeToBytes([]any{big.NewInt(1), uint64(7), big.NewInt(2e9), big.NewInt(4e9), uint64(65000), to, value, data, types.AccessList{}})
	return hex.EncodeToString(append([]byte{0x02}, b...))
}

// The bot will not broadcast a signed sweep that could pay anyone but the
// active generation (the node would not have signed it).
func TestA01Bot_SweepTxPaysOnly(t *testing.T) {
	succ, other, token := common.HexToAddress(rotAddr(2)), common.HexToAddress("0xee"), common.HexToAddress("0xc0")
	transfer := func(to common.Address) []byte {
		d := append([]byte{0xa9, 0x05, 0x9c, 0xbb}, common.LeftPadBytes(to.Bytes(), 32)...)
		return append(d, common.LeftPadBytes([]byte{5}, 32)...)
	}
	if !sweepTxPaysOnly(rotUnsigned(succ, big.NewInt(5), nil), succ) || !sweepTxPaysOnly(rotUnsigned(token, new(big.Int), transfer(succ)), succ) {
		t.Fatal("a successor sweep was refused")
	}
	approve := append([]byte{0x09, 0x5e, 0xa7, 0xb3}, transfer(succ)[4:]...)
	for name, tx := range map[string]string{
		"ETH elsewhere":       rotUnsigned(other, big.NewInt(5), nil),
		"token elsewhere":     rotUnsigned(token, new(big.Int), transfer(other)),
		"value with calldata": rotUnsigned(succ, big.NewInt(5), []byte{1}),
		"approve":             rotUnsigned(token, new(big.Int), approve),
		"long calldata":       rotUnsigned(token, new(big.Int), append(transfer(succ), 0)),
		"not EIP-1559":        "01" + rotUnsigned(succ, big.NewInt(5), nil)[2:],
		"garbage":             "02ff",
	} {
		if sweepTxPaysOnly(tx, succ) {
			t.Errorf("%s passed", name)
		}
	}
}

// Deposits to every fund-holding generation's address are found in one block.
func TestA01Bot_ScansEveryVaultGeneration(t *testing.T) {
	c := &fakeChain{}
	user, _ := ethCrypto.GenerateKey()
	old, cur := common.HexToAddress(rotAddr(1)), common.HexToAddress(rotAddr(2))
	c.addTx(t, user, &types.DynamicFeeTx{Nonce: 0, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &old, Value: big.NewInt(1e18)})
	c.addTx(t, user, &types.DynamicFeeTx{Nonce: 1, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(30e9), Gas: 21000, To: &cur, Value: big.NewInt(2e18)})
	res, err := scanBlock(c.rpc(t), 101, []string{rotAddr(1), rotAddr(2)}, nil)
	if err != nil || len(res.Deposits) != 2 {
		t.Fatalf("deposits %+v %v: want both generations' deposits", res, err)
	}
	res, _ = scanBlock(c.rpc(t), 101, []string{rotAddr(2)}, nil)
	if len(res.Deposits) != 1 || res.Deposits[0].Amount.Cmp(big.NewInt(2e18)) != 0 {
		t.Fatalf("active only: %+v", res.Deposits)
	}
}

// ROT-HALT-1: after a cut-over the retiring generation finishes its own
// in-flight withdrawal: the bot fetches that generation's signature (not the
// active key's) and keeps its entry until the nonces are handed off.
func TestA01Bot_RetiringGenerationsWithdrawalUsesItsKey(t *testing.T) {
	tx := queuedTx(t, 40e9)
	l1, l2, rpc, gql, cp, cfg := wdWorld(t, tx)
	ps := spendJSON(tx)
	var rec map[string]any
	_ = json.Unmarshal([]byte(ps), &rec)
	rec["vault_at_queue"] = rotAddr(1)
	b, _ := json.Marshal(rec)
	l2.set("vsc1bridge", "d-0", string(b))
	l2.set("vsc1bridge", "wnh", "0")
	sh, _ := computeSighash(tx)
	l2.sigs[sh] = tssSigWith(t, rotKey(1), tx)
	l2.sigKey = map[string]string{sh: "vsc1bridge-primary"}
	v, _ := decodeVaultView("vsc1bridge", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive})))
	cfg.VaultAddress, cfg.ActiveGen, cfg.VaultGens = v.Active.Address, 1, v.Gens

	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primaryv1")
	if len(l1.sent) != 1 || cp.SentWithdrawals["0"].Vault != rotAddr(1) {
		t.Fatalf("broadcasts %d, entry %+v: want generation 0's withdrawal sent with its key", len(l1.sent), cp.SentWithdrawals["0"])
	}
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primaryv1")
	if _, ok := cp.SentWithdrawals["0"]; !ok || len(l1.sent) != 1 {
		t.Fatalf("generation 0's in-flight entry pruned (and re-sent, %d broadcasts) while it holds the nonces", len(l1.sent))
	}
	// Hand-off: the nonces restart at 0 for generation 1; the old entry goes.
	l2.set("vsc1bridge", "wnh", "")
	l2.set("vsc1bridge", "np", "0")
	handleWithdrawals(context.Background(), rpc, gql, cp, cfg, "vsc1bridge-primaryv1")
	if _, ok := cp.SentWithdrawals["0"]; ok {
		t.Fatal("generation 0's entry kept after the hand-off")
	}
}

func TestA01Bot_RedrivesOnlyTheLowestOpenNonce(t *testing.T) {
	sweeps := []sweepRecord{{Gen: 0, Nonce: 5}, {Gen: 0, Nonce: 4}, {Gen: 2, Nonce: 9}, {Gen: 0, Nonce: 6}}
	if lowestOpenNonce(sweeps, 0) != 4 || lowestOpenNonce(sweeps, 2) != 9 {
		t.Fatalf("lowest: gen 0 %d, gen 2 %d", lowestOpenNonce(sweeps, 0), lowestOpenNonce(sweeps, 2))
	}
}

// The health check watches the key of every generation that holds or may
// receive funds, and of a pending one (renewKey renews it too), not a purged
// one; with no registry, the one key.
func TestA01Bot_HealthWatchesEveryLiveKey(t *testing.T) {
	v, _ := decodeVaultView("vsc1b", hex.EncodeToString(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining},
		rotEntry{1, 2, btcvault.VaultStatusActive}, rotEntry{2, 3, btcvault.VaultStatusPending}, rotEntry{3, 4, btcvault.VaultStatusPurged})))
	got := EVMBotConfig{VaultGens: v.Gens}.healthKeys("vsc1b-primary")
	if strings.Join(got, ",") != "vsc1b-primary,vsc1b-primaryv1,vsc1b-primaryv2" {
		t.Fatalf("keys %v", got)
	}
	if got := (EVMBotConfig{}).healthKeys("vsc1b-primary"); len(got) != 1 || got[0] != "vsc1b-primary" {
		t.Fatalf("no registry: %v", got)
	}
}
