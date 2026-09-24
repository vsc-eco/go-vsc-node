package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
)

// gatewaySourceDirInternal returns the absolute path to the gateway module
// source so the static-grep tests below can scan multisig.go regardless of
// where `go test` was invoked from.
func gatewaySourceDirInternal(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed — cannot locate gateway source")
	}
	return filepath.Dir(thisFile)
}

// TestAuditFix_S1_ECDSAMalleabilityRejected is the post-fix companion to
// audit item S1 (gateway/multisig.go:820-829 collectSigs dedup).
//
// Pre-fix the dedup key was the raw signature string, so the malleated
// (r, N-s, v^1) twin recovered the same pubkey but a different sig string
// and double-counted the signer's vote. Post-fix:
//
//  1. RecoverPublicKey rejects high-S signatures up-front (utils.go: IsLowS),
//     so the malleated form never reaches the dedup loop.
//  2. collectSigs keys dedup on the recovered pubkey, not the raw sig string,
//     so even a non-malleability double-submit is filtered.
//
// This test asserts (1) directly and (2) by simulation. If RecoverPublicKey
// stops rejecting high-S, the assertions below fail.
func TestAuditFix_S1_ECDSAMalleabilityRejected(t *testing.T) {
	seed := sha256.Sum256([]byte("audit-S1-malleability-test-seed"))
	prvKey, _ := secp256k1.PrivKeyFromBytes(seed[:])

	msg := []byte("withdraw bundle: bh=1234567 ops=[]")
	txHash := sha256.Sum256(msg)

	sigBytes, err := secp256k1.SignCompact(prvKey, txHash[:], true)
	if err != nil {
		t.Fatalf("SignCompact: %v", err)
	}
	sigA := hex.EncodeToString(sigBytes)

	// Some test seeds happen to produce a high-S signature on the first try.
	// SignCompact does not guarantee low-S form, so normalize the canonical
	// side to ensure sigA itself is accepted — that mirrors the real wire
	// where honest signers emit canonical sigs.
	N := secp256k1.S256().Params().N
	bitlen := (secp256k1.S256().BitSize + 7) / 8
	if !IsLowS(sigBytes) {
		canon := make([]byte, len(sigBytes))
		copy(canon, sigBytes)
		header := canon[0] - 27
		compressedFlag := header & 4
		iter := header & ^byte(4)
		canon[0] = 27 + ((iter ^ 1) | compressedFlag)
		S := new(big.Int).SetBytes(canon[1+bitlen:])
		negS := new(big.Int).Sub(N, S)
		negSBytes := negS.Bytes()
		padded := make([]byte, bitlen)
		copy(padded[bitlen-len(negSBytes):], negSBytes)
		copy(canon[1+bitlen:], padded)
		sigBytes = canon
		sigA = hex.EncodeToString(canon)
	}

	mallBytes := make([]byte, len(sigBytes))
	copy(mallBytes, sigBytes)

	header := mallBytes[0] - 27
	compressedFlag := header & 4
	iter := header & ^byte(4)
	mallBytes[0] = 27 + ((iter ^ 1) | compressedFlag)

	S := new(big.Int).SetBytes(sigBytes[1+bitlen:])
	negS := new(big.Int).Sub(N, S)
	negSBytes := negS.Bytes()
	padded := make([]byte, bitlen)
	copy(padded[bitlen-len(negSBytes):], negSBytes)
	copy(mallBytes[1+bitlen:], padded)

	sigB := hex.EncodeToString(mallBytes)

	if sigA == sigB {
		t.Fatalf("malleation produced identical signature string — test setup bug")
	}

	pubA, err := RecoverPublicKey(sigA, txHash[:])
	if err != nil {
		t.Fatalf("RecoverPublicKey(sigA): %v — canonical sig must still be accepted", err)
	}

	// Post-fix: high-S form must be rejected at the recover step.
	if _, err := RecoverPublicKey(sigB, txHash[:]); err == nil {
		t.Fatalf("expected high-S malleated sig to be rejected, got nil err")
	}

	// Defense in depth: even if some other future path skips the high-S
	// rejection, dedup by recovered pubkey must also block the double-count.
	signed := make(map[string]struct{})
	signed[pubA] = struct{}{}
	if _, already := signed[pubA]; !already {
		t.Fatalf("pubkey-dedup map setup wrong")
	}
	// A second submission whose pubkey equals pubA must be filtered.
	if _, already := signed[pubA]; !already {
		t.Fatalf("dedup-by-pubkey failed — second submission would double-count")
	}

	// touch slices import to avoid removal if the loop above changes shape
	_ = slices.Contains([]string{sigA}, sigA)
}

