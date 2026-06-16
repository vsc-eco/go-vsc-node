package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vsc-node/lib/dids"

	blsAPI "github.com/protolambda/bls12-381-util"
)

// realWitnessFixture is a single witness with a real BLS key + valid PoP
// generated via dids.GenerateBlsPoP. Used wherever we need a verifiable
// fixture rather than a hand-rolled hex string (which the parser would
// reject because it does multibase decode + length checks).
type realWitnessFixture struct {
	account string
	did     dids.BlsDID
	pop     string
}

func newRealWitness(t *testing.T, account string) realWitnessFixture {
	t.Helper()
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	priv := &dids.BlsPrivKey{}
	if err := priv.Deserialize(&seed); err != nil {
		t.Fatalf("priv.Deserialize: %v", err)
	}
	pub, err := blsAPI.SkToPk(priv)
	if err != nil {
		t.Fatalf("SkToPk: %v", err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		t.Fatalf("NewBlsDID: %v", err)
	}
	pop, err := dids.GenerateBlsPoP(priv, account)
	if err != nil {
		t.Fatalf("GenerateBlsPoP: %v", err)
	}
	return realWitnessFixture{account: account, did: did, pop: pop}
}

func fakeGQL(t *testing.T, responseBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling fixture: %v", err)
	}
	return string(b)
}

// witnessFromFixture wires a real BLS key fixture into the witness shape
// the upstream resolver returns, ready for fakeGQL serialisation.
func witnessFromFixture(f realWitnessFixture, enabled bool) witness {
	return witness{
		Account: f.account,
		Enabled: &enabled,
		DidKeys: []key{
			{T: "consensus", Ct: "DID-BLS", Key: f.did.String(), PoP: f.pop},
		},
	}
}

func TestBuildPayload_TwoRealWitnesses(t *testing.T) {
	f1 := newRealWitness(t, "witness1")
	f2 := newRealWitness(t, "witness2")
	witnesses := []witness{
		witnessFromFixture(f1, true),
		witnessFromFixture(f2, true),
	}

	payload, counts, err := buildPayload(witnesses, 65, true)
	if err != nil {
		t.Fatalf("buildPayload: %v (counts=%+v)", err, counts)
	}
	if !strings.HasPrefix(payload, "65;") {
		t.Errorf("payload should start with epoch '65;'; got %q", payload[:min(20, len(payload))])
	}
	parts := strings.Split(payload[len("65;"):], "|")
	if len(parts) != 2 {
		t.Fatalf("expected 2 entries; got %d (payload=%q)", len(parts), payload)
	}
	// Entries are sorted by account; witness1 < witness2 lexicographically.
	if !strings.HasSuffix(parts[0], "=witness1") {
		t.Errorf("first entry should end with =witness1 (lex sort); got %q", parts[0])
	}
	if !strings.HasSuffix(parts[1], "=witness2") {
		t.Errorf("second entry should end with =witness2; got %q", parts[1])
	}
	// Each entry: <did>=<pubkey_96hex>=<pop_192hex>=<account>
	for i, entry := range parts {
		fields := strings.Split(entry, "=")
		if len(fields) != 4 {
			t.Fatalf("entry %d should have 4 fields; got %d (%q)", i, len(fields), entry)
		}
		if !strings.HasPrefix(fields[0], "did:key:z") {
			t.Errorf("entry %d field0 should be a BLS DID; got %q", i, fields[0])
		}
		if len(fields[1]) != 96 {
			t.Errorf("entry %d pubkey field should be 96 hex chars; got len=%d", i, len(fields[1]))
		}
		if len(fields[2]) != 192 {
			t.Errorf("entry %d pop field should be 192 hex chars; got len=%d", i, len(fields[2]))
		}
	}
}

func TestBuildPayload_SkipsDisabledMissingKeyMissingPoP(t *testing.T) {
	f := newRealWitness(t, "active")
	witnesses := []witness{
		witnessFromFixture(f, true),
		// Disabled — should be skipped even with valid key + PoP.
		witnessFromFixture(newRealWitness(t, "disabledw"), false),
		// Enabled but no DID-BLS consensus key.
		{Account: "no-key", Enabled: ptrBool(true), DidKeys: []key{
			{T: "gateway", Ct: "secp256k1", Key: "02abc", PoP: ""},
		}},
		// Enabled, DID-BLS key present, but PoP empty (older binaries).
		{Account: "no-pop", Enabled: ptrBool(true), DidKeys: []key{
			{T: "consensus", Ct: "DID-BLS", Key: f.did.String(), PoP: ""},
		}},
	}

	payload, counts, err := buildPayload(witnesses, 1, true)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if counts.skippedDisabled != 1 {
		t.Errorf("skippedDisabled: got %d want 1", counts.skippedDisabled)
	}
	if counts.skippedNoBLSKey != 1 {
		t.Errorf("skippedNoBLSKey: got %d want 1", counts.skippedNoBLSKey)
	}
	if counts.skippedNoPoP != 1 {
		t.Errorf("skippedNoPoP: got %d want 1", counts.skippedNoPoP)
	}
	if !strings.Contains(payload, "=active") {
		t.Errorf("expected 'active' to survive filtering; payload=%q", payload)
	}
	if strings.Contains(payload, "=disabledw") || strings.Contains(payload, "=no-key") || strings.Contains(payload, "=no-pop") {
		t.Errorf("disabled / no-key / no-pop should not appear in payload; got %q", payload)
	}
}

