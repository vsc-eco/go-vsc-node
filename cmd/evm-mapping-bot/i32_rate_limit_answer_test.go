package main

import (
	"context"
	"strings"
	"testing"
)

// I-32 (testnet 11-05 01:00): thirdweb, one of the bot's two Sepolia RPCs,
// answers a rate-limited call with HTTP 200 and a JSON-RPC error (code -32005,
// "Rate limit exceeded. Upgrade your plan ..."). The bot took that as the
// chain's answer: no failover (B14 fails over only on no answer), the start
// endpoint moved onto the throttled one after a failover landed there, the
// attestation prefetch cached it as "this RPC cannot serve proofs" (the
// submitHeaders link-down, I-18's cost) and eth_getBlockReceipts as "method
// not served" (the per-tx path, I-16's cost). A throttle answer is now no
// answer, like an HTTP 429.
const thirdwebRateLimited = `{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"Rate limit exceeded. Upgrade your plan at https://thirdweb.com/pricing"}}`

func TestI32_RateLimitAnswerIsNoAnswer(t *testing.T) {
	ctx := context.Background()
	throttled, good := proofServer(t, 200, thirdwebRateLimited), proofServer(t, 200, proofOK)
	call := func(rpc *ethRPC) (string, error) {
		res, err := rpc.callCtx(ctx, "eth_getProof", `"0x00", [], "0x3e8"`)
		return string(res), err
	}
	// Throttled first: the call goes to the next endpoint.
	rpc := newEthRPC(throttled + "," + good)
	if res, err := call(rpc); err != nil || !strings.Contains(res, "accountProof") {
		t.Fatalf("throttled + good: %s %v, want the proof from the second endpoint", res, err)
	}
	// The good endpoint fails once (502) and the failover lands on the
	// throttled one: no answer, and calls keep starting at the good endpoint.
	rpc = newEthRPC(proofServer(t, 502, "bad gateway") + "," + throttled)
	if _, err := call(rpc); err == nil || isRPCAnswer(err) {
		t.Fatalf("502 + throttled: err %v, want a no-answer error (retryable)", err)
	}
	if rpc.start.Load() != 0 {
		t.Fatalf("calls start at endpoint %d, want 0: a throttled endpoint is not the one that answered", rpc.start.Load())
	}
	// Alchemy-style: the throttle as JSON-RPC code 429 in a 200.
	alch := proofServer(t, 200, `{"jsonrpc":"2.0","id":1,"error":{"code":429,"message":"Your app has exceeded its compute units per second capacity"}}`)
	if _, err := call(newEthRPC(alch)); err == nil || isRPCAnswer(err) {
		t.Fatalf("JSON code 429: err %v, want a no-answer error", err)
	}
	// Another code, the throttle named in the message.
	msg := proofServer(t, 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Too Many Requests: rate limit reached for this key"}}`)
	if _, err := call(newEthRPC(msg)); err == nil || isRPCAnswer(err) {
		t.Fatalf("throttle in the message: err %v, want a no-answer error", err)
	}
	// The attestation prefetch caches nothing (ask again next tick, no link-down).
	if e := prefetchHistoryProofs(ctx, newEthRPC(throttled), 1000, []uint64{700}); e != nil {
		t.Fatalf("throttled prefetch: cached %+v, want nothing cached", e)
	}
	// Other JSON-RPC errors stay the answer (B14, I-5).
	bad := proofServer(t, 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid argument 1: hex string has odd length"}}`)
	if _, err := call(newEthRPC(bad + "," + good)); err == nil || !isRPCAnswer(err) || !strings.Contains(err.Error(), "odd length") {
		t.Fatalf("a malformed request: err %v, want its error as the answer", err)
	}
}