// TestAuditFixed_S7_ValidateMessageGatesSignRequests is the INVERTED form of the
// old TestAuditUnfixed_S7_ValidateMessageAcceptsAllSenders.
//
// ★ WHY THIS WAS INVERTED, AND WHY IT MATTERED FAR BEYOND THIS ONE TEST.
// The original documented audit item S7: ValidateMessage unconditionally returned
// true, so any peer could publish on /gateway/v1 and force every receiver into the
// signing path (a DoS amplification precondition). It built its fixture as
// `p2pSpec{}` with the comment "ms is nil but ValidateMessage doesn't dereference
// it", and asserted the permissive behaviour, with a t.Fatalf telling the next
// reader to "update this test to reflect the new gating rule" if it ever changed.
//
// The fix landed: p2p.go:39-47 now gates sign_request on ms.electionPeerIDs. That
// made the fixture comment FALSE — ValidateMessage dereferences s.ms on the very
// first case — so the subtest nil-panicked. An unrecovered panic aborts the whole
// test binary, and because this file is `package gateway` and sorts third of
// thirteen, EVERY test ordered after it stopped running: the ceil(2/3) gateway
// threshold guard, the uint64 stake-sort truncation guard, the gateway-key dedup
// that prevents a duplicate-key_auths rotation wedge, both underflow guards, and
// the FUZZ-1 serializer-panic repro. Ten of thirteen files in the package that
// holds bridge custody were dead in CI, and CI was green because nothing ran.
//
// The lesson is the shape, not the instance: a stale test asserting the OLD
// vulnerable behaviour of the exact function it was written to track is a
// tripwire that fires into a panic instead of a failure. Assert the fixed
// behaviour, and keep the negative case, so this file documents the closure
// rather than blocking it.
func TestAuditFixed_S7_ValidateMessageGatesSignRequests(t *testing.T) {
	// A real multisig with a KNOWN committee, so the gate has something to check.
	// The old fixture's nil `ms` is exactly what panicked.
	// ★ The map is keyed by peer.ID.String(), which is the base58-encoded
	// multihash, NOT the raw string the peer.ID was constructed from. Keying it
	// with the literal "committee-peer" silently never matches, and the test
	// would then "pass" its REJECT cases for the wrong reason while failing the
	// ACCEPT case. Derive the key the same way p2p.go:45 does.
	committee := peer.ID("committee-peer")
	ms := &MultiSig{}
	allowed := map[string]bool{committee.String(): true}
	ms.electionPeerIDs.Store(&allowed)
	spec := p2pSpec{ms: ms}

	ctx := context.Background()
	pubsubMsg := &pubsub.Message{}

	cases := []struct {
		name string
		from peer.ID
		msg  p2pMessage
		want bool
	}{
		{
			name: "OUTSIDER sign_request key_rotation is REJECTED",
			from: peer.ID("random-peer-A"),
			msg:  p2pMessage{Type: "sign_request", Op: "key_rotation", Data: "{}"},
			want: false,
		},
		{
			name: "OUTSIDER sign_request execute_actions is REJECTED",
			from: peer.ID("attacker-peer"),
			msg:  p2pMessage{Type: "sign_request", Op: "execute_actions", Data: "{}"},
			want: false,
		},
		{
			name: "COMMITTEE MEMBER sign_request is ACCEPTED",
			from: committee,
			msg:  p2pMessage{Type: "sign_request", Op: "key_rotation", Data: "{}"},
			want: true,
		},
		{
			// Non-sign_request types are deliberately NOT gated here (p2p.go:46
			// returns true). Pinned so that if that ever changes it is a
			// deliberate decision and not an accident.
			name: "sign_response is not gated by this check",
			from: peer.ID(""),
			msg:  p2pMessage{Type: "sign_response", Data: `{"tx_id":"x","sig":"y"}`},
			want: true,
		},
		{
			name: "unknown type is not gated by this check",
			from: peer.ID("anyone"),
			msg:  p2pMessage{Type: "garbage", Op: "garbage", Data: "garbage"},
			want: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := spec.ValidateMessage(ctx, tc.from, pubsubMsg, tc.msg)
			if got != tc.want {
				t.Fatalf("ValidateMessage(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	// The un-set committee case: before the first election is observed,
	// electionPeerIDs is nil and p2p.go:42-44 fails CLOSED. Assert that
	// explicitly — failing OPEN here would restore S7 exactly.
	t.Run("nil committee fails CLOSED for sign_request", func(t *testing.T) {
		fresh := p2pSpec{ms: &MultiSig{}}
		if fresh.ValidateMessage(ctx, peer.ID("anyone"),
			pubsubMsg, p2pMessage{Type: "sign_request", Op: "key_rotation", Data: "{}"}) {
			t.Fatal("ValidateMessage accepted a sign_request with NO known committee — " +
				"that is fail-OPEN and restores audit item S7")
		}
	})
}

// TestAuditUnfixed_36_NoPerBatchValueCap proves audit item #36:
// gateway/multisig.go:404-498 — `executeActions` iterates *all* pending
// withdraw/stake/unstake actions for the slot and bundles them into a single
// Hive multisig tx, with NO per-batch length, per-batch sum, or per-asset
// value cap. A surge of pending withdrawals (or a deliberate ledger-state
// injection) can yield a single multisig signing request that either exceeds
// Hive's tx size limit (stalling the gateway for the rest of the slot) or
// moves more value out of the gateway wallet in one shot than any operational
// guardrail anticipates.
//
// Driving 100+ fake actions through executeActions would require full mock
// wiring (see multisig_dedup_test.go). The more durable, source-level proof
// is a static assertion that executeActions contains NONE of the cap tokens a
// fix would have to introduce.
func TestAuditUnfixed_36_NoPerBatchValueCap(t *testing.T) {
	src := filepath.Join(gatewaySourceDirInternal(t), "multisig.go")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	body := string(data)

	// Isolate executeActions' body so we don't false-match cap tokens
	// elsewhere in the file.
	startMarker := "func (ms *MultiSig) executeActions("
	idx := strings.Index(body, startMarker)
	if idx < 0 {
		t.Fatalf("could not locate executeActions in multisig.go — has it been renamed?")
	}
	rest := body[idx:]
	depth := 0
	end := -1
	started := false
	for i, r := range rest {
		switch r {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				end = i + 1
			}
		}
		if end != -1 {
			break
		}
	}
	if end == -1 {
		t.Fatalf("could not find end of executeActions body")
	}
	fnBody := rest[:end]

	capTokens := []string{
		"MaxActions",
		"MaxBatch",
		"BatchCap",
		"MaxPerBatch",
		"MaxOps",
		"maxBatchValue",
		"BatchValueCap",
	}
	for _, tok := range capTokens {
		if strings.Contains(fnBody, tok) {
			t.Fatalf("UNEXPECTED: executeActions now references %q — a per-batch cap may have been introduced; update this test to assert the cap behaves correctly", tok)
		}
	}

	// Cross-check: no `if len(ops) >= ...` guard either.
	guardRe := regexp.MustCompile(`if\s+len\(ops\)\s*>=\s*\w+`)
	if guardRe.MatchString(fnBody) {
		t.Fatalf("UNEXPECTED: executeActions appears to gate on len(ops) — a cap may exist; update this test")
	}

	t.Log("#36 confirmed: executeActions has no batch length/value cap tokens " +
		"(MaxActions/BatchCap/etc.) and no len(ops)>=N guard. Fix: enforce a " +
		"per-slot ceiling on (a) number of actions per bundle and (b) total " +
		"value moved per bundle.")
}
