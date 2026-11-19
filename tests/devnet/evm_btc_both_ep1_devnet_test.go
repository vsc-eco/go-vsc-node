//go:build evm_devnet

// EP1 (ETH-PAYLOAD-1, GAP-5; contract #19 + bot #298, unit/real-wasm only
// until now): an ETH deposit whose tx is too big for the old map payload (the
// tx twice in hex) must still be mapped. A 2,000-byte calldata deposit (its
// deposit_to instruction plus padding the contract ignores) puts the old form
// over the node's 8,192-byte call cap; the bot must send the tx once as a
// base64 tx-trie proof (tx_proof) and the bridge must credit the deposit_to
// account. Runs on the BOTH devnet (BTC and USDC live next to it).
//
// Run: go test -tags evm_devnet -run TestBothBridges_EP1 ./tests/devnet/ -timeout 200m -v
package devnet

import (
	"math/big"
	"strings"
	"testing"
	"time"

	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

func TestBothBridges_EP1_LargeETHDeposit(t *testing.T) {
	bbSkipShort(t)
	b := newBothEnv(t, 180*time.Minute, bbWeatherLevel("none"), true)
	const dest = "hive:magi.test2"
	key := mustKey(t)
	from := cryptoPubkeyAddr(key)
	if err := b.anvil.SetBalance(b.ctx, from, bbEth(2)); err != nil {
		t.Fatal(err)
	}
	data := []byte("deposit_to=" + dest + "&pad=" + strings.Repeat("x", 2000-len("deposit_to="+dest+"&pad=")))
	nonce, err := b.anvil.cli.PendingNonceAt(b.ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	to := b.vault0
	// Gas: the EIP-7623 calldata floor (Prague, anvil's default) is 21,000 + 10 gas per token, 4 tokens per
	// non-zero byte. 16 gas per byte (EP1 11-11 16:28: Gas 63,000 against a ~101,000 floor) is accepted by
	// anvil's pool but never mined.
	tx, err := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID: b.anvil.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(50e9),
		Gas: 21_000 + 40*uint64(len(data)) + 10_000, To: &to, Value: bbEth(1), Data: data,
	}), ethtypes.LatestSignerForChainID(b.anvil.ChainID), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.MarshalBinary()
	t.Logf("EP1: deposit tx %s is %d bytes (old map payload carried it twice in hex: ~%d bytes)", tx.Hash().Hex(), len(raw), 4*len(raw))
	if err := b.anvil.cli.SendTransaction(b.ctx, tx); err != nil {
		t.Fatalf("send the large deposit: %v", err)
	}
	if rc, err := b.anvil.waitReceipt(b.ctx, tx.Hash(), 30*time.Second); err != nil || rc.Status != 1 {
		t.Fatalf("the large deposit did not mine: %v", err)
	}
	// 1 ETH less the 1% reserve share.
	want := new(big.Int).Sub(bbEth(1), new(big.Int).Div(bbEth(1), big.NewInt(100))).String()
	if err := b.botPoll(30*time.Minute, func() bool { return strings.SplitN(b.e.stateStr("a-"+dest+"-eth"), "|", 2)[0] == want }); err != nil {
		t.Fatalf("the large ETH deposit was never credited to %s (a-%s-eth %q, want %s)\nbot:\n%s", dest, dest, b.e.stateStr("a-"+dest+"-eth"), want, b.botLogs())
	}
	t.Logf("CASE EP1 PASS: a %d-byte ETH deposit tx (over the old payload cap) credited %s wei to %s through tx_proof", len(raw), want, dest)
}
