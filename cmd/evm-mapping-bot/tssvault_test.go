package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/accounts"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	cbornode "github.com/ipfs/go-ipld-cbor"
)

const (
	tssPubG  = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	tssAddrG = "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf"
)

// A18: the bot's vault is the bridge's TSS key address, read the way the
// contract reads it, not a configured address it only warned about.
func TestA18_BotVaultIsTheTssKeysAddress(t *testing.T) {
	for _, tc := range []struct {
		status, configured string
		want               string // "" = keeps waiting
		wantErr            bool
	}{
		{status: "active", want: tssAddrG},
		{status: "deprecated", want: tssAddrG},
		{status: "active", configured: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf", want: tssAddrG},
		{status: "active", configured: "0x00000000000000000000000000000000000000fa", wantErr: true},
		{status: "created"},
		{status: "deleted"},
		{status: "retired"},
	} {
		l2 := newFakeL2()
		l2.keys["vsc1bridge-primary"] = [2]string{tc.status, tssPubG}
		l2.set("vsc1bridge", "vaultok", tssPubG) // E23: the key's check signature is verified
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		got, err := waitForTssVault(ctx, l2.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", tc.configured, 20*time.Millisecond)
		cancel()
		switch {
		case tc.wantErr:
			if err == nil || !strings.Contains(err.Error(), "VAULT_ADDRESS") {
				t.Errorf("%s, VAULT_ADDRESS=%s: vault %q err %v, want a mismatch refusal", tc.status, tc.configured, got, err)
			}
		case tc.want == "":
			if err != context.DeadlineExceeded {
				t.Errorf("%s: vault %q err %v, want it to keep waiting", tc.status, got, err)
			}
		default:
			if err != nil || got != tc.want {
				t.Errorf("%s, VAULT_ADDRESS=%q: vault %q err %v, want %s", tc.status, tc.configured, got, err, tc.want)
			}
		}
	}
}

// Before keygen the bot waits, then takes the key once it exists.
func TestA18_BotWaitsForKeygen(t *testing.T) {
	l2 := newFakeL2()
	l2.set("vsc1bridge", "vaultok", tssPubG)
	gql := l2.gql(t)
	go func() {
		time.Sleep(100 * time.Millisecond)
		l2.mu.Lock()
		l2.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
		l2.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := waitForTssVault(ctx, gql, nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
	if err != nil || got != tssAddrG {
		t.Fatalf("vault %q err %v, want %s after keygen", got, err, tssAddrG)
	}
}

// E23: the contract refuses deposits to the key's address until the key has
// signed the vault check message and the signature is attested. The bot asks
// for the signature, attests it (low-S, with the v that recovers the vault),
// and only then takes the address as the vault.
func TestE23_BotDrivesTheVaultCheck(t *testing.T) {
	t.Run("sepolia", func(t *testing.T) { botDrivesTheVaultCheck(t, "11155111", 11155111) })
	// No chainid in state: the contract's chainId() reads 1, and so must the bot.
	t.Run("chainid unset", func(t *testing.T) { botDrivesTheVaultCheck(t, "", 1) })
}

func botDrivesTheVaultCheck(t *testing.T, chainIDState string, chainID uint64) {
	logs := captureLogs(t)
	defer func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	l2 := newFakeL2()
	l2.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
	if chainIDState != "" {
		l2.set("vsc1bridge", "chainid", chainIDState)
	}
	gql := l2.gql(t)
	botKey, _ := ethCrypto.GenerateKey()
	sub := &l2Submitter{ethKey: botKey, did: dids.NewEthDID(ethCrypto.PubkeyToAddress(botKey.PublicKey).Hex()), gql: gql,
		cfg: EVMBotConfig{ContractID: "vsc1bridge", NetID: "vsc-devnet", RcLimit: 10000}}
	digest := vaultCheckDigest("vsc1bridge", chainID, tssPubG)
	key1, _ := ethCrypto.ToECDSA(append(make([]byte, 31), 1)) // the key behind tssPubG
	sig, _ := ethCrypto.Sign(digest, key1)
	der := hex.EncodeToString(derSig(sig, true)) // the TSS group's signature, high-S, as DER

	done := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		v, err := waitForTssVault(ctx, gql, sub, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
		if err != nil {
			v = "error: " + err.Error()
		}
		done <- v
	}()
	// submitted returns the payloads of the L2 calls of action so far.
	submitted := func(action string) []string {
		l2.mu.Lock()
		defer l2.mu.Unlock()
		var out []string
		for _, b := range l2.submitted {
			var tx map[string]any
			if err := cbornode.DecodeInto(b, &tx); err != nil {
				t.Fatalf("undecodable L2 tx: %v", err)
			}
			ops, _ := tx["tx"].([]any)
			op, _ := ops[0].(map[string]any)
			raw, _ := base64.StdEncoding.DecodeString(fmt.Sprint(op["payload"])) // the call's CBOR
			var call map[string]any
			if err := cbornode.DecodeInto(raw, &call); err != nil {
				t.Fatalf("undecodable call: %v", err)
			}
			if call["action"] == action {
				out = append(out, fmt.Sprint(call["payload"]))
			}
		}
		return out
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	wait("requestVaultCheck", func() bool { return len(submitted("requestVaultCheck")) == 1 })
	select {
	case v := <-done:
		t.Fatalf("the bot took the vault %q before its check signature", v)
	case <-time.After(100 * time.Millisecond):
	}
	if n := len(submitted("requestVaultCheck")); n != 1 {
		t.Fatalf("requestVaultCheck sent %d times within the retry window", n)
	}

	l2.mu.Lock()
	l2.sigs[hex.EncodeToString(digest)] = der
	l2.mu.Unlock()
	lowS := hex.EncodeToString(sig) // go-ethereum signs low-S: r || s || v
	wait("attestVault", func() bool { return len(submitted("attestVault")) > 0 })
	if got := submitted("attestVault")[0]; got != `{"signature":"`+lowS+`"}` {
		t.Fatalf("attestVault payload %s, want the low-S signature %s", got, lowS)
	}
	l2.set("vsc1bridge", "vaultok", tssPubG) // the attest landed
	select {
	case v := <-done:
		if v != tssAddrG {
			t.Fatalf("vault %q, want %s", v, tssAddrG)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the bot did not take the vault once verified")
	}
}

// derSig is sig (r || s || v) as the TSS group's DER, with s replaced by its
// high twin when high.
func derSig(sig []byte, high bool) []byte {
	sv := new(big.Int).SetBytes(sig[32:64])
	if high {
		sv.Sub(ethCrypto.S256().Params().N, sv)
	}
	enc := func(x []byte) []byte {
		x = new(big.Int).SetBytes(x).Bytes()
		if x[0]&0x80 != 0 {
			x = append([]byte{0}, x...)
		}
		return append([]byte{0x02, byte(len(x))}, x...)
	}
	body := append(enc(sig[:32]), enc(sv.Bytes())...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// vaultCheckRSV gives attestVault the low-S signature with the v that
// recovers the vault, for both recovery parities and either S, and refuses a
// signature that does not recover it.
func TestE23_CheckSignatureRecoversTheVault(t *testing.T) {
	key, _ := ethCrypto.ToECDSA(append(make([]byte, 31), 1))
	seen := map[byte]bool{}
	for i := 0; len(seen) < 2 && i < 64; i++ {
		digest := vaultCheckDigest("vsc1bridge", uint64(i), tssPubG)
		sig, _ := ethCrypto.Sign(digest, key)
		seen[sig[64]] = true
		for _, high := range []bool{false, true} {
			got, err := vaultCheckRSV(derSig(sig, high), hex.EncodeToString(digest), tssAddrG)
			if err != nil || hex.EncodeToString(got) != hex.EncodeToString(sig) {
				t.Fatalf("v=%d high-S=%v: got %x err %v, want %x", sig[64], high, got, err, sig)
			}
		}
		if _, err := vaultCheckRSV(derSig(sig, false), hex.EncodeToString(digest), "0x00000000000000000000000000000000000000fa"); err == nil {
			t.Fatal("a signature that does not recover the vault was accepted")
		}
	}
	if len(seen) < 2 {
		t.Fatal("never saw both recovery parities")
	}
}

// The bot computes the contract's check message (account-mapping
// mapping.VaultCheckDigest): personal_sign of a fixed text, here through
// go-ethereum's own EIP-191 implementation.
func TestE23_VaultCheckDigestIsPersonalSign(t *testing.T) {
	msg := "magi evm vault check-signature v1:vsc1bridge:11155111:" + tssPubG
	if got, want := vaultCheckDigest("vsc1bridge", 11155111, tssPubG), accounts.TextHash([]byte(msg)); hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("digest %x, want %x", got, want)
	}
}
