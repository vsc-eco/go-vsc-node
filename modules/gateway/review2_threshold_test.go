package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"

	"github.com/vsc-eco/hivego"
)

// errAcctClient makes getThreshold's GetAccount fail (simulates Hive RPC down).
type errAcctClient struct{}

func (errAcctClient) GetAccount(_ []string) ([]hivego.AccountData, error) {
	return nil, errors.New("hive rpc unreachable")
}

// review2 HIGH #80 — getThreshold()'s error was discarded at 4 sites
// (`threshold, _, _, _ := ms.getThreshold()`); threshold then defaulted to 0,
// and in waitForSigs the collection loop `for threshold > signedWeight` exits
// immediately, returning 0 sigs which callers treat as "fully signed" →
// unsigned/under-signed broadcast.
//
// With an account client that errors:
//   - getThreshold must surface the error (not silently return 0).
//   - waitForSigs must ABORT with an error that identifies the
//     getThreshold failure — fix/review2 returns `getThreshold failed: ...`
//     (GREEN). On #170 the threshold error is swallowed and waitForSigs
//     instead falls through to the unrelated "no channel for txId" path
//     (never mentioning getThreshold) → this test FAILS (RED).
func TestReview2GetThresholdErrorNotSwallowed(t *testing.T) {
	ms := &MultiSig{
		sconf:         systemconfig.MocknetConfig(),
		accountClient: errAcctClient{},
		msgChan:       make(map[string]chan *p2pMessage),
	}

	if _, _, _, err := ms.getThreshold(); err == nil {
		t.Fatal("getThreshold returned nil error despite GetAccount failing")
	}

	_, _, err := ms.waitForSigs(context.Background(), hivego.HiveTransaction{}, "txid")
	if err == nil {
		t.Fatal("review2 #80: waitForSigs returned nil error — threshold failure was swallowed (would broadcast under-signed)")
	}
	if !strings.Contains(err.Error(), "getThreshold") {
		t.Fatalf("review2 #80: waitForSigs must abort identifying the getThreshold failure; got: %v", err)
	}
}
