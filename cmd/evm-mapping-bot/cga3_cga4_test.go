package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/btcvault"
)

// flakyGQL answers with a GraphQL error every query whose body contains one of
// match (every query when match is empty) and passes the rest to next.
func flakyGQL(t *testing.T, next *vscGraphQL, match ...string) *vscGraphQL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fail := len(match) == 0
		for _, m := range match {
			if strings.Contains(string(body), m) {
				fail = true
			}
		}
		if fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"message": "graphql unavailable"}}})
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, next.urls[0], bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return newVSCGraphQL([]string{srv.URL})
}

// CG-A3: a map whose result cannot be read (the node's GraphQL down) is not
// dead-lettered at the timeout; it stays pending, /health says so, and the map's
// real result decides once it can be read.
func TestCGA3_AnUnreadableMapIsNotDeadLettered(t *testing.T) {
	l2 := newFakeL2()
	ctx := context.Background()
	cp := &Checkpoint{SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{
		"maptx1": {Block: 10, DepositTx: "0xdep1", SubmittedAt: time.Now().Add(-31 * time.Minute).Unix()},
	}}
	h := &botHealth{}
	report := func() healthReport {
		h.recordTick(time.Now(), nil, cp)
		return h.report(time.Now())
	}
	if r := report(); !hasIssue(r, "CG-A3") {
		t.Fatalf("a map past the timeout with no result is not reported: %v", r.Issues)
	}

	resolvePendingMaps(ctx, flakyGQL(t, l2.gql(t)), cp)
	if len(cp.DeadLetters) != 0 || len(cp.PendingMaps) != 1 {
		t.Fatalf("status unreadable past the timeout: dead letters %+v, pending %d; want none and 1", cp.DeadLetters, len(cp.PendingMaps))
	}

	l2.status = func(string) string { return "FAILED" }
	resolvePendingMaps(ctx, flakyGQL(t, l2.gql(t), "output { id }"), cp)
	if len(cp.DeadLetters) != 0 || len(cp.PendingMaps) != 1 {
		t.Fatalf("FAILED, reason unreadable past the timeout: dead letters %+v, pending %d; want none and 1", cp.DeadLetters, len(cp.PendingMaps))
	}
	if r := report(); !hasIssue(r, "CG-A3") || r.Status != "unhealthy" {
		t.Fatalf("/health while the result is unreadable: %s %v", r.Status, r.Issues)
	}

	l2.status = nil // readable again: the map landed
	resolvePendingMaps(ctx, l2.gql(t), cp)
	if len(cp.DeadLetters) != 0 || len(cp.PendingMaps) != 0 {
		t.Fatalf("readable and CONFIRMED: dead letters %+v, pending %d; want none and 0", cp.DeadLetters, len(cp.PendingMaps))
	}
	if r := report(); hasIssue(r, "CG-A3") {
		t.Fatalf("resolved map still reported: %v", r.Issues)
	}
}

// The timeout paths that do decide are unchanged: a map the node does not know
// is mapped again (I-46), one still not final is the operator's.
func TestCGA3_DecidedTimeoutsUnchanged(t *testing.T) {
	l2 := newFakeL2()
	ctx := context.Background()
	old := time.Now().Add(-31 * time.Minute).Unix()
	cp := &Checkpoint{SentWithdrawals: map[string]SentTx{}, BlockRetries: map[uint64]int{}, PendingMaps: map[string]PendingMap{
		"maptx1": {Block: 10, DepositTx: "0xdep1", SubmittedAt: old},
	}}
	l2.status = func(string) string { return "-" } // the fake's "not found"
	resolvePendingMaps(ctx, l2.gql(t), cp)
	if len(cp.PendingMaps) != 0 || len(cp.DeadLetters) != 1 || !cp.DeadLetters[0].AutoRetry {
		t.Fatalf("not found past the timeout: pending %d, dead letters %+v; want the bot's own retry", len(cp.PendingMaps), cp.DeadLetters)
	}
	cp.PendingMaps["maptx2"] = PendingMap{Block: 11, DepositTx: "0xdep2", SubmittedAt: old}
	l2.status = func(string) string { return "UNCONFIRMED" }
	resolvePendingMaps(ctx, l2.gql(t), cp)
	if len(cp.PendingMaps) != 0 || len(cp.DeadLetters) != 2 || cp.DeadLetters[1].AutoRetry {
		t.Fatalf("not final past the timeout: pending %d, dead letters %+v; want an operator dead letter", len(cp.PendingMaps), cp.DeadLetters)
	}
}

