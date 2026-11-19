package main

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// I-28, testnet 11-04: after the gen-0 -> gen-1 rotation depositAddress()
// named gen 1's MagiDeposit, which nobody had deployed; a 0.001 ETH
// deposit(recipient) to it succeeded on L1 with no Deposit log and was never
// credited. The health check now names every generation whose MagiDeposit
// has no code (a pending one too, see the next test) and stays quiet once it is
// deployed.
func TestI28_UndeployedMagiDepositIsAnAlarm(t *testing.T) {
	active, pending := healthVault, "0x00000000000000000000000000000000000000bb"
	for _, c := range []struct {
		name      string
		code      map[string]string
		wantAlarm bool
		wantSub   string
	}{
		{"neither deployed", nil, true, depositContractAddress(active)},
		{"the pending generation's not deployed", map[string]string{depositContractAddress(active): "0x6080"}, true, depositContractAddress(pending)},
		{"both deployed", map[string]string{depositContractAddress(active): "0x6080", depositContractAddress(pending): "0x6080"}, false, ""},
		{"code unreadable", map[string]string{depositContractAddress(active): "error", depositContractAddress(pending): "0x6080"}, true, "cannot read the MagiDeposit's code"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l1, _, gql, rpc := healthWorld(t, 10)
			l1.code = c.code
			h := newBotHealth(12 * time.Second)
			now := time.Now()
			h.recordTick(now, nil, emptyCP())
			h.check(context.Background(), now, gql, rpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, nil, 0, []string{active, pending})
			r := h.report(now)
			if got := hasIssue(r, "I-28"); got != c.wantAlarm {
				t.Fatalf("I-28 alarm %v, want %v: %v", got, c.wantAlarm, r.Issues)
			}
			if c.wantSub != "" && !hasIssue(r, c.wantSub) {
				t.Fatalf("issues %v do not name %s", r.Issues, c.wantSub)
			}
			if !c.wantAlarm && r.Status != "ok" {
				t.Fatalf("unhealthy with both MagiDeposits deployed: %v", r.Issues)
			}
		})
	}
}

// The bot checks the generations a depositor can be told about: fund-holding
// and pending, not purged.
func TestI28_DepositVaultsAreTheNamedGenerations(t *testing.T) {
	cfg := EVMBotConfig{VaultAddress: "0xa", VaultGens: []vaultGen{
		{Gen: 0, Status: btcvault.VaultStatusPurged, Address: "0xpurged"},
		{Gen: 1, Status: btcvault.VaultStatusInactive, Address: "0xinactive"},
		{Gen: 2, Status: btcvault.VaultStatusActive, Address: "0xactive"},
		{Gen: 3, Status: btcvault.VaultStatusPending, Address: "0xpending"},
		{Gen: 4, Status: btcvault.VaultStatusPending}, // no key yet
	}}
	got := cfg.depositVaults()
	if len(got) != 3 || got[0] != "0xinactive" || got[1] != "0xactive" || got[2] != "0xpending" {
		t.Fatalf("deposit vaults %v, want [0xinactive 0xactive 0xpending]", got)
	}
	if got := (EVMBotConfig{VaultAddress: "0xa"}).depositVaults(); len(got) != 1 || got[0] != "0xa" {
		t.Fatalf("no registry: %v, want the configured vault", got)
	}
}

