package mapper

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"

	transactionpool "vsc-node/modules/transaction-pool"

	"encoding/hex"

	"github.com/btcsuite/btcd/wire"
)

func base64Decode(wire []byte) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(string(wire))
}

func buildRawTxHex(t *testing.T) (rawTxHex string, txID string) {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	tx.AddTxOut(&wire.TxOut{Value: 1000, PkScript: []byte{0x51}})
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		t.Fatalf("serialize tx: %v", err)
	}
	return hex.EncodeToString(buf.Bytes()), tx.TxID()
}

// --- mustPaginate / size boundary ---------------------------------------------------

func TestMustPaginate_BoundaryAtMaxPagePayloadBytes(t *testing.T) {
	if mustPaginate(maxPagePayloadBytes) {
		t.Fatalf("payload of exactly maxPagePayloadBytes should not require pagination")
	}
	if !mustPaginate(maxPagePayloadBytes + 1) {
		t.Fatalf("payload one byte over maxPagePayloadBytes should require pagination")
	}
	if mustPaginate(0) {
		t.Fatalf("empty payload must not require pagination")
	}
}

// --- splitIntoPages: deterministic, byte-exact -------------------------------------

func TestSplitIntoPages_ReassemblesExact(t *testing.T) {
	payload := make([]byte, 3*maxPagePayloadBytes+17)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	pages, err := splitIntoPages(payload)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	// Pages are base64-wire chunks; concat must round-trip through base64
	// decode to the original raw payload.
	var joined bytes.Buffer
	for i, p := range pages {
		if i < len(pages)-1 && len(p) != maxPagePayloadBytes {
			t.Fatalf("non-final page %d has size %d (want %d)", i, len(p), maxPagePayloadBytes)
		}
		if len(p) > maxPagePayloadBytes {
			t.Fatalf("page %d oversized: %d bytes", i, len(p))
		}
		joined.Write(p)
	}
	wire := encodeForWire(payload)
	if !bytes.Equal(joined.Bytes(), wire) {
		t.Fatalf("concat(pages) != base64(original)")
	}
	decoded, err := base64Decode(joined.Bytes())
	if err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decode(concat(pages)) != original")
	}
}

func TestSplitIntoPages_EmptyYieldsSinglePage(t *testing.T) {
	pages, err := splitIntoPages(nil)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(pages) != 1 || len(pages[0]) != 0 {
		t.Fatalf("empty payload must yield one empty page, got %d pages (first len=%d)", len(pages), len(pages[0]))
	}
}

func TestSplitIntoPages_RejectsTooManyPages(t *testing.T) {
	// A raw payload large enough to require 129 pages must be rejected.
	rawPerPage := maxPagePayloadBytes * 3 / 4
	payload := make([]byte, 129*rawPerPage)
	if _, err := splitIntoPages(payload); err == nil {
		t.Fatalf("expected error for payload exceeding MaxPagesPerPlan")
	}
}

// --- buildPagePlan / ordering -------------------------------------------------------

