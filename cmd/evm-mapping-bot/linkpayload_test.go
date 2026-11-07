package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"vsc-node/modules/common/params"
	transactionpool "vsc-node/modules/transaction-pool"
)

// LINK-PAYLOAD-1: phase C3 (real Sepolia) showed six real headers make an
// 8,299-byte submitHeaders, over the node's 8,192-byte cap, so the link-down
// never stored a header and the deposits never mapped. Batches are now packed
// by payload size. Uses the real Glamsterdam Sepolia header from testdata.
func TestLINKPAYLOAD1_HeaderBatchesFitThePayloadCap(t *testing.T) {
	raw, err := os.ReadFile("testdata/headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]struct {
		RLP string `json:"rlp"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimPrefix(fx["sepolia"].RLP, "0x")
	if h == "" {
		t.Fatal("no sepolia header in testdata")
	}
	wrap := func(run []map[string]string) []byte {
		p, _ := json.Marshal(map[string]any{"headers": run})
		return p
	}
	hs := make([]map[string]string, 54) // C3: 54 headers between the deposit blocks and the tip
	for i := range hs {
		hs[i] = map[string]string{"rlp_hex": h}
	}
	t.Logf("six real Sepolia headers: %d bytes raw, %d as the node counts (cap %d)", len(wrap(hs[:6])), nodePayloadSize(wrap(hs[:6])), params.MAX_CONTRACT_PAYLOAD_SIZE)
	covered, calls := 0, 0
	for end := len(hs); end > 0; {
		n := packHeadersFromEnd(hs[:end], linkPerCall, wrap)
		if n == 0 {
			t.Fatal("a single real header did not fit one call")
		}
		if got := nodePayloadSize(wrap(hs[end-n : end])); got > params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("batch of %d is %d bytes, over the cap", n, got)
		}
		if n < linkPerCall && end-n > 0 && nodePayloadSize(wrap(hs[end-n-1:end])) <= params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("batch of %d could have taken one more header", n)
		}
		covered += n
		calls++
		end -= n
	}
	if covered != len(hs) {
		t.Fatalf("covered %d of %d headers", covered, len(hs))
	}
	t.Logf("54 real-size headers in %d submitHeaders calls, each within the cap", calls)

	// attestHeaders: a header plus its storage proof (~2.2 KB base64 of real
	// EIP-2935 nodes) per item, at most attestPerCall per call.
	items := []map[string]string{
		{"rlp_hex": h, "storage_proof": strings.Repeat("A", 2400)},
		{"rlp_hex": h, "storage_proof": strings.Repeat("A", 2400)},
		{"rlp_hex": h, "storage_proof": strings.Repeat("A", 2400)},
	}
	awrap := func(run []map[string]string) []byte {
		p, _ := json.Marshal(map[string]any{"root_block": 12009834, "headers": run})
		return p
	}
	for i := 0; i < len(items); {
		n := packHeaders(items[i:], attestPerCall, awrap)
		if n == 0 || nodePayloadSize(awrap(items[i:i+n])) > params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("attest batch %d: n=%d bytes=%d", i, n, len(awrap(items[i:i+max(n, 1)])))
		}
		i += n
	}
}

// The bot's size check matches the node's: SerializeVSC (what the bot sends)
// decoded by transaction-pool.DecodeTxCbor into a json.RawMessage payload
// (what TxVscCallContract.ExecuteTx compares to MAX_CONTRACT_PAYLOAD_SIZE).
func TestLINKPAYLOAD1_NodePayloadSizeIsTheNodesCount(t *testing.T) {
	for _, p := range []string{
		`{"headers":[{"rlp_hex":"f90260a0"}]}`,
		`{"tx_data":{"block_height":1,"raw_hex":"02","receipt_proof":"AAEC+/=="},"instructions":[]}`,
		`{"a":"quote \" backslash \\ lt < amp &"}`,
	} {
		op, err := (&transactionpool.VscContractCall{ContractId: "vsc1x", Action: "a", Payload: p, RcLimit: 100, Caller: "hive:x", NetId: "n"}).SerializeVSC()
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := transactionpool.DecodeTxCbor(op, &got); err != nil {
			t.Fatal(err)
		}
		if nodePayloadSize([]byte(p)) != len(got.Payload) {
			t.Fatalf("nodePayloadSize %d, the node counts %d (%s)", nodePayloadSize([]byte(p)), len(got.Payload), got.Payload)
		}
	}
}

// Phase C4's case: six headers that fit the cap raw (8,187 B) but not as the
// node counts them (8,215 B). Old code sent all six and the node refused it.
func TestLINKPAYLOAD1_SixHeadersThatFitOnlyRaw(t *testing.T) {
	wrap := func(run []map[string]string) []byte {
		p, _ := json.Marshal(map[string]any{"headers": run})
		return p
	}
	six := func(l int) []map[string]string {
		hs := make([]map[string]string, 6)
		for i := range hs {
			hs[i] = map[string]string{"rlp_hex": strings.Repeat("a", l)}
		}
		return hs
	}
	l := 1
	for len(wrap(six(l+1))) <= params.MAX_CONTRACT_PAYLOAD_SIZE {
		l++
	}
	hs := six(l)
	if raw, node := len(wrap(hs)), nodePayloadSize(wrap(hs)); raw > params.MAX_CONTRACT_PAYLOAD_SIZE || node <= params.MAX_CONTRACT_PAYLOAD_SIZE {
		t.Fatalf("setup: raw %d node %d", raw, node)
	}
	if n := packHeadersFromEnd(hs, linkPerCall, wrap); n != 5 {
		t.Fatalf("packed %d headers whose node size is %d; want 5", n, nodePayloadSize(wrap(hs[6-n:])))
	}
}