// CG-A4: after a rotation the bot starts from the registry's active generation,
// whatever state generation 0's key is in.
func TestCGA4_StartsFromTheActiveGenerationAfterARotation(t *testing.T) {
	reg := string(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive}))
	for _, status := range []string{"retired", "deleted", "active"} {
		l2 := newFakeL2()
		l2.keys["vsc1bridge-primary"] = [2]string{status, tssPubG}
		l2.set("vsc1bridge", "v", reg)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		got, err := waitForTssVault(ctx, l2.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
		cancel()
		if err != nil || got != rotAddr(2) {
			t.Fatalf("gen 0 key %s: vault %q err %v, want the active generation %s", status, got, err, rotAddr(2))
		}
	}

	l2 := newFakeL2()
	l2.keys["vsc1bridge-primary"] = [2]string{"retired", tssPubG}
	l2.set("vsc1bridge", "v", reg)
	for _, tc := range []struct {
		configured string
		wantErr    bool
	}{
		{rotAddr(2), false}, // the active generation
		{rotAddr(1), false}, // an older fund-holding generation: used as the active one
		{"0x00000000000000000000000000000000000000fa", true},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		got, err := waitForTssVault(ctx, l2.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", tc.configured, 20*time.Millisecond)
		cancel()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "VAULT_ADDRESS") {
				t.Fatalf("VAULT_ADDRESS=%s: vault %q err %v, want a refusal", tc.configured, got, err)
			}
			continue
		}
		if err != nil || got != rotAddr(2) {
			t.Fatalf("VAULT_ADDRESS=%s: vault %q err %v, want %s", tc.configured, got, err, rotAddr(2))
		}
	}
}

// A registry that cannot be read never refuses and never binds: before the fix's
// path, the old one still serves a live gen 0 key, and a retired one waits.
func TestCGA4_UnreadableRegistryFallsBackToTheKey(t *testing.T) {
	reg := string(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusDraining}, rotEntry{1, 2, btcvault.VaultStatusActive}))
	l2 := newFakeL2()
	l2.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
	l2.set("vsc1bridge", "vaultok", tssPubG)
	l2.set("vsc1bridge", "v", reg)
	l2.failState = map[string]bool{"vsc1bridge": true}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	_, err := waitForTssVault(ctx, l2.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
	cancel()
	if err != context.DeadlineExceeded {
		t.Fatalf("every state read failing (registry and vaultok): err %v, want it to keep waiting", err)
	}

	l2.keys["vsc1bridge-primary"] = [2]string{"retired", tssPubG}
	l2.failState = nil
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	got, err := waitForTssVault(ctx, l2.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
	cancel()
	if err != nil || got != rotAddr(2) {
		t.Fatalf("registry readable again: vault %q err %v, want %s", got, err, rotAddr(2))
	}

	gen := newFakeL2() // a fresh registry with only a Pending generation: the key path as before
	gen.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
	gen.set("vsc1bridge", "vaultok", tssPubG)
	gen.set("vsc1bridge", "v", string(rotRegistry(rotEntry{0, 1, btcvault.VaultStatusPending})))
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	got, err = waitForTssVault(ctx, gen.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
	cancel()
	if err != nil || got != tssAddrG {
		t.Fatalf("registry with no Active generation: vault %q err %v, want the key's address %s", got, err, tssAddrG)
	}

	pre := newFakeL2() // a bridge before its first rotation: no registry, the key path as before
	pre.keys["vsc1bridge-primary"] = [2]string{"active", tssPubG}
	pre.set("vsc1bridge", "vaultok", tssPubG)
	ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
	got, err = waitForTssVault(ctx, pre.gql(t), nil, "vsc1bridge", "vsc1bridge-primary", "", 20*time.Millisecond)
	cancel()
	if err != nil || got != tssAddrG {
		t.Fatalf("pre-rotation bridge: vault %q err %v, want the key's address %s", got, err, tssAddrG)
	}
}
