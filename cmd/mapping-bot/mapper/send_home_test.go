package mapper

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"testing"

	"vsc-node/cmd/mapping-bot/chain"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A deposit swap refunded to a Bitcoin address lands on that address's own Magi
// account, which has no RC: the bot calls sendHome (permissionless, pays the RC)
// so the sats go back to the address on Bitcoin.

const shRefundAddr = "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7"

// actionCaller answers every call with a tx id named after its action, so a map
// and a sendHome can resolve to different on-chain statuses.
type actionCaller struct {
	mu    sync.Mutex
	calls []contractCall
	errs  map[string]error
}

func (c *actionCaller) CallContract(ctx context.Context, in json.RawMessage, action string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, contractCall{Action: action, Payload: in})
	return "tx-" + action, c.errs[action]
}

func (c *actionCaller) actions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	for i, call := range c.calls {
		out[i] = call.Action
	}
	return out
}

func swapInstrWithRefund(refundTo string) string {
	return url.Values{
		"swap_to":        {"hive:bob"},
		"swap_asset_out": {"hbd"},
		"refund_to":      {refundTo},
	}.Encode()
}

func newSendHomeBot(t *testing.T, instruction string) (*Bot, *mockGraphQL, *actionCaller, *mockFailedTxStore, []byte) {
	t.Helper()
	bot, gql, _, state, addr, _ := newTestBotWithMocks()
	caller := &actionCaller{errs: map[string]error{}}
	bot.Caller = caller
	failed := newMockFailedTxStore()
	bot.FailedTxDB = failed
	gql.lastHeight = "200"
	gql.refundable = map[string]int64{}
	gql.txStatuses = map[string]string{"tx-map": "CONFIRMED", "tx-sendHome": "CONFIRMED"}
	depositAddr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"
	addr.instructions[depositAddr] = instruction
	require.NoError(t, state.SetBlockHeight(context.Background(), 100))
	return bot, gql, caller, failed, buildTestBlock(t, depositAddr, &chaincfg.TestNet4Params)
}

func TestSendHome_RefundToABitcoinAddressGoesHome(t *testing.T) {
	bot, gql, caller, _, block := newSendHomeBot(t, swapInstrWithRefund(shRefundAddr))
	gql.refundable[shRefundAddr] = 10_000

	require.True(t, bot.HandleMap(block, 100))
	require.Equal(t, []string{"map", "sendHome"}, caller.actions())
	payload := caller.calls[1].Payload
	assert.True(t, json.Valid(payload), "a failed call's payload is served as JSON on /health")
	assert.JSONEq(t, `"`+shRefundAddr+`"`, string(payload))
	assert.Empty(t, bot.refundWatch, "a refund sent home leaves the watch")
}

// The cases below drive the watch directly: each broadcast waits out the status
// poll (initialStatusDelay), so only the cases that must call pay for one.

func TestSendHome_TheRefundAddressDIDFormIsWatchedByItsAddress(t *testing.T) {
	bot, _, _, _, _ := newSendHomeBot(t, "")
	bot.watchRefunds([]string{swapInstrWithRefund("did:pkh:bip122:00000000da84f2bafbbc53dee25a72ae:" + shRefundAddr)})
	assert.Equal(t, map[string]int{shRefundAddr: refundWatchPasses}, bot.refundWatch)
}

func TestSendHome_NothingRecordedNoCall_AndALaggingReadIsCaughtNextPass(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})

	// The swap completed (or the node has not caught up): no record, no call.
	bot.sendHomeRefunds(context.Background())
	assert.Empty(t, caller.actions())
	assert.Equal(t, refundWatchPasses-1, bot.refundWatch[shRefundAddr])

	// The record shows up on the next pass: it goes home then.
	gql.refundable[shRefundAddr] = 10_000
	bot.sendHomeRefunds(context.Background())
	assert.Equal(t, []string{"sendHome"}, caller.actions())
	assert.Empty(t, bot.refundWatch)
}

func TestSendHome_AnAddressIsDroppedAfterItsPasses(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})
	for i := 0; i < refundWatchPasses; i++ {
		bot.sendHomeRefunds(context.Background())
	}
	assert.Empty(t, bot.refundWatch)
	gql.refundable[shRefundAddr] = 10_000
	bot.sendHomeRefunds(context.Background())
	assert.Empty(t, caller.actions(), "no calls once dropped")
}

func TestSendHome_HiveRefundsAndPlainDepositsAreNotWatched(t *testing.T) {
	bot, _, _, _, _ := newSendHomeBot(t, "")
	bot.watchRefunds([]string{
		swapInstrWithRefund("hive:alice"),
		"deposit_to=hive:testuser",
		swapInstrWithRefund("not-an-address"),
		swapInstrWithRefund("bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"), // mainnet address on testnet4
		"%zz=bad",
	})
	assert.Empty(t, bot.refundWatch)
}

func TestSendHome_ASubDustRecordIsLeftAlone(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	gql.refundable[shRefundAddr] = sendHomeDustSats
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})
	bot.sendHomeRefunds(context.Background())
	assert.Empty(t, caller.actions())
}

