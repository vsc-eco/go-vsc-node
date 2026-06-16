package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	bls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genBlsKey produces a fresh deterministic BLS keypair from secure random.
// Returns (privkey, pubkey hex-encoded, pubkey raw 48 bytes).
func genBlsKey(t *testing.T) (*bls.SecretKey, string, []byte) {
	t.Helper()
	var skBytes [32]byte
	_, err := rand.Read(skBytes[:])
	require.NoError(t, err)
	var sk bls.SecretKey
	require.NoError(t, sk.Deserialize(&skBytes))
	pk, err := bls.SkToPk(&sk)
	require.NoError(t, err)
	pkRaw := pk.Serialize()
	return &sk, hex.EncodeToString(pkRaw[:]), pkRaw[:]
}

func signMsg(t *testing.T, sk *bls.SecretKey, msg []byte) string {
	t.Helper()
	sig := bls.Sign(sk, msg)
	raw := sig.Serialize()
	return hex.EncodeToString(raw[:])
}

func callBlsVerify(t *testing.T, pubkeyHex, msgHex, sigHex string) SdkResult {
	t.Helper()
	fn := SdkNamespaces["crypto"]["bls_verify"].(func(context.Context, any, any, any) SdkResult)
	return fn(context.Background(), pubkeyHex, msgHex, sigHex)
}

func callBlsVerifyAggregate(t *testing.T, pubkeysHex, msgHex, aggSigHex string) SdkResult {
	t.Helper()
	fn := SdkNamespaces["crypto"]["bls_verify_aggregate"].(func(context.Context, any, any, any) SdkResult)
	return fn(context.Background(), pubkeysHex, msgHex, aggSigHex)
}

// ===== single-pubkey bls_verify =====

func TestBlsVerify_HappyPath(t *testing.T) {
	sk, pkHex, _ := genBlsKey(t)
	msg := []byte("vsc dash islock attestation v1")
	sig := signMsg(t, sk, msg)

	res := callBlsVerify(t, pkHex, hex.EncodeToString(msg), sig)
	require.False(t, res.IsErr(), "verify must not error: %v", res)
	assert.Equal(t, "true", res.Unwrap().Result)
}

func TestBlsVerify_WrongKey(t *testing.T) {
	sk1, _, _ := genBlsKey(t)
	_, pk2Hex, _ := genBlsKey(t)
	msg := []byte("wrong-key test")
	sig := signMsg(t, sk1, msg)

	// Sign with sk1 but claim pk2 — must NOT verify.
	res := callBlsVerify(t, pk2Hex, hex.EncodeToString(msg), sig)
	require.False(t, res.IsErr())
	assert.Equal(t, "false", res.Unwrap().Result)
}

func TestBlsVerify_WrongMessage(t *testing.T) {
	sk, pkHex, _ := genBlsKey(t)
	msg1 := []byte("message one")
	msg2 := []byte("message two")
	sig := signMsg(t, sk, msg1)

	res := callBlsVerify(t, pkHex, hex.EncodeToString(msg2), sig)
	require.False(t, res.IsErr())
	assert.Equal(t, "false", res.Unwrap().Result)
}

func TestBlsVerify_MalformedPubkeyHex(t *testing.T) {
	res := callBlsVerify(t, "not-hex", "deadbeef", strings.Repeat("00", 96))
	assert.True(t, res.IsErr())
}

func TestBlsVerify_WrongPubkeyLength(t *testing.T) {
	// 40 bytes instead of 48
	res := callBlsVerify(t, strings.Repeat("00", 40), "deadbeef", strings.Repeat("00", 96))
	assert.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "48 bytes")
}

func TestBlsVerify_WrongSigLength(t *testing.T) {
	_, pkHex, _ := genBlsKey(t)
	// 80 bytes instead of 96
	res := callBlsVerify(t, pkHex, "deadbeef", strings.Repeat("00", 80))
	assert.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "96 bytes")
}

func TestBlsVerify_GasReported(t *testing.T) {
	sk, pkHex, _ := genBlsKey(t)
	msg := []byte("gas check")
	sig := signMsg(t, sk, msg)

	res := callBlsVerify(t, pkHex, hex.EncodeToString(msg), sig)
	require.False(t, res.IsErr())
	gas := res.Unwrap().Gas
	assert.Greater(t, gas, uint(0), "gas must be reported (used by RC accounting)")
}

// ===== aggregate bls_verify_aggregate =====

func TestBlsVerifyAggregate_HappyPath(t *testing.T) {
	// Simulate a 5-validator quorum signing the same canonical message.
	const N = 5
	sks := make([]*bls.SecretKey, N)
	var pkBlob strings.Builder
	msg := []byte("aggregate happy path: dash-is-lock-v1 message")
	sigs := make([]*bls.Signature, N)

	for i := 0; i < N; i++ {
		var pkHex string
		sks[i], pkHex, _ = genBlsKey(t)
		pkBlob.WriteString(pkHex)
		sigs[i] = bls.Sign(sks[i], msg)
	}

	aggSig, err := bls.Aggregate(sigs)
	require.NoError(t, err)
	aggSigRaw := aggSig.Serialize()
	aggSigHex := hex.EncodeToString(aggSigRaw[:])

	res := callBlsVerifyAggregate(t, pkBlob.String(), hex.EncodeToString(msg), aggSigHex)
	require.False(t, res.IsErr(), "aggregate verify must not error: %v", res)
	assert.Equal(t, "true", res.Unwrap().Result)
}

