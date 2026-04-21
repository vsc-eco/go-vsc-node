package test_utils

import (
	"fmt"
	"testing"

	tss "vsc-node/modules/db/vsc/tss"
)

func TestFindUnsignedRequests_OrdersByLastAttempt(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 300, LastAttempt: 300},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 100, LastAttempt: 100},
			"c": {Id: "c", KeyId: "k1", Msg: "cc", Status: tss.SignPending, CreatedHeight: 200, LastAttempt: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].LastAttempt != 100 || results[1].LastAttempt != 200 || results[2].LastAttempt != 300 {
		t.Errorf("expected last-attempt order [100,200,300], got [%d,%d,%d]",
			results[0].LastAttempt, results[1].LastAttempt, results[2].LastAttempt)
	}
}

func TestFindUnsignedRequests_GatesOnLastAttempt(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"in-flight": {Id: "in-flight", KeyId: "k1", Msg: "in-flight", Status: tss.SignPending, CreatedHeight: 50, LastAttempt: 200},
			"ready":     {Id: "ready", KeyId: "k1", Msg: "ready", Status: tss.SignPending, CreatedHeight: 50, LastAttempt: 50},
		},
	}

	// bh < in-flight's LastAttempt: only "ready" returned.
	results, err := mock.FindUnsignedRequests(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Msg != "ready" {
		t.Fatalf("want [ready], got %+v", results)
	}

	// bh past the reservation: both eligible.
	results, err = mock.FindUnsignedRequests(300, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
}

func TestFindUnsignedRequests_SurfacesCapped(t *testing.T) {
	// At-cap rows remain in the query result so the caller can mark them
	// failed at selection time. Enforcement is lazy, not filter-based.
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"capped": {
				Id: "capped", KeyId: "k1", Msg: "capped",
				Status:        tss.SignPending,
				CreatedHeight: 100,
				LastAttempt:   100,
				AttemptCount:  tss.MaxSignAttempts,
			},
		},
	}

	results, err := mock.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Msg != "capped" {
		t.Fatalf("want [capped] returned for caller to mark failed, got %+v", results)
	}
	if results[0].AttemptCount != tss.MaxSignAttempts {
		t.Errorf("expected AttemptCount=%d, got %d", tss.MaxSignAttempts, results[0].AttemptCount)
	}
}

func TestFindUnsignedRequests_RespectsLimit(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{},
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("m-%02d", i)
		mock.Requests[id] = tss.TssRequest{
			Id: id, KeyId: "k1", Msg: id,
			Status: tss.SignPending, CreatedHeight: uint64(100 + i), LastAttempt: uint64(100 + i),
		}
	}

	results, err := mock.FindUnsignedRequests(500, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 6 {
		t.Fatalf("want 6, got %d", len(results))
	}
	// Lowest LastAttempt (== CreatedHeight) first → m-00..m-05.
	for i, r := range results {
		want := fmt.Sprintf("m-%02d", i)
		if r.Msg != want {
			t.Errorf("position %d: want %q, got %q", i, want, r.Msg)
		}
	}
}

func TestFindUnsignedRequests_Limit1ForExistenceCheck(t *testing.T) {
	// tss.go:437 uses Limit=1 as an existence check. Preserve this.
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100, LastAttempt: 100},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 200, LastAttempt: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(300, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for existence check, got %d", len(results))
	}
	if results[0].LastAttempt != 100 {
		t.Errorf("expected earliest (100), got %d", results[0].LastAttempt)
	}
}

func TestFindUnsignedRequests_SkipsSignedAndFailed(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a":      {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, LastAttempt: 100, CreatedHeight: 100},
			"signed": {Id: "signed", KeyId: "k1", Msg: "ss", Sig: "somesig", Status: tss.SignComplete, LastAttempt: 50, CreatedHeight: 50},
			"failed": {Id: "failed", KeyId: "k1", Msg: "ff", Status: tss.SignFailed, LastAttempt: 60, CreatedHeight: 60},
			"b":      {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, LastAttempt: 200, CreatedHeight: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(300, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 unsigned pending results, got %d", len(results))
	}
}

func TestFindUnsignedRequests_LimitLargerThanResults(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, LastAttempt: 100, CreatedHeight: 100},
		},
	}

	results, err := mock.FindUnsignedRequests(300, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (fewer than limit), got %d", len(results))
	}
}

func TestSetSignedRequest_ResetsFailed(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"x": {Id: "x", KeyId: "k1", Msg: "m", Status: tss.SignFailed, Sig: "stale", CreatedHeight: 100, AttemptCount: 5, LastAttempt: 1200},
		},
	}

	if err := mock.SetSignedRequest(tss.TssRequest{KeyId: "k1", Msg: "m", CreatedHeight: 900}); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["x"]
	if got.Status != tss.SignPending {
		t.Errorf("expected SignPending, got %s", got.Status)
	}
	if got.AttemptCount != 0 {
		t.Errorf("expected AttemptCount reset to 0, got %d", got.AttemptCount)
	}
	if got.LastAttempt != 900 {
		t.Errorf("expected LastAttempt=900, got %d", got.LastAttempt)
	}
	if got.CreatedHeight != 900 {
		t.Errorf("expected CreatedHeight=900, got %d", got.CreatedHeight)
	}
	if got.Sig != "" {
		t.Errorf("expected Sig cleared, got %q", got.Sig)
	}
}

func TestSetSignedRequest_NoOpOnUnsigned(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"x": {Id: "x", KeyId: "k1", Msg: "m", Status: tss.SignPending, CreatedHeight: 100, AttemptCount: 2, LastAttempt: 400},
		},
	}

	if err := mock.SetSignedRequest(tss.TssRequest{KeyId: "k1", Msg: "m", CreatedHeight: 900}); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["x"]
	if got.AttemptCount != 2 || got.LastAttempt != 400 || got.CreatedHeight != 100 {
		t.Errorf("expected unchanged unsigned, got %+v", got)
	}
}

