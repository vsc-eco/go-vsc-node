//go:build evm_devnet

// U1: the EVM bridge upgraded over live three-asset state, the way testnet's
// step 3 will do it (TESTNET-PLAN-1110: upgrade vsc1Bo8h... from the deployed
// build to the merged one, then restart the bot on the develop build). The
// devnet starts on the build testnet runs today and the bot that went with it,
// with BTC, ETH and USDC live; mid-rotation (the next EVM key's keygen
// requested) and with 8 withdrawals in flight plus one queued, the bridge code
// is updated through the timelocked vsc.update_contract. The bridge's books
// must not move across the upgrade, the old bot must keep working against the
// new code until it is replaced, and the rotation then finishes on the new code
// for all three assets (runCycle's checks, including the queued-debit recount
// the new code runs before activateKey).
//
// Inputs (besides the BOTH ones): U1_OLD_BRIDGE_WASM (the deployed testnet
// build, sha256 a1652470...: /home/clauderfly/evm-fix/devnet-art/B9/evm-bridge-devnet.wasm)
// and U1_OLD_BOT_BIN (the bot of that artifact set). The new build is
// EVM_BRIDGE_DEVNET_WASM / EVM_BOT_BIN as for every other test.
//
// Run: go test -tags evm_devnet -run TestBothBridges_U1 ./tests/devnet/ -timeout 330m -v
package devnet

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// rawCID is the CIDv1 (raw codec, sha2-256) of b: the code id Magi stores.
func rawCID(b []byte) string {
	d := sha256.Sum256(b)
	buf := append([]byte{0x01, 0x55, 0x12, 0x20}, d[:]...)
	return "b" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
}

// u1Code is the bridge's active code id as the GQL node reports it.
func u1Code(b *bothEnv) string {
	var out struct {
		FindContract []struct {
			Code string `json:"code"`
		} `json:"findContract"`
	}
	const q = `query($id:String!){findContract(filterOptions:{byId:$id}){code}}`
	if err := b.d.gqlQuery(b.ctx, b.e.gqlNode, q, map[string]any{"id": b.e.amID}, &out); err != nil || len(out.FindContract) == 0 {
		return ""
	}
	return out.FindContract[0].Code
}

// u1Books is the bridge state that a code update must leave untouched.
func u1Books(b *bothEnv) map[string]string {
	keys := []string{"s-eth", "s-usdc", "gr", "a-" + b.user + "-eth", "a-" + b.user + "-usdc", "v", "msr", "n", "np",
		"wqh", "wqt", "wqd-eth", "wqd-usdc", "vb-0-eth", "vb-0-usdc", "si", "th"}
	for i := 0; i < 12; i++ {
		keys = append(keys, "d-"+strconv.Itoa(i), "wq-"+strconv.Itoa(i))
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k] = b.e.stateStr(k)
	}
	return out
}

func u1Int(s string) int { n, _ := strconv.Atoi(s); return n }