// I-28 live shape (testnet 11-04 19:33): the bridge records a generation's key
// only at activation, so a keyed, pending generation reads with no address
// (gen 2: status pending, addr ""). Its MagiDeposit is checked at its TSS key's
// address, so the alarm comes while it still waits to activate; a pending
// generation with no key yet adds nothing.
func TestI28_PendingGenerationIsCheckedAtItsKeyAddress(t *testing.T) {
	l1, l2, gql, rpc := healthWorld(t, 10)
	l2.keys["vsc1bridge-primaryv3"] = [2]string{"active", tssPubG}
	keyAddr, ok := tssPubKeyToAddress(tssPubG)
	if !ok {
		t.Fatal("test key has no address")
	}
	keyAddr = strings.ToLower(keyAddr)
	cfg := EVMBotConfig{VaultGens: []vaultGen{
		{Gen: 2, Status: btcvault.VaultStatusActive, Address: healthVault, KeyID: "vsc1bridge-primaryv2"},
		{Gen: 3, Status: btcvault.VaultStatusPending, KeyID: "vsc1bridge-primaryv3"}, // keyed, not yet in the registry
		{Gen: 4, Status: btcvault.VaultStatusPending, KeyID: "vsc1bridge-primaryv4"}, // no TSS key yet
	}}
	got := cfg.depositVaultsWithPending(context.Background(), gql)
	if len(got) != 2 || got[0] != healthVault || got[1] != keyAddr {
		t.Fatalf("deposit vaults %v, want [%s %s]", got, healthVault, keyAddr)
	}
	if plain := cfg.depositVaults(); len(plain) != 1 {
		t.Fatalf("depositVaults alone %v: the pending generation has no registry address", plain)
	}
	l1.code = map[string]string{depositContractAddress(healthVault): "0x6080"}
	h := newBotHealth(12 * time.Second)
	now := time.Now()
	h.recordTick(now, nil, emptyCP())
	h.check(context.Background(), now, gql, rpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, nil, 0, got)
	r := h.report(now)
	if !hasIssue(r, "I-28") || !hasIssue(r, depositContractAddress(keyAddr)) {
		t.Fatalf("no I-28 alarm naming the pending generation's MagiDeposit %s: %v", depositContractAddress(keyAddr), r.Issues)
	}
}

// I-11, testnet 11-04 04:37: the custody halt was set by a USDC shortfall
// (sis-usdc set, sis-eth clear) and the alarm said "the vault holds less ETH
// than the bridge booked". It now names the short asset(s).
func TestI11_CustodyHaltNamesTheShortAsset(t *testing.T) {
	tokens := map[string]string{healthUSDC: healthUSDC}
	for _, c := range []struct {
		sis     map[string]string
		want    string
		mustNot string
	}{
		{map[string]string{"sis-usdc": "777"}, "USDC at block 777", "ETH at block"},
		{map[string]string{"sis-eth": "776"}, "ETH at block 776", "USDC at block"},
		{map[string]string{"sis-eth": "776", "sis-usdc": "777"}, "ETH at block 776, USDC at block 777", ""},
		{map[string]string{"sis-rft": "778"}, "an asset the bot does not watch is short", "less ETH"},
	} {
		l1, l2, gql, rpc := healthWorld(t, 10)
		l1.tokens = map[string]map[string]*big.Int{healthUSDC: {healthVault: big.NewInt(10_000_000)}}
		l2.set("vsc1bridge", "chainid", "11155111")
		l2.set("vsc1bridge", "t-11155111-"+healthUSDC[2:], "usdc|6|10000000")
		l2.set("vsc1bridge", "s-usdc", "10000000|10000000|0|7")
		l2.set("vsc1bridge", "si", "777")
		for k, v := range c.sis {
			l2.set("vsc1bridge", k, v)
		}
		h := newBotHealth(12 * time.Second)
		now := time.Now()
		h.recordTick(now, nil, emptyCP())
		h.check(context.Background(), now, gql, rpc, "vsc1bridge", []string{"vsc1bridge-primary"}, "did:bot", []string{healthVault}, tokens, 0, nil)
		r := h.report(now)
		if !hasIssue(r, "C2: the bridge's custody halt is set at block 777") || !hasIssue(r, c.want) || (c.mustNot != "" && hasIssue(r, c.mustNot)) {
			t.Fatalf("sis %v: issues %v, want %q (and not %q)", c.sis, r.Issues, c.want, c.mustNot)
		}
	}
}
