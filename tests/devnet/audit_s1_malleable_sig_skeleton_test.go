package devnet

import (
	"testing"
)

// TestAuditFix_S1_MalleableSigRejectedByGateway is the devnet shape
// for audit S1. Pre-fix, the gateway sign-collection loop deduped on
// the raw signature string, so a single signer's (r, s) and
// (r, N-s, v^1) submissions counted twice and let a sub-2/3 set of
// signers pass the gateway weight check. Post-fix RecoverPublicKey
// rejects high-S signatures and collectSigs dedups on the recovered
// pubkey.
//
// SKIPPED. Why: triggering the bug in devnet requires a peer that
// publishes a syntactically-valid `sign_response` carrying a malleated
// (r, N-s, v^1) signature on the /gateway/v1 pubsub topic. The honest
// magi binaries WON'T produce this — `hivego.SignCompact` →
// `dcrd/secp256k1.signRFC6979` enforces low-S internally, so honest
// signers always emit canonical sigs. To exercise the malleability
// path end-to-end the test needs one of:
//   - A "rogue signer" libp2p peer wired into the devnet network that
//     knows the /gateway/v1 protocol, joins the active election, and
//     emits both the canonical and malleated sig for a single tx_id.
//   - A test-only build tag on one node that monkey-patches the
//     signing path to emit the malleated twin alongside the canonical
//     sig.
//   - A direct p2p message injection helper (currently absent from
//     tests/devnet/).
//
// The unit-test path
// (modules/gateway/audit_unfixed_internal_test.go
// TestAuditFix_S1_ECDSAMalleabilityRejected) drives RecoverPublicKey
// + the dedup map directly with crafted compact sigs and asserts the
// rejection — that covers the algorithmic invariant.
//
// To make this runnable later:
//   1. Add a "rogue gateway peer" helper in tests/devnet/ that joins
//      the libp2p pubsub mesh and impersonates a witness on a given
//      tx_id.
//   2. Or add an env-var build-tag on magi-N to inject the malleated
//      twin via a test-only override in modules/gateway/multisig.go.
//   3. Trigger a gateway action (withdraw/swap) and assert the
//      weight collection rejects the duplicate-pubkey submission.
func TestAuditFix_S1_MalleableSigRejectedByGateway(t *testing.T) {
	t.Skip("requires rogue libp2p signer or test-only signing override; " +
		"covered by modules/gateway/audit_unfixed_internal_test.go at the " +
		"unit level — honest signers can't produce high-S so devnet drivers " +
		"need explicit injection")
}
