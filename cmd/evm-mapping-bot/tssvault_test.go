package main

import (
	"context"
	"strings"
	"testing"
	"time"
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
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		got, err := waitForTssVault(ctx, l2.gql(t), "vsc1bridge-primary", tc.configured, 20*time.Millisecond)
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
	gql := l2.gql(t)
	go func() {
		time.Sleep(100 * time.Millisecond)
		l2.mu.Lock()
		l2.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
		l2.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := waitForTssVault(ctx, gql, "vsc1bridge-primary", "", 20*time.Millisecond)
	if err != nil || got != tssAddrG {
		t.Fatalf("vault %q err %v, want %s after keygen", got, err, tssAddrG)
	}
}
