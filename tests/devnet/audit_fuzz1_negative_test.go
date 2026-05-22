package devnet

import (
	"testing"
)

// TestAuditFix_FUZZ1_NegativePreFixCodeCrashesOnPoison was the differential
// counterpart to TestAuditFix_FUZZ1_PoisonedGatewayKeyDoesNotCrashRotation:
// run a 5-node devnet against PRE-FIX code (built from
// tibfox/tests/audit-unfixed-meds via Devnet.OldCodeSourceDir), poison a
// witness gateway_key, and assert at least one node crashes on the next
// rotation tick.
//
// SKIPPED. The 5-node devnet baseline cannot exercise the panic path the
// audit calls out: keyRotation in modules/gateway/multisig.go has an
// explicit `if len(gatewayKeys) < 8 { return ..., "not enough keys" }`
// gate that short-circuits BEFORE the serializer (the actual panic site)
// runs. With only 5 witnesses the rotation aborts cleanly on every tick
// regardless of whether one key is poisoned. The audit's CRIT
// "mass-crash exploit" reachability assumes mainnet-size committees
// (mainnet 17, testnet 17 — see user's `magi_witness_count` memory);
// 5-node devnet structurally can't reproduce it.
//
// What was tried (see git history for the deleted behavioural draft):
//   - git worktree at `tibfox/tests/audit-unfixed-meds` → pre-fix source.
//   - Config.OldCodeSourceDir + OldCodeGoImage="golang:1.25.10"
//     (matching the main Dockerfile.devnet's toolchain bump).
//   - 5 nodes all listed in OldCodeNodes so every witness ran pre-fix.
//   - Poisoned magi.test3's gateway_key with "STM" via Mongo.
//   - Waited two rotation ticks. Result: every node alive, no panic
//     in any node's logs — because keyRotation returned
//     "not enough keys" before reaching DecodePublicKey.
//
// What would unblock a real devnet negative test:
//   - Bump cfg.Nodes to 8 (or higher) so gatewayKeys >= 8 after
//     adding the poisoned witness, letting the rotation reach the
//     serializer. Approximately doubles RAM and runtime.
//   - Or expose the gatewayKeys minimum as a sysconfig knob so the
//     devnet can drop it to a small value while keeping production
//     at the mainnet-appropriate 8. Single-line plumbing change.
//
// Negative coverage that already exists for FUZZ-1:
//   - modules/gateway/audit_unfixed_test.go
//     TestAuditUnfixed_FUZZ1_DecodePublicKeyPanics pins the upstream
//     panic on `hivego.DecodePublicKey("STM")` and PASSES on the
//     test-baseline commit (tibfox/tests/audit-unfixed-meds). This is
//     the function-level negative: it asserts the panic surface is
//     reachable pre-fix without any infrastructure work.
//   - TestAuditFix_FUZZ1_SafeValidateGatewayKey_NoPanicOnShortInput
//     (positive) and TestAuditFix_FUZZ1_PoisonedGatewayKeyDoesNotCrashRotation
//     (positive devnet) cover the fixed direction.
//
// Together: function-level negative + positive devnet end-to-end on the
// 5-node default + positive recovery test. The 8-node negative devnet is
// available if someone needs it later; the skip note records exactly
// what to flip.
func TestAuditFix_FUZZ1_NegativePreFixCodeCrashesOnPoison(t *testing.T) {
	t.Skip("requires 8+ nodes to bypass the pre-fix 'not enough keys' " +
		"short-circuit; covered at unit level by " +
		"modules/gateway/audit_unfixed_test.go on tibfox/tests/audit-unfixed-meds")
}