func TestBuildPayload_ZeroResultErrorsLoud(t *testing.T) {
	witnesses := []witness{
		// Only a disabled witness — buildPayload should refuse.
		witnessFromFixture(newRealWitness(t, "disabledw"), false),
	}
	_, counts, err := buildPayload(witnesses, 1, true)
	if err == nil {
		t.Fatal("expected error on empty payload; got nil (would silently disable all attestations)")
	}
	if !strings.Contains(err.Error(), "refusing to emit") {
		t.Errorf("error should mention 'refusing to emit'; got: %v", err)
	}
	if counts.skippedDisabled != 1 {
		t.Errorf("skippedDisabled: got %d want 1", counts.skippedDisabled)
	}
}

func TestBuildPayload_PoPVerificationCatchesMismatch(t *testing.T) {
	// f.pop is bound to f.account ("alice"). Swap in a different account
	// — VerifyBlsPoP recomputes the canonical message and the sig fails.
	f := newRealWitness(t, "alice")
	withWrongAccount := witnessFromFixture(f, true)
	withWrongAccount.Account = "evilop" // mismatch on purpose
	withWrongAccount.DidKeys[0].PoP = f.pop // PoP bound to "alice", not "evilop"

	_, counts, err := buildPayload([]witness{withWrongAccount}, 1, true)
	if err == nil {
		t.Fatal("expected error: PoP bound to 'alice' should not verify for 'evilop'")
	}
	if counts.skippedPoPInvalid != 1 {
		t.Errorf("skippedPoPInvalid: got %d want 1 (counts=%+v)", counts.skippedPoPInvalid, counts)
	}
}

func TestBuildPayload_VerifyPoPFalseSkipsCheck(t *testing.T) {
	// Same setup as the verification test, but -verifyPoP=false → mismatch
	// is NOT caught. Used by operators who want to mirror contract
	// behaviour bit-for-bit when reproducing historical payloads.
	f := newRealWitness(t, "alice")
	withWrongAccount := witnessFromFixture(f, true)
	withWrongAccount.Account = "evilop"
	withWrongAccount.DidKeys[0].PoP = f.pop

	payload, _, err := buildPayload([]witness{withWrongAccount}, 1, false)
	if err != nil {
		t.Fatalf("with verifyPoP=false, mismatch should NOT error; got %v", err)
	}
	if !strings.Contains(payload, "=evilop") {
		t.Errorf("expected mismatched-account entry to survive when verifyPoP=false; got %q", payload)
	}
}

func TestFetchWitnesses_HappyPath(t *testing.T) {
	f := newRealWitness(t, "w1")
	body := mustJSON(t, map[string]any{
		"data": map[string]any{
			"witnessNodes": []witness{witnessFromFixture(f, true)},
		},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	got, err := fetchWitnesses(context.Background(), srv.URL, 100)
	if err != nil {
		t.Fatalf("fetchWitnesses: %v", err)
	}
	if len(got) != 1 || got[0].Account != "w1" {
		t.Errorf("unexpected witnesses: %+v", got)
	}
}

func TestFetchWitnesses_GraphQLErrorBubbles(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"errors": []map[string]any{{"message": "field witnessNodes does not exist"}},
	})
	srv := fakeGQL(t, body)
	defer srv.Close()

	_, err := fetchWitnesses(context.Background(), srv.URL, 1)
	if err == nil || !strings.Contains(err.Error(), "field witnessNodes does not exist") {
		t.Errorf("expected GraphQL error to bubble up; got %v", err)
	}
}

func TestFetchWitnesses_HTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fetchWitnesses(context.Background(), srv.URL, 1)
	if err == nil || !strings.Contains(err.Error(), "upstream http 500") {
		t.Errorf("expected upstream http 500 to surface; got %v", err)
	}
}

func ptrBool(b bool) *bool { return &b }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
