package state_engine_test

// Post-fix companion to audit finding S8.
//
// Pre-fix the upsert filter was {key_id, tx_id} — retries with a new tx_id
// inserted a second row, breaking GetCommitmentByHeight's "highest row wins"
// assumption and letting any witness rewind keyInfo.Epoch by replaying an
// older commitment.
//
// Post-fix the filter is {key_id, block_height, type}. Retries collapse to
// one row keyed on the commitment's semantic identity, and the additional
// state_engine.go gate refuses to advance an active key's epoch backwards.
//
// Without a live mongo we still assert at the source level: confirm the
// filter is now keyed on block_height and that "tx_id" no longer appears in
// the filter object.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditFix_S8_UpsertFilterUsesBlockHeightNotTxId reads
// the source file and confirms the upsert filter is now keyed on
// block_height + type, so retries with new tx_ids collapse to one row.
func TestAuditFix_S8_UpsertFilterUsesBlockHeightNotTxId(t *testing.T) {
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

	if !strings.Contains(filter, "\"block_height\"") {
		t.Fatalf("expected upsert filter to contain \"block_height\" post-fix; got: %q", filter)
	}
	if strings.Contains(filter, "\"tx_id\"") {
		t.Fatalf("upsert filter still contains \"tx_id\" — S8 retry-double-write defect is back: %q", filter)
	}
	if !strings.Contains(filter, "\"type\"") {
		t.Fatalf("expected upsert filter to also include \"type\" so keygen/reshare/blame/sign rows do not collide at the same (key_id, block_height); got: %q", filter)
	}
	t.Logf("S8 fix confirmed: SetCommitmentData filter = bson.M{%s}", strings.TrimSpace(filter))
}
