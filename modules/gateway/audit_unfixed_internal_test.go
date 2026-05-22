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

// TestAuditUnfixed_S1_ECDSAMalleabilityBreaksDedup proves the bug-precondition
// for audit item S1 (gateway/multisig.go:820-829 collectSigs dedup).
//
// The current dedup key is the raw signature string. Because secp256k1 ECDSA
// signatures are malleable — (r, N-s) with the recovery-id bit flipped recovers
// the SAME public key — a single signer can submit two distinct signature
// strings that both verify under their own pubkey. The string-equality check
// in `slices.Contains(sigs, sigRes.Sig)` then fails to dedup, so the signer's
// weight is counted twice.
//
// Canonical fix patterns (cited in the audit; landed upstream):
//   - modules/tss/tss.go (signedAccounts map keyed by recovered account)
//   - modules/state-processing/state_engine.go (IsOverHalfOrder rejects high-S)
func TestAuditUnfixed_S1_ECDSAMalleabilityBreaksDedup(t *testing.T) {
	// 1. Deterministic key + hash.
	seed := sha256.Sum256([]byte("audit-S1-malleability-test-seed"))
	prvKey, _ := secp256k1.PrivKeyFromBytes(seed[:])

	msg := []byte("withdraw bundle: bh=1234567 ops=[]")
	txHash := sha256.Sum256(msg)

	// 2. Canonical compact signature (compressed=true to match
	//    RecoverPublicKey's caller use in collectSigs).
	sigBytes, err := secp256k1.SignCompact(prvKey, txHash[:], true)
	if err != nil {
		t.Fatalf("SignCompact: %v", err)
	}
	sigA := hex.EncodeToString(sigBytes)

	// 3. Malleate. Compact layout (dcrd secp256k1/v2):
	//      byte[0]       = 27 + iter + (4 if compressed)
	//      byte[1..33]   = R (32 bytes, big-endian, left-padded)
	//      byte[33..65]  = S (32 bytes, big-endian, left-padded)
	//    Negate S mod N and XOR iter's low bit — same recovered pubkey.
	N := secp256k1.S256().Params().N
	bitlen := (secp256k1.S256().BitSize + 7) / 8 // 32

	mallBytes := make([]byte, len(sigBytes))
	copy(mallBytes, sigBytes)

	header := mallBytes[0] - 27
	compressedFlag := header & 4
	iter := header & ^byte(4)
	newIter := iter ^ 1
	mallBytes[0] = 27 + (newIter | compressedFlag)

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

	// 4. Both must recover to the same pubkey. This is the precondition.
	pubA, err := RecoverPublicKey(sigA, txHash[:])
	if err != nil {
		t.Fatalf("RecoverPublicKey(sigA): %v", err)
	}
	pubB, err := RecoverPublicKey(sigB, txHash[:])
	if err != nil {
		t.Fatalf("RecoverPublicKey(sigB): %v — malleation arithmetic wrong", err)
	}
	if pubA != pubB {
		t.Fatalf("malleated sig did not recover same pubkey:\n  pubA=%s\n  pubB=%s", pubA, pubB)
	}

	// 5. Simulate the dedup loop at multisig.go:820-829. The current code
	//    keys dedup on the raw signature string, NOT on the recovered pubkey.
	//    So sigB sails past the check even though it represents the same
	//    signer's vote as sigA.
	sigs := []string{sigA}
	if slices.Contains(sigs, sigB) {
		t.Fatalf("UNEXPECTED: dedup-by-string already filters malleated form — bug may be fixed; update or remove this test")
	}
	// In the buggy path, the second entry is appended and the signer's
	// weight gets counted twice.
	sigs = append(sigs, sigB)
	if len(sigs) != 2 {
		t.Fatalf("expected two entries in dedup slice, got %d", len(sigs))
	}

	// Post-fix expectation:
	//   Key dedup on RecoverPublicKey(sig, txHash), not on sig string;
	//   additionally reject any sig whose S > N/2 (low-S enforcement).
	t.Logf("S1 confirmed: two distinct signature strings recover to same pubkey %q; "+
		"string-keyed dedup admits both. Fix: key dedup on recovered pubkey "+
		"(see modules/tss/tss.go signedAccounts pattern) AND reject high-S "+
		"(see state-processing/state_engine.go IsOverHalfOrder).", pubA)
}

// TestAuditUnfixed_S7_ValidateMessageAcceptsAllSenders proves audit item S7:
// gateway/p2p.go:38-41 — ValidateMessage unconditionally returns true. There
// is no leader / committee gating, so any peer can publish any payload on the
// /gateway/v1 topic and force every receiver into HandleMessage's signing
// path. This is the DoS amplification precondition.
//
// Post-fix expectation: ValidateMessage must check that `from` is either the
// current-slot leader (for sign_request) or a member of the active gateway
// committee (for sign_response). Spam from outside the committee should be
// rejected at validation time, before any signing work is queued.
func TestAuditUnfixed_S7_ValidateMessageAcceptsAllSenders(t *testing.T) {
	spec := p2pSpec{} // ms is nil but ValidateMessage doesn't dereference it.

	ctx := context.Background()
	pubsubMsg := &pubsub.Message{}

	cases := []struct {
		name string
		from peer.ID
		msg  p2pMessage
	}{
		{
			name: "random peer, sign_request key_rotation",
			from: peer.ID("random-peer-A"),
			msg:  p2pMessage{Type: "sign_request", Op: "key_rotation", Data: "{}"},
		},
		{
			name: "empty peer id, sign_response",
			from: peer.ID(""),
			msg:  p2pMessage{Type: "sign_response", Data: `{"tx_id":"x","sig":"y"}`},
		},
		{
			name: "random peer, sign_request execute_actions",
			from: peer.ID("attacker-peer"),
			msg:  p2pMessage{Type: "sign_request", Op: "execute_actions", Data: "{}"},
		},
		{
			name: "completely unknown type",
			from: peer.ID("anyone"),
			msg:  p2pMessage{Type: "garbage", Op: "garbage", Data: "garbage"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := spec.ValidateMessage(ctx, tc.from, pubsubMsg, tc.msg)
			if !got {
				t.Fatalf("UNEXPECTED: ValidateMessage rejected %q — bug may be fixed; "+
					"update this test to reflect the new gating rule", tc.name)
			}
		})
	}

	t.Log("S7 confirmed: ValidateMessage returns true for arbitrary sender + payload. " +
		"Fix: reject from-peer when not in active gateway committee, " +
		"and (for sign_request) when not the current slot leader.")
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