func TestBlsVerifyAggregate_SingleKey(t *testing.T) {
	// Degenerate case: aggregate over exactly 1 key. Must work.
	sk, pkHex, _ := genBlsKey(t)
	msg := []byte("single-key aggregate")
	sig := bls.Sign(sk, msg)
	sigRaw := sig.Serialize()
	sigHex := hex.EncodeToString(sigRaw[:])

	res := callBlsVerifyAggregate(t, pkHex, hex.EncodeToString(msg), sigHex)
	require.False(t, res.IsErr())
	assert.Equal(t, "true", res.Unwrap().Result)
}

func TestBlsVerifyAggregate_WrongQuorumMember(t *testing.T) {
	// One signer who isn't in the claimed pubkey set should make it fail.
	const N = 3
	var pkBlob strings.Builder
	msg := []byte("quorum membership test")
	sigs := make([]*bls.Signature, N)

	for i := 0; i < N; i++ {
		var pkHex string
		var sk *bls.SecretKey
		sk, pkHex, _ = genBlsKey(t)
		pkBlob.WriteString(pkHex)
		sigs[i] = bls.Sign(sk, msg)
	}

	// Now replace sigs[1] with a sig from a non-member key.
	imposterSk, _, _ := genBlsKey(t)
	sigs[1] = bls.Sign(imposterSk, msg)

	aggSig, err := bls.Aggregate(sigs)
	require.NoError(t, err)
	aggSigRaw := aggSig.Serialize()

	res := callBlsVerifyAggregate(t, pkBlob.String(), hex.EncodeToString(msg), hex.EncodeToString(aggSigRaw[:]))
	require.False(t, res.IsErr())
	assert.Equal(t, "false", res.Unwrap().Result, "imposter sig must break aggregate verification")
}

func TestBlsVerifyAggregate_DifferentMessages(t *testing.T) {
	// Quorum members sign DIFFERENT messages, then we try to verify
	// against ONE of those messages. Aggregate verify of same-message
	// flow must reject this.
	const N = 3
	var pkBlob strings.Builder
	msg1 := []byte("message one")
	msg2 := []byte("message two")
	sigs := make([]*bls.Signature, N)

	for i := 0; i < N; i++ {
		var pkHex string
		var sk *bls.SecretKey
		sk, pkHex, _ = genBlsKey(t)
		pkBlob.WriteString(pkHex)
		// First two sign msg1, third signs msg2 — broken quorum.
		if i < 2 {
			sigs[i] = bls.Sign(sk, msg1)
		} else {
			sigs[i] = bls.Sign(sk, msg2)
		}
	}

	aggSig, err := bls.Aggregate(sigs)
	require.NoError(t, err)
	aggSigRaw := aggSig.Serialize()

	res := callBlsVerifyAggregate(t, pkBlob.String(), hex.EncodeToString(msg1), hex.EncodeToString(aggSigRaw[:]))
	require.False(t, res.IsErr())
	assert.Equal(t, "false", res.Unwrap().Result, "different-message quorum must not verify as same-message")
}

func TestBlsVerifyAggregate_EmptyPubkeys(t *testing.T) {
	res := callBlsVerifyAggregate(t, "", "deadbeef", strings.Repeat("00", 96))
	assert.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "empty")
}

func TestBlsVerifyAggregate_BadPubkeyChunking(t *testing.T) {
	// 50 bytes — not a multiple of 48 — must reject.
	res := callBlsVerifyAggregate(t, strings.Repeat("00", 50), "deadbeef", strings.Repeat("00", 96))
	assert.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "multiple of 48")
}

func TestBlsVerifyAggregate_MaxBound(t *testing.T) {
	// 257 pubkeys exceeds the 256 cap.
	res := callBlsVerifyAggregate(t, strings.Repeat("00", 48*257), "deadbeef", strings.Repeat("00", 96))
	assert.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "exceeds max")
}

func TestBlsVerifyAggregate_GasScalesWithPubkeyCount(t *testing.T) {
	// Verify that aggregate-verify gas grows with N (the per-pubkey term
	// is non-zero). Both verifications below succeed; we only compare gas.
	mkBlob := func(n int) (string, string, string) {
		var pkBlob strings.Builder
		msg := []byte("gas scaling test")
		sigs := make([]*bls.Signature, n)
		for i := 0; i < n; i++ {
			sk, pkHex, _ := genBlsKey(t)
			pkBlob.WriteString(pkHex)
			sigs[i] = bls.Sign(sk, msg)
		}
		agg, err := bls.Aggregate(sigs)
		require.NoError(t, err)
		aggRaw := agg.Serialize()
		return pkBlob.String(), hex.EncodeToString(msg), hex.EncodeToString(aggRaw[:])
	}
	pk1, msg1, sig1 := mkBlob(1)
	pk10, msg10, sig10 := mkBlob(10)

	res1 := callBlsVerifyAggregate(t, pk1, msg1, sig1)
	res10 := callBlsVerifyAggregate(t, pk10, msg10, sig10)
	require.False(t, res1.IsErr())
	require.False(t, res10.IsErr())
	gas1 := res1.Unwrap().Gas
	gas10 := res10.Unwrap().Gas
	assert.Greater(t, gas10, gas1, "10-pubkey verify must charge more gas than 1-pubkey verify")
}
