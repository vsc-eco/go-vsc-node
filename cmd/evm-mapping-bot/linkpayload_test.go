package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"vsc-node/modules/common/params"
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
	if six := len(wrap(hs[:6])); six <= params.MAX_CONTRACT_PAYLOAD_SIZE {
		t.Logf("note: six real Sepolia headers are %d bytes (cap %d)", six, params.MAX_CONTRACT_PAYLOAD_SIZE)
	} else {
		t.Logf("six real Sepolia headers: %d bytes, over the %d cap (the C3 failure)", six, params.MAX_CONTRACT_PAYLOAD_SIZE)
	}
	covered, calls := 0, 0
	for end := len(hs); end > 0; {
		n := packHeadersFromEnd(hs[:end], linkPerCall, wrap)
		if n == 0 {
			t.Fatal("a single real header did not fit one call")
		}
		if got := len(wrap(hs[end-n : end])); got > params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("batch of %d is %d bytes, over the cap", n, got)
		}
		if n < linkPerCall && end-n > 0 && len(wrap(hs[end-n-1:end])) <= params.MAX_CONTRACT_PAYLOAD_SIZE {
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
		if n == 0 || len(awrap(items[i:i+n])) > params.MAX_CONTRACT_PAYLOAD_SIZE {
			t.Fatalf("attest batch %d: n=%d bytes=%d", i, n, len(awrap(items[i:i+max(n, 1)])))
		}
		i += n
	}
}