func TestSendHome_AnUnreadableRecordIsCheckedAgain(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	gql.readErr = map[string]error{"FetchRefundable": errors.New("node down")}
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})
	bot.sendHomeRefunds(context.Background())
	assert.Empty(t, caller.actions())
	assert.Contains(t, bot.refundWatch, shRefundAddr)
}

func TestSendHome_AnOnChainRefusalIsRecordedForRetryAndDropped(t *testing.T) {
	bot, gql, caller, failed, _ := newSendHomeBot(t, "")
	gql.refundable[shRefundAddr] = 10_000
	gql.txStatuses["tx-sendHome"] = "FAILED"
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})

	bot.sendHomeRefunds(context.Background())
	assert.Equal(t, []string{"sendHome"}, caller.actions())
	all, _ := failed.GetAll(context.Background())
	require.Len(t, all, 1)
	assert.Equal(t, "sendHome", all[0].Action)
	_, err := json.Marshal(all[0])
	assert.NoError(t, err, "the /health endpoint serves the record as JSON")
	assert.Empty(t, bot.refundWatch, "an operator retries it from /retry")
}

func TestSendHome_ATransientFailureIsTriedAgain(t *testing.T) {
	bot, gql, caller, failed, _ := newSendHomeBot(t, "")
	gql.refundable[shRefundAddr] = 10_000
	caller.errs["sendHome"] = errors.New("node unreachable")
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})

	bot.sendHomeRefunds(context.Background())
	assert.Contains(t, bot.refundWatch, shRefundAddr, "kept for the next pass")
	all, _ := failed.GetAll(context.Background())
	assert.Empty(t, all, "nothing landed, nothing to retry")

	delete(caller.errs, "sendHome")
	bot.sendHomeRefunds(context.Background())
	acts := caller.actions()
	assert.Equal(t, "sendHome", acts[len(acts)-1])
	assert.Empty(t, bot.refundWatch)
}

func TestSendHome_OnlyTheBTCBotTakesAddressRefunds(t *testing.T) {
	btc := &chain.ChainConfig{Name: "btc", ChainParams: &chaincfg.TestNet4Params}
	assert.Equal(t, &chaincfg.TestNet4Params, AddressRefundParams(btc))
	for _, name := range []string{"dash", "ltc", "doge", "bch"} {
		c := &chain.ChainConfig{Name: name, ChainParams: &chaincfg.TestNet4Params}
		assert.Nil(t, AddressRefundParams(c), name)
		err := ValidateInstruction(swapInstrWithRefund(shRefundAddr), false, AddressRefundParams(c))
		assert.Error(t, err, "%s: an address refund_to is refused, its contract cannot send it home", name)
		assert.NoError(t, ValidateInstruction(swapInstrWithRefund("hive:alice"), false, AddressRefundParams(c)), name)
	}
	assert.Nil(t, AddressRefundParams(nil))

	bot, _, _, _, _ := newSendHomeBot(t, "")
	bot.Chain.Name = "dash"
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})
	assert.Empty(t, bot.refundWatch, "a DASH bot has no sendHome to call")
}

func TestSendHome_ALowBotRCWaitsAndKeepsTheRefund(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	gql.refundable[shRefundAddr] = 10_000
	gql.accountRC = map[string]int64{bot.botEthDID.String(): sendHomeRcReserve - 1}
	bot.watchRefunds([]string{swapInstrWithRefund(shRefundAddr)})

	for i := 0; i < refundWatchPasses+2; i++ {
		bot.sendHomeRefunds(context.Background())
	}
	assert.Empty(t, caller.actions(), "the keeper never spends the RC the maps need")
	assert.Contains(t, bot.refundWatch, shRefundAddr, "a waiting refund is never dropped")

	gql.accountRC[bot.botEthDID.String()] = sendHomeRcReserve
	bot.sendHomeRefunds(context.Background())
	assert.Equal(t, []string{"sendHome"}, caller.actions())
	assert.Empty(t, bot.refundWatch)
}

func TestSendHome_APassSendsAFewAndTheRestNextPass(t *testing.T) {
	bot, gql, caller, _, _ := newSendHomeBot(t, "")
	addrs := []string{
		shRefundAddr,
		"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"tb1qqqqqp399et2xygdj5xreqhjjvcmzhxw4aywxecjdzew6hylgvsesrxh6hy",
		shGeneratedAddr(t),
	}
	var instrs []string
	for _, a := range addrs {
		gql.refundable[a] = 10_000
		instrs = append(instrs, swapInstrWithRefund(a))
	}
	bot.watchRefunds(instrs)
	require.Len(t, bot.refundWatch, len(addrs))

	bot.sendHomeRefunds(context.Background())
	assert.Len(t, caller.actions(), sendHomePerPass)
	assert.Len(t, bot.refundWatch, len(addrs)-sendHomePerPass, "the rest wait, without using up passes")

	bot.sendHomeRefunds(context.Background())
	assert.Len(t, caller.actions(), len(addrs))
	assert.Empty(t, bot.refundWatch)
}

func shGeneratedAddr(t *testing.T) string {
	t.Helper()
	a, err := btcutil.NewAddressWitnessPubKeyHash(make([]byte, 20), &chaincfg.TestNet4Params)
	require.NoError(t, err)
	return a.EncodeAddress()
}
