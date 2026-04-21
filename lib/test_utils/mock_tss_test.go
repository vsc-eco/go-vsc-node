package test_utils

import (
	"testing"

	tss "vsc-node/modules/db/vsc/tss"
)

func TestFindUnsignedRequests_OrdersByNextAttemptHeight(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 300, NextAttemptHeight: 300},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 100, NextAttemptHeight: 100},
			"c": {Id: "c", KeyId: "k1", Msg: "cc", Status: tss.SignPending, CreatedHeight: 200, NextAttemptHeight: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].NextAttemptHeight != 100 || results[1].NextAttemptHeight != 200 || results[2].NextAttemptHeight != 300 {
		t.Errorf("expected next-attempt order [100,200,300], got [%d,%d,%d]",
			results[0].NextAttemptHeight, results[1].NextAttemptHeight, results[2].NextAttemptHeight)
	}
}

func TestFindUnsignedRequests_BackedOffMovesToEnd(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			// Old creation but backed off - should be LAST despite older CreatedHeight.
			"stale": {Id: "stale", KeyId: "k1", Msg: "stale", Status: tss.SignPending, CreatedHeight: 50, AttemptCount: 4, NextAttemptHeight: 450},
			"fresh": {Id: "fresh", KeyId: "k1", Msg: "fresh", Status: tss.SignPending, CreatedHeight: 400, NextAttemptHeight: 400},
		},
	}

	results, err := mock.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Msg != "fresh" || results[1].Msg != "stale" {
		t.Errorf("expected fresh before stale, got %s,%s", results[0].Msg, results[1].Msg)
	}
}

func TestFindUnsignedRequests_LimitTruncates(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, NextAttemptHeight: 300, CreatedHeight: 300},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, NextAttemptHeight: 100, CreatedHeight: 100},
			"c": {Id: "c", KeyId: "k1", Msg: "cc", Status: tss.SignPending, NextAttemptHeight: 200, CreatedHeight: 200},
			"d": {Id: "d", KeyId: "k1", Msg: "dd", Status: tss.SignPending, NextAttemptHeight: 400, CreatedHeight: 400},
			"e": {Id: "e", KeyId: "k1", Msg: "ee", Status: tss.SignPending, NextAttemptHeight: 500, CreatedHeight: 500},
		},
	}

	results, err := mock.FindUnsignedRequests(600, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].NextAttemptHeight != 100 {
		t.Errorf("expected earliest (100), got %d", results[0].NextAttemptHeight)
	}
	if results[2].NextAttemptHeight != 300 {
		t.Errorf("expected third earliest (300), got %d", results[2].NextAttemptHeight)
	}
}

func TestFindUnsignedRequests_Limit1ForExistenceCheck(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100, NextAttemptHeight: 100},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 200, NextAttemptHeight: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(300, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for existence check, got %d", len(results))
	}
	if results[0].NextAttemptHeight != 100 {
		t.Errorf("expected earliest (100), got %d", results[0].NextAttemptHeight)
	}
}

func TestFindUnsignedRequests_SkipsSignedAndFailed(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a":      {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, NextAttemptHeight: 100, CreatedHeight: 100},
			"signed": {Id: "signed", KeyId: "k1", Msg: "ss", Sig: "somesig", Status: tss.SignComplete, NextAttemptHeight: 50, CreatedHeight: 50},
			"failed": {Id: "failed", KeyId: "k1", Msg: "ff", Status: tss.SignFailed, NextAttemptHeight: 60, CreatedHeight: 60},
			"b":      {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, NextAttemptHeight: 200, CreatedHeight: 200},
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

func TestFindUnsignedRequests_GatesOnNextAttemptHeight(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"due":    {Id: "due", KeyId: "k1", Msg: "due", Status: tss.SignPending, NextAttemptHeight: 100, CreatedHeight: 50},
			"future": {Id: "future", KeyId: "k1", Msg: "future", Status: tss.SignPending, NextAttemptHeight: 500, CreatedHeight: 50},
		},
	}

	results, err := mock.FindUnsignedRequests(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (gated), got %d: %+v", len(results), results)
	}
	if results[0].Msg != "due" {
		t.Errorf("expected 'due', got %q", results[0].Msg)
	}
}

func TestFindUnsignedRequests_LimitLargerThanResults(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, NextAttemptHeight: 100, CreatedHeight: 100},
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
			"x": {Id: "x", KeyId: "k1", Msg: "m", Status: tss.SignFailed, Sig: "stale", CreatedHeight: 100, AttemptCount: 5, NextAttemptHeight: 1200},
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
	if got.NextAttemptHeight != 900 {
		t.Errorf("expected NextAttemptHeight=900, got %d", got.NextAttemptHeight)
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
			"x": {Id: "x", KeyId: "k1", Msg: "m", Status: tss.SignPending, CreatedHeight: 100, AttemptCount: 2, NextAttemptHeight: 400},
		},
	}

	if err := mock.SetSignedRequest(tss.TssRequest{KeyId: "k1", Msg: "m", CreatedHeight: 900}); err != nil {
		t.Fatal(err)
	}

	got := mock.Requests["x"]
	if got.AttemptCount != 2 || got.NextAttemptHeight != 400 || got.CreatedHeight != 100 {
		t.Errorf("expected unchanged unsigned, got %+v", got)
	}
}

func TestBumpAttempt_SetsAttemptAndNext(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"x": {Id: "x", KeyId: "k1", Msg: "m", Status: tss.SignPending, CreatedHeight: 100, NextAttemptHeight: 100},
			"y": {Id: "y", KeyId: "k2", Msg: "m", Status: tss.SignPending, CreatedHeight: 100, NextAttemptHeight: 100},
		},
	}

	if err := mock.BumpAttempt("k1", "m", 3, 750); err != nil {
		t.Fatal(err)
	}

	if mock.Requests["x"].AttemptCount != 3 || mock.Requests["x"].NextAttemptHeight != 750 {
		t.Errorf("expected bumped x, got %+v", mock.Requests["x"])
	}
	if mock.Requests["y"].AttemptCount != 0 || mock.Requests["y"].NextAttemptHeight != 100 {
		t.Errorf("expected untouched y, got %+v", mock.Requests["y"])
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
