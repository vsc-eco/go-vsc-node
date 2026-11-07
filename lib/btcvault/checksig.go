package btcvault

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
)

// BRK-2 (check-SIGNATURE-before-activate). Today a fresh vault generation
// activates on keygen AGREEMENT alone — the committee agreed a pubkey, but
// nothing ever proves the new committee can PRODUCE a signature with it. Funds
// could then route into an agreed-but-UNSIGNABLE vault and custody collapses to
// the single CSV backup key (brick council FS3-1). The cure: the new key must
// sign a canonical, replay-bound message M whose signature is verified on-chain
// (reusing the existing deterministic vsc.tss_sign secp256k1 verify) before the
// contract will flip the generation Active.
//
// M lives here, in the leaf btcvault lib, so the SAME bytes are computed at BOTH
// the node scope gate (modules/tss scopeCheckSig — a Pending gen may sign
// NOTHING but M) and the node flag-set handler (modules/state-processing
// vsc.tss_sign — a landed sig over M sets SignatureVerified). One source of
// truth = no cross-file drift.

// CheckSigDomainTag is the 22-byte domain-separation prefix of the check-sig
// preimage. It guarantees M can NEVER equal a real BTC sighash (a
// double-SHA256 over a serialized tx — structurally not this preimage) or any
// other signable target, so a check-sign can never be repurposed as a
// withdrawal/migration signature, or vice-versa. Repurposing would need a
// 2^256 preimage collision AND a Pending gen (which holds zero UTXOs).
const CheckSigDomainTag = "MAGI-VAULT-CHECKSIG-v2" // len 22

// CheckSigMessage returns the canonical 32-byte check-signature digest M that a
// freshly keygen'd vault key must produce a signature over before its
// generation may be activated:
//
//	M = SHA256( CheckSigDomainTag(22B)
//	           ‖ uint16_BE(len(keyId)) ‖ keyId
//	           ‖ uint32_BE(gen)
//	           ‖ pubkey )
//
// Fixed-width and length-delimited, so there is no concatenation ambiguity
// between fields. The pubkey MUST come from the COMMITTED tss_keys row (the node
// keystore, written only on the BLS-threshold-verified state-processing path),
// NEVER from a contract field — a compromised/lying contract must not be able to
// choose the message a key signs (the C-C guard). Binding keyId (which itself
// encodes the generation) AND gen makes M unique per generation, so a valid
// check-sig for gen N can never satisfy gen N+1's gate.
func CheckSigMessage(keyId string, gen uint32, pubkey []byte) []byte {
	buf := make([]byte, 0, len(CheckSigDomainTag)+2+len(keyId)+4+len(pubkey))
	buf = append(buf, CheckSigDomainTag...)

	var klen [2]byte
	binary.BigEndian.PutUint16(klen[:], uint16(len(keyId)))
	buf = append(buf, klen[:]...)
	buf = append(buf, keyId...)

	var genBytes [4]byte
	binary.BigEndian.PutUint32(genBytes[:], gen)
	buf = append(buf, genBytes[:]...)

	buf = append(buf, pubkey...)

	sum := sha256.Sum256(buf)
	return sum[:]
}

// VaultGenFromKeyId reverses the full node keyId ("<contractId>-" +
// VaultKeyName(gen)) into its generation. Returns (gen, true) only for a
// recognised BTC-vault key name; (0, false) otherwise (wrong contract prefix, or
// a name that is not a vault key). Both the node gate and the flag-set handler
// call this with their own BTC contract id so M binds the identical gen on every
// node.
func VaultGenFromKeyId(contractId, keyId string) (uint32, bool) {
	if contractId == "" {
		return 0, false
	}
	prefix := contractId + "-"
	if !strings.HasPrefix(keyId, prefix) {
		return 0, false
	}
	return VaultGenFromKeyName(keyId[len(prefix):])
}

// VaultGenFromKeyName is the strict inverse of VaultKeyName: "main" =>
// generation 0, "mainv<N>" => generation N (N a canonical base-10 uint32).
// Anything else => (0, false). It rejects non-canonical forms ("mainv0",
// "mainv01", "mainv", leading zeros) so it is an EXACT inverse — VaultKeyName
// never emits a leading zero and encodes generation 0 as "main", never
// "mainv0".
func VaultGenFromKeyName(name string) (uint32, bool) {
	if name == "main" {
		return 0, true
	}
	const p = "mainv"
	if !strings.HasPrefix(name, p) {
		return 0, false
	}
	num := name[len(p):]
	if num == "" || num[0] == '0' { // no leading zero; gen 0 is "main", not "mainv0"
		return 0, false
	}
	n, err := strconv.ParseUint(num, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}
