package state_engine_test

// Audit finding S8 (unfixed): tss_db.SetCommitmentData uses
// {key_id, tx_id} as the upsert filter (modules/db/vsc/tss/commitments.go:19-46).
// This means two *different* commitments for the same (key_id, block_height)
// but different tx_id will produce TWO rows, rather than being deduped to one.
// Consumers (GetCommitmentByHeight, blame aggregation) then read one of
// several rows non-deterministically, which can mis-attribute blame or
// double-count signatures.
//
// Reachability: any node that re-broadcasts its commitment in a fresh
// custom_json (e.g. retry after a Hive RPC error) will end up with a
// second row.
//
// Without a live mongo we cannot exercise the upsert directly; this
// test asserts the *static precondition* by reading the source and
// confirming the filter key is "tx_id" (not "block_height" or the
// proper logical-dedup key). When the bug is fixed (filter switched to
// {key_id, block_height, type} or similar) the assertion below will
// fail, signalling that the test needs to be updated.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditUnfixed_S8_UpsertFilterUsesTxIdInsteadOfBlockHeight reads
// the source file and confirms the upsert filter is keyed on tx_id,
// which makes the dedup useless across retries.
func TestAuditUnfixed_S8_UpsertFilterUsesTxIdInsteadOfBlockHeight(t *testing.T) {
	// Locate modules/db/vsc/tss/commitments.go relative to this test
	// file (state_engine package lives at modules/state-processing).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller file")
	}
	repoModulesDir := filepath.Dir(filepath.Dir(thisFile)) // .../modules
	srcPath := filepath.Join(repoModulesDir, "db", "vsc", "tss", "commitments.go")

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	src := string(raw)

	// Carve out the SetCommitmentData function body (up to the next
	// top-level `func`). This is a coarse but stable slice.
	const fnTag = "func (tsc *tssCommitments) SetCommitmentData("
	start := strings.Index(src, fnTag)
	if start < 0 {
		t.Fatalf("did not find %q in source — file structure changed", fnTag)
	}
	rest := src[start:]
	end := strings.Index(rest[1:], "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	body := rest[:end]

	// Find the filter document. FindOneAndUpdate's second arg is the
	// filter. We look for "FindOneAndUpdate" then assert that within
	// the next ~400 chars (the filter object) we see "tx_id" but NOT
	// "block_height".
	fIdx := strings.Index(body, "FindOneAndUpdate")
	if fIdx < 0 {
		t.Fatalf("FindOneAndUpdate not found in SetCommitmentData body")
	}
	// The filter spans from FindOneAndUpdate( to the matching `,` that
	// introduces the update doc. Grab a generous slice and check the
	// first bson.M{ ... } block within.
	tail := body[fIdx:]
	openBrace := strings.Index(tail, "bson.M{")
	if openBrace < 0 {
		t.Fatalf("bson.M{ filter not found")
	}
	filterStart := openBrace + len("bson.M{")
	closeBrace := strings.Index(tail[filterStart:], "}")
	if closeBrace < 0 {
		t.Fatalf("closing brace for filter bson.M{} not found")
	}
	filter := tail[filterStart : filterStart+closeBrace]

	if !strings.Contains(filter, "\"tx_id\"") {
		t.Fatalf("expected upsert filter to contain \"tx_id\" (current buggy behavior); got: %q", filter)
	}
	if strings.Contains(filter, "\"block_height\"") {
		// When fixed, block_height becomes part of the dedup key — at
		// that point this branch fires and we update the assertion.
		t.Fatalf("upsert filter now contains \"block_height\" — fix appears to have landed, update this test: %q", filter)
	}
	t.Logf("precondition confirmed: SetCommitmentData filter = bson.M{%s}", strings.TrimSpace(filter))
}
