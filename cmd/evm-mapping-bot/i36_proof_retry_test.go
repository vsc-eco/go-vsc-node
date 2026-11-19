package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// I-36 (testnet 11-03..05): one eth_getProof that no endpoint answered (a 429,
// a dropped connection) cost a B6 custody check for 10 min: 23 of 156 rounds
// skipped whole, 13 more their USDC part. The proof fetches retry a call no
// endpoint answered; an endpoint's
// answer (it cannot serve the proof) stays final.
func TestI36_AProofFetchRetriesANoAnswer(t *testing.T) {
	var calls atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(proofOK))
	}))
	defer flaky.Close()
	if _, _, _, err := fetchAccountProof(context.Background(), newEthRPC(flaky.URL), "0x0000000000000000000000000000000000000001", 1000); err != nil {
		t.Fatalf("account proof after one 429: %v, want the proof on the retry", err)
	}
	calls.Store(0)
	if _, _, err := fetchStorageProofs(context.Background(), newEthRPC(flaky.URL), "0x0000000000000000000000000000000000000001", []string{"0x00"}, 1000); err != nil {
		t.Fatalf("storage proof after one 429: %v, want the proof on the retry", err)
	}
	var limitedCalls atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limitedCalls.Add(1)
		_, _ = w.Write([]byte(proofWindowErr))
	}))
	defer limited.Close()
	if _, _, _, err := fetchAccountProof(context.Background(), newEthRPC(limited.URL), "0x0000000000000000000000000000000000000001", 1000); err == nil || !isProofCapabilityErr(err) {
		t.Fatalf("an endpoint that cannot serve the proof: %v, want its answer", err)
	}
	if n := limitedCalls.Load(); n != 1 {
		t.Fatalf("the cannot-serve answer was asked %d times, want 1 (an answer is final)", n)
	}
}