func TestBothBridges_U1_UpgradeMidRotation(t *testing.T) {
	bbSkipShort(t)
	oldWasm, oldBot := os.Getenv("U1_OLD_BRIDGE_WASM"), os.Getenv("U1_OLD_BOT_BIN")
	if oldWasm == "" || oldBot == "" {
		t.Skip("U1_OLD_BRIDGE_WASM and U1_OLD_BOT_BIN (the deployed testnet bridge and its bot) required")
	}
	newWasm, newBot := evmArtifact(t, "EVM_BRIDGE_DEVNET_WASM"), evmArtifact(t, "EVM_BOT_BIN")
	newCode, err := os.ReadFile(newWasm)
	if err != nil {
		t.Fatal(err)
	}
	oldCode, err := os.ReadFile(oldWasm)
	if err != nil {
		t.Fatal(err)
	}
	newCID, oldCID := rawCID(newCode), rawCID(oldCode)
	if newCID == oldCID {
		t.Fatal("old and new bridge builds are the same file")
	}
	// The devnet comes up on the old build and the old bot.
	t.Setenv("EVM_BRIDGE_DEVNET_WASM", oldWasm)
	t.Setenv("EVM_BOT_BIN", oldBot)
	b := newBothEnv(t, 330*time.Minute, bbWeatherLevel("none"), true)
	if got := u1Code(b); got != oldCID {
		t.Fatalf("bridge deployed with code %q, want the old build %s", got, oldCID)
	}
	t.Logf("U1: bridge %s on the old build %s with BTC, ETH and USDC live", b.e.amID, oldCID)

	b.runCycle(cycleHooks{afterCreateKeys: func(start time.Time) {
		// Hold the prover: withdrawals mine on L1 but cannot settle, so they
		// are in flight across the upgrade.
		b.mu.Lock()
		b.proverHold = devnetNow().Add(6 * time.Hour)
		b.mu.Unlock()
		var recips []ethcommon.Address
		amt := new(big.Int).Div(bbEth(1), big.NewInt(20)) // 0.05 ETH
		for i := 0; i < 9; i++ {
			recips = append(recips, b.evmWithdraw(amt))
		}
		if err := pollUntil(b.ctx, 20*time.Minute, func() bool {
			return u1Int(b.e.stateStr("np"))-u1Int(b.e.stateStr("n")) == 8 && u1Int(b.e.stateStr("wqt")) > u1Int(b.e.stateStr("wqh"))
		}); err != nil {
			t.Fatalf("want 8 withdrawals in flight and 1 queued before the upgrade: n=%q np=%q wqh=%q wqt=%q",
				b.e.stateStr("n"), b.e.stateStr("np"), b.e.stateStr("wqh"), b.e.stateStr("wqt"))
		}
		before := u1Books(b)
		b.note("U1 before the upgrade: n=%s np=%s wqh=%s wqt=%s s-eth=%s s-usdc=%s gr=%s", before["n"], before["np"], before["wqh"], before["wqt"], before["s-eth"], before["s-usdc"], before["gr"])

		up := devnetNow()
		if err := b.d.UpdateContract(b.ctx, ContractUpdateOpts{ContractId: b.e.amID, WasmPath: newWasm, Name: "evm-bridge", DeployerNode: b.e.ownerNode, GQLNode: b.e.gqlNode}); err != nil {
			t.Fatalf("contract update: %v", err)
		}
		if err := pollUntil(b.ctx, 30*time.Minute, func() bool { return u1Code(b) == newCID }); err != nil {
			t.Fatalf("the update never activated: code %q, want %s", u1Code(b), newCID)
		}
		b.mark("U1 contract update queued -> active", up)
		after := u1Books(b)
		var moved []string
		for k, v := range before {
			if after[k] != v {
				moved = append(moved, fmt.Sprintf("%s: %q -> %q", k, v, after[k]))
			}
		}
		if len(moved) > 0 {
			t.Fatalf("the code update moved the bridge's books:\n%s", strings.Join(moved, "\n"))
		}
		t.Logf("CASE U1-BOOKS PASS: %d bridge keys identical across the upgrade (8 in flight, 1 queued, keygen pending)", len(before))

		// The window between testnet steps 3 and 4: the old bot runs on the new
		// code for a few minutes; it must not crash on it.
		devnetSleep(3 * time.Minute)
		if l := b.botLogs(); strings.Contains(l, "panic:") || strings.Contains(l, "fatal error:") {
			t.Fatalf("the old bot crashed on the new code:\n%s", l)
		}
		t.Setenv("EVM_BOT_BIN", newBot)
		b.restartBot()
		b.note("U1 bot restarted on the new build after the upgrade")
		b.mu.Lock()
		b.proverHold = time.Time{}
		b.mu.Unlock()
		if err := b.botPoll(45*time.Minute, func() bool {
			for _, r := range recips {
				if b.l1Balance(r).Cmp(amt) != 0 {
					return false
				}
			}
			return b.e.stateStr("wqt") == b.e.stateStr("wqh")
		}); err != nil {
			t.Fatalf("the withdrawals in flight and queued across the upgrade never all paid (n=%q np=%q wqh=%q wqt=%q)\nbot:\n%s",
				b.e.stateStr("n"), b.e.stateStr("np"), b.e.stateStr("wqh"), b.e.stateStr("wqt"), b.botLogs())
		}
		b.mark("U1 9 withdrawals across the upgrade -> all paid", up)
	}})
	t.Logf("CASE U1 PASS: upgraded mid-rotation over live BTC/ETH/USDC state; books unchanged; old bot then new bot; rotation finished on the new code")
}
