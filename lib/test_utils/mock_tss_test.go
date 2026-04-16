package test_utils

import (
	"testing"

	tss "vsc-node/modules/db/vsc/tss"
)

func TestFindUnsignedRequests_FIFOOrder(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 300},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 100},
			"c": {Id: "c", KeyId: "k1", Msg: "cc", Status: tss.SignPending, CreatedHeight: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results[0].CreatedHeight != 100 || results[1].CreatedHeight != 200 || results[2].CreatedHeight != 300 {
		t.Errorf("expected FIFO order [100,200,300], got [%d,%d,%d]",
			results[0].CreatedHeight, results[1].CreatedHeight, results[2].CreatedHeight)
	}
}

func TestFindUnsignedRequests_LimitTruncates(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 300},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 100},
			"c": {Id: "c", KeyId: "k1", Msg: "cc", Status: tss.SignPending, CreatedHeight: 200},
			"d": {Id: "d", KeyId: "k1", Msg: "dd", Status: tss.SignPending, CreatedHeight: 400},
			"e": {Id: "e", KeyId: "k1", Msg: "ee", Status: tss.SignPending, CreatedHeight: 500},
		},
	}

	results, err := mock.FindUnsignedRequests(600, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Should be the 3 oldest
	if results[0].CreatedHeight != 100 {
		t.Errorf("expected oldest first (100), got %d", results[0].CreatedHeight)
	}
	if results[2].CreatedHeight != 300 {
		t.Errorf("expected third oldest (300), got %d", results[2].CreatedHeight)
	}
}

func TestFindUnsignedRequests_Limit1ForExistenceCheck(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100},
			"b": {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 200},
		},
	}

	results, err := mock.FindUnsignedRequests(300, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for existence check, got %d", len(results))
	}
	if results[0].CreatedHeight != 100 {
		t.Errorf("expected oldest (100), got %d", results[0].CreatedHeight)
	}
}

func TestFindUnsignedRequests_SkipsSignedAndFailed(t *testing.T) {
	mock := &MockTssRequestsDb{
		Requests: map[string]tss.TssRequest{
			"a":       {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100},
			"signed":  {Id: "signed", KeyId: "k1", Msg: "ss", Sig: "somesig", Status: tss.SignComplete, CreatedHeight: 50},
			"failed":  {Id: "failed", KeyId: "k1", Msg: "ff", Status: tss.SignFailed, CreatedHeight: 60},
			"b":       {Id: "b", KeyId: "k1", Msg: "bb", Status: tss.SignPending, CreatedHeight: 200},
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
			"a": {Id: "a", KeyId: "k1", Msg: "aa", Status: tss.SignPending, CreatedHeight: 100},
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