func TestReserveAttempt_IdempotentWithinReservation(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"x": {Id: "x", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100, LastAttempt: 100},
		},
	}

	// First reservation at bh=500.
	if err := mock.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}
	// Re-issuing the same block's reservation must not double-count.
	if err := mock.ReserveAttempt("k1", "aa", 500); err != nil {
		t.Fatal(err)
	}
	// Even at bh within the reservation window: no-op.
	if err := mock.ReserveAttempt("k1", "aa", 500+tss.SignAttemptReservation-1); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["x"]
	if got.LastAttempt != 500+tss.SignAttemptReservation {
		t.Errorf("expected LastAttempt=%d, got %d", 500+tss.SignAttemptReservation, got.LastAttempt)
	}
	if got.AttemptCount != 1 {
		t.Errorf("expected AttemptCount=1, got %d", got.AttemptCount)
	}
}

func TestReserveAttempt_NoOpOnNonPending(t *testing.T) {
	// Defensive guard: if a row has transitioned to failed or complete,
	// ReserveAttempt must not touch it.
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"failed":   {Id: "failed", KeyId: "k1", Msg: "failed", Status: tss.SignFailed, CreatedHeight: 100, LastAttempt: 100, AttemptCount: 3},
			"complete": {Id: "complete", KeyId: "k1", Msg: "complete", Status: tss.SignComplete, Sig: "sig", CreatedHeight: 100, LastAttempt: 100, AttemptCount: 1},
		},
	}

	if err := mock.ReserveAttempt("k1", "failed", 500); err != nil {
		t.Fatal(err)
	}
	if err := mock.ReserveAttempt("k1", "complete", 500); err != nil {
		t.Fatal(err)
	}

	failed := mock.Requests["failed"]
	if failed.LastAttempt != 100 {
		t.Errorf("failed row LastAttempt should stay 100, got %d", failed.LastAttempt)
	}
	if failed.AttemptCount != 3 {
		t.Errorf("failed row AttemptCount should stay 3, got %d", failed.AttemptCount)
	}

	complete := mock.Requests["complete"]
	if complete.LastAttempt != 100 {
		t.Errorf("complete row LastAttempt should stay 100, got %d", complete.LastAttempt)
	}
	if complete.AttemptCount != 1 {
		t.Errorf("complete row AttemptCount should stay 1, got %d", complete.AttemptCount)
	}
}

func TestMarkFailed_TransitionsPendingRow(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"to-fail": {
				Id: "to-fail", KeyId: "k1", Msg: "to-fail",
				Status:        tss.SignPending,
				CreatedHeight: 100,
				LastAttempt:   100,
				AttemptCount:  tss.MaxSignAttempts,
			},
		},
	}

	if err := mock.MarkFailed("k1", "to-fail"); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["to-fail"]
	if got.Status != tss.SignFailed {
		t.Errorf("expected SignFailed, got %s", got.Status)
	}
}

func TestMarkFailed_NoOpOnComplete(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"done": {
				Id: "done", KeyId: "k1", Msg: "done",
				Status:        tss.SignComplete,
				Sig:           "sig",
				CreatedHeight: 100,
				LastAttempt:   100,
				AttemptCount:  2,
			},
		},
	}

	if err := mock.MarkFailed("k1", "done"); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["done"]
	if got.Status != tss.SignComplete {
		t.Errorf("expected SignComplete, got %s", got.Status)
	}
	if got.Sig != "sig" {
		t.Errorf("expected Sig preserved, got %q", got.Sig)
	}
}

func TestMarkFailedByKey_OnlyUnsignedForMatchingKey(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "target", Msg: "m1", Status: tss.SignPending},
			"b": {Id: "b", KeyId: "target", Msg: "m2", Status: tss.SignComplete, Sig: "sig1"},
			"c": {Id: "c", KeyId: "other", Msg: "m3", Status: tss.SignPending},
		},
	}

	if err := mock.MarkFailedByKey("target"); err != nil {
		t.Fatal(err)
	}

	if mock.Requests["a"].Status != tss.SignFailed {
		t.Errorf("expected a to be failed, got %s", mock.Requests["a"].Status)
	}
	if mock.Requests["b"].Status != tss.SignComplete {
		t.Errorf("expected b still complete, got %s", mock.Requests["b"].Status)
	}
	if mock.Requests["c"].Status != tss.SignPending {
		t.Errorf("expected c still pending, got %s", mock.Requests["c"].Status)
	}
}

func TestDeprecateLegacyKeys_ReturnsDeprecatedIds(t *testing.T) {
	mock := &MockTssKeysDb{
		Keys: map[string]tss.TssKey{
			"legacy1":  {Id: "legacy1", Status: tss.TssKeyActive, ExpiryEpoch: 0},
			"legacy2":  {Id: "legacy2", Status: tss.TssKeyActive, ExpiryEpoch: 0},
			"withexp":  {Id: "withexp", Status: tss.TssKeyActive, ExpiryEpoch: 100},
			"inactive": {Id: "inactive", Status: tss.TssKeyDeprecated, ExpiryEpoch: 0},
		},
	}

	ids, err := mock.DeprecateLegacyKeys()
	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 deprecated ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if id != "legacy1" && id != "legacy2" {
			t.Errorf("unexpected id %q", id)
		}
	}
	if mock.Keys["legacy1"].Status != tss.TssKeyDeprecated {
		t.Error("legacy1 not deprecated")
	}
	if mock.Keys["withexp"].Status != tss.TssKeyActive {
		t.Error("withexp should still be active")
	}
}