func TestBuildPagePlan_OrderedAndBound(t *testing.T) {
	payload := make([]byte, maxPagePayloadBytes*2+42)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	env := paginationEnvelopeData{
		TxID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vout:        7,
		BlockHeight: 1234,
	}
	plan, err := buildPagePlan(payload, env)
	if err != nil {
		t.Fatalf("buildPagePlan: %v", err)
	}
	if len(plan) < 2 {
		t.Fatalf("expected >= 2 pages for a 2x+ payload, got %d", len(plan))
	}
	for i, job := range plan {
		if job.PageIdx != uint32(i) {
			t.Fatalf("plan[%d].PageIdx = %d", i, job.PageIdx)
		}
		if job.TotalPages != uint32(len(plan)) {
			t.Fatalf("plan[%d].TotalPages = %d", i, job.TotalPages)
		}
		if job.TxID != env.TxID || job.Vout != env.Vout || job.BlockHeight != env.BlockHeight {
			t.Fatalf("plan[%d] envelope mismatch", i)
		}
	}

	// Full-plan round trip: concat of page payloads is the base64 wire form;
	// decoding yields the original payload and hashes to parentID.
	var joined bytes.Buffer
	for _, job := range plan {
		joined.WriteString(job.Payload)
	}
	decoded, err := base64Decode(joined.Bytes())
	if err != nil {
		t.Fatalf("concat decode: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("reassembled payload != original")
	}
}

// --- assertPagePlanFitsL2 (client-side witness for PagePlanFitsL2) -----------------

func TestAssertPagePlanFitsL2_Holds(t *testing.T) {
	payload := make([]byte, maxPagePayloadBytes*3+1)
	plan, err := buildPagePlan(payload, paginationEnvelopeData{
		TxID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Vout:        1,
		BlockHeight: 77,
	})
	if err != nil {
		t.Fatalf("buildPagePlan: %v", err)
	}
	if err := assertPagePlanFitsL2(plan); err != nil {
		t.Fatalf("plan should fit L2 cap: %v", err)
	}
	for _, job := range plan {
		body, err := encodePageSubmission(job)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if len(body) > transactionpool.MAX_TX_SIZE {
			t.Fatalf("encoded page body %d > L2 cap %d", len(body), transactionpool.MAX_TX_SIZE)
		}
	}
}

// --- encodePageSubmission: wire-format compatibility --------------------------------

func TestEncodePageSubmission_MatchesContractSchema(t *testing.T) {
	payload := []byte(`{"action":"map","x":1}`)
	env := paginationEnvelopeData{
		TxID:        "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Vout:        2,
		BlockHeight: 90,
	}
	plan, err := buildPagePlan(payload, env)
	if err != nil {
		t.Fatalf("buildPagePlan: %v", err)
	}
	if len(plan) != 1 {
		t.Fatalf("expected a single-page plan, got %d", len(plan))
	}
	body, err := encodePageSubmission(plan[0])
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded struct {
		TxID        string `json:"tx_id"`
		Vout        uint32 `json:"vout"`
		BlockHeight uint32 `json:"block_height"`
		PageIdx     uint32 `json:"page_idx"`
		TotalPages  uint32 `json:"total_pages"`
		Payload     string `json:"payload"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.TxID != env.TxID || decoded.Vout != env.Vout || decoded.BlockHeight != env.BlockHeight {
		t.Fatalf("envelope mismatch: %+v", decoded)
	}
	if decoded.PageIdx != 0 || decoded.TotalPages != 1 {
		t.Fatalf("unexpected indices: %+v", decoded)
	}
	// decoded.Payload is the base64 wire form; decoding it must reproduce
	// the original raw payload bytes.
	roundTrip, err := base64Decode([]byte(decoded.Payload))
	if err != nil {
		t.Fatalf("wire decode: %v", err)
	}
	if !bytes.Equal(roundTrip, payload) {
		t.Fatalf("payload round-trip failure: %q vs %q", roundTrip, payload)
	}
}

// --- pageActionFor / supported actions ---------------------------------------------

func TestPageActionFor(t *testing.T) {
	cases := map[string]string{
		"map":          "mapPage",
		"confirmSpend": "confirmSpendPage",
		"":             "",
	}
	for in, want := range cases {
		if got := pageActionFor(in); got != want {
			t.Fatalf("pageActionFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequirePaginatedAction(t *testing.T) {
	if err := requirePaginatedAction("map"); err != nil {
		t.Fatalf("map should be paginatable: %v", err)
	}
	if err := requirePaginatedAction("confirmSpend"); err != nil {
		t.Fatalf("confirmSpend should be paginatable: %v", err)
	}
	if err := requirePaginatedAction("unmap"); err == nil {
		t.Fatalf("unmap should not be paginatable")
	}
}

func TestParseEnvelopeData_Map(t *testing.T) {
	rawTxHex, txID := buildRawTxHex(t)
	body := []byte(`{"tx_data":{"block_height":42,"raw_tx_hex":"` + rawTxHex + `","merkle_proof_hex":"","tx_index":0},"instructions":["x=y"]}`)
	env, err := parseEnvelopeData("map", body)
	if err != nil {
		t.Fatalf("parse map envelope: %v", err)
	}
	if env.TxID != txID || env.BlockHeight != 42 || env.Vout != 0 {
		t.Fatalf("unexpected env: %+v", env)
	}
}

func TestParseEnvelopeData_ConfirmSpend(t *testing.T) {
	rawTxHex, txID := buildRawTxHex(t)
	body := []byte(`{"tx_data":{"block_height":55,"raw_tx_hex":"` + rawTxHex + `","merkle_proof_hex":"","tx_index":0},"indices":[9,10]}`)
	env, err := parseEnvelopeData("confirmSpend", body)
	if err != nil {
		t.Fatalf("parse confirmSpend envelope: %v", err)
	}
	if env.TxID != txID || env.BlockHeight != 55 || env.Vout != 9 {
		t.Fatalf("unexpected env: %+v", env)
	}
}

func TestParseEnvelopeData_RejectsBadPayload(t *testing.T) {
	if _, err := parseEnvelopeData("map", []byte(`{"tx_data":{}}`)); err == nil {
		t.Fatalf("expected error for missing raw tx")
	}
	if _, err := parseEnvelopeData("confirmSpend", []byte(`{"tx_data":{"block_height":1,"raw_tx_hex":"zz"}}`)); err == nil {
		t.Fatalf("expected error for invalid raw tx hex")
	}
}
