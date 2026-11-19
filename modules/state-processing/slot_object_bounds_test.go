package state_engine

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/require"
)

// outputObject is the output object MakeOutputs builds for one contract from
// results (blockProducer.go), with a realistic header.
func outputObject(results []ContractResult) map[string]interface{} {
	out := make([]contracts.ContractOutputResult, 0, len(results))
	inputs := make([]string, 0, len(results))
	for _, v := range results {
		ret := v.Ret
		if !utf8.ValidString(ret) {
			ret = base64.RawStdEncoding.EncodeToString([]byte(v.Ret))
		}
		out = append(out, contracts.ContractOutputResult{Ret: ret, Ok: v.Success, Err: v.Err, ErrMsg: v.ErrMsg, Logs: v.Logs, TssOps: v.TssOps})
		inputs = append(inputs, v.TxId)
	}
	return map[string]interface{}{
		"__t":          "vsc-output",
		"__v":          "0.1",
		"contract_id":  "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d",
		"metadata":     contracts.ContractMetadata{},
		"state_merkle": "bafyreicdzw3y5ku6rcscb4olyuzpldmjveamy72fppdrvono7jkqeipmgi",
		"inputs":       inputs,
		"results":      out,
	}
}

func encodedLen(t *testing.T, obj interface{}) int {
	t.Helper()
	b, err := common.EncodeDagCbor(obj)
	require.NoError(t, err)
	return len(b)
}

func boundTestResults() map[string]ContractResult {
	code := contracts.RUNTIME_ABORT
	invalid := strings.Repeat("\xff", 3000)
	txId := "bafyreigyby4arvnjssyvqap6pkgazs4ra3ki35ow7drlxdtxizmdwy64kq-0"
	logs := make([]string, 200)
	for i := range logs {
		logs[i] = fmt.Sprintf("swap|in=btc|out=hbd|ai=%d|to=contract:vsc1Brvi4YZHLkocYNAFd7Gf1JpsPjzNnv4i45", i)
	}
	return map[string]ContractResult{
		"empty":           {TxId: txId, Success: true},
		"ascii return":    {TxId: txId, Success: true, Ret: strings.Repeat("a", 50000)},
		"invalid return":  {TxId: txId, Success: true, Ret: invalid},
		"unicode return":  {TxId: txId, Success: true, Ret: strings.Repeat("é€😀", 5000)},
		"failed, message": {TxId: txId, Err: &code, ErrMsg: strings.Repeat("x", 10000)},
		"failed, invalid": {TxId: txId, Err: &code, ErrMsg: invalid},
		"many logs":       {TxId: txId, Success: true, Logs: logs},
		"invalid logs":    {TxId: txId, Success: true, Logs: []string{invalid, "\x00\x01\x02", invalid}},
		"one empty log":   {TxId: txId, Success: true, Logs: []string{"", "", "", ""}},
		"tss ops":         {TxId: txId, Success: true, TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d-main", Args: strings.Repeat("ab", 200), Epochs: 7}}},
		"html in strings": {TxId: txId, Success: true, Ret: strings.Repeat("<&>", 1000), Logs: []string{strings.Repeat("\"\\", 500)}},
	}
}

// The estimate is an upper bound on what a result really adds to its output
// object as MakeOutputs encodes it, for every kind of string a contract can
// produce: valid and invalid UTF-8, characters JSON escapes, many short logs.
func TestContractResultBytesIsAtLeastTheEncodedSize(t *testing.T) {
	base := encodedLen(t, outputObject(nil))
	require.Less(t, base, 1024, "the output object's own fields")
	for name, r := range boundTestResults() {
		one := encodedLen(t, outputObject([]ContractResult{r})) - base
		require.GreaterOrEqual(t, contractResultBytes(r), one, "%s: estimate %d, encoded %d", name, contractResultBytes(r), one)
		// And several together, as one output holds them.
		ten := make([]ContractResult, 10)
		for i := range ten {
			ten[i] = r
		}
		require.GreaterOrEqual(t, 10*contractResultBytes(r), encodedLen(t, outputObject(ten))-base, "%s x10", name)
	}
}

// The same for one ledger operation in the block's ledger record.
func TestOpLogEventBytesIsAtLeastTheEncodedSize(t *testing.T) {
	oplog := func(ops []ledgerSystem.OpLogEvent) map[string]interface{} {
		return map[string]interface{}{"__t": "vsc-oplog", "__v": "0.1", "ledger": ops, "outputs": []OplogOutputEntry{}}
	}
	base := encodedLen(t, oplog(nil))
	ops := map[string]ledgerSystem.OpLogEvent{
		"transfer":       {To: "hive:magi.test1", From: "contract:vsc1Brvi4YZHLkocYNAFd7Gf1JpsPjzNnv4i45", Amount: 1, Asset: "hbd", Type: "transfer", Id: "bafyreigyby4arvnjssyvqap6pkgazs4ra3ki35ow7drlxdtxizmdwy64kq-0-0"},
		"max amount":     {To: "did:pkh:eip155:1:0x3f5ce5fbfe3e9af3971dd833d26ba9b5c936f0be", From: "hive:x", Amount: 1<<63 - 1, Asset: "hbd_savings", Type: "withdraw", Id: "x"},
		"negative":       {To: "a", From: "b", Amount: -(1 << 62), Asset: "hive", Type: "t", Id: "i"},
		"memo":           {To: "a", From: "b", Amount: 5, Asset: "hive", Memo: strings.Repeat("m", 2048), Type: "deposit", Id: "i"},
		"invalid memo":   {To: "a", From: "b", Amount: 5, Asset: "hive", Memo: strings.Repeat("\xfe", 2048), Type: "deposit", Id: "i"},
		"with params":    {To: "a", From: "b", Amount: 5, Asset: "hive", Type: "withdraw", Id: "i", Params: map[string]interface{}{"to": "bc1qk90rvuqpmewgplaxt2x9s46pce6rh0vdjcpw20csf77x99rcqn9qnjdj97", "fee": 300, "nested": map[string]interface{}{"a": []int{1, 2, 3}}}},
		"escaped params": {To: "<>", From: "&", Amount: 5, Asset: "hive", Type: "withdraw", Id: "i", Params: map[string]interface{}{"k": strings.Repeat("<>&\xff", 100)}},
	}
	for name, op := range ops {
		one := encodedLen(t, oplog([]ledgerSystem.OpLogEvent{op})) - base
		require.GreaterOrEqual(t, ledgerSystem.OpLogEventBytes(op), one, "%s: estimate %d, encoded %d", name, ledgerSystem.OpLogEventBytes(op), one)
	}
}

func TestSlotObjectsOverBound(t *testing.T) {
	se := &StateEngine{contractOutputBytes: map[string]int{}, LedgerState: &ledgerSystem.LedgerState{}}
	small := []ContractIdResult{{ContractId: "a", Output: ContractResult{TxId: "t", Success: true, Ret: "ok"}}}
	require.Empty(t, se.slotObjectsOverBound(small, 0))

	big := func(id string, n int) ContractIdResult {
		return ContractIdResult{ContractId: id, Output: ContractResult{TxId: "t", Success: true, Ret: strings.Repeat("r", n)}}
	}
	// One call that alone is over the bound.
	require.Contains(t, se.slotObjectsOverBound([]ContractIdResult{big("a", params.MAX_CONTRACT_OUTPUT_BYTES)}, 0), "contract a")
	// Exactly at the bound fits.
	exact := params.MAX_CONTRACT_OUTPUT_BYTES - contractResultBytes(ContractResult{TxId: "t"})
	require.Empty(t, se.slotObjectsOverBound([]ContractIdResult{big("a", exact)}, 0))
	require.NotEmpty(t, se.slotObjectsOverBound([]ContractIdResult{big("a", exact+1)}, 0))
	// What earlier transactions used counts, per contract.
	se.contractOutputBytes["a"] = params.MAX_CONTRACT_OUTPUT_BYTES - 100
	require.Contains(t, se.slotObjectsOverBound(small, 0), "contract a")
	require.Empty(t, se.slotObjectsOverBound([]ContractIdResult{{ContractId: "b", Output: small[0].Output}}, 0), "another contract has its own output")
	// A nested contract's logs count to that contract.
	nested := []ContractIdResult{
		{ContractId: "b", Output: ContractResult{TxId: "t", Success: true}},
		{ContractId: "a", Output: ContractResult{TxId: "t", Success: true, Logs: []string{"x"}}},
	}
	require.Contains(t, se.slotObjectsOverBound(nested, 0), "contract a")
	// The ledger record.
	se.contractOutputBytes = map[string]int{}
	require.Empty(t, se.slotObjectsOverBound(nil, params.MAX_BLOCK_OPLOG_BYTES))
	require.Contains(t, se.slotObjectsOverBound(nil, params.MAX_BLOCK_OPLOG_BYTES+1), "ledger record")
	se.LedgerState.OplogBytes = params.MAX_BLOCK_OPLOG_BYTES - 10
	require.Contains(t, se.slotObjectsOverBound(nil, 11), "ledger record")
	require.Empty(t, se.slotObjectsOverBound(nil, 10))
}

// A failed call is recorded with as much of its message as fits, its revert
// symbol is cut, and once the output is full further failures are left out:
// however many there are, a contract's output stays inside the bound.
func TestAppendOutputBoundsFailedResults(t *testing.T) {
	code := contracts.RUNTIME_ABORT
	newSE := func() *StateEngine { return &StateEngine{ContractResults: map[string][]ContractResult{}} }

	// A huge message is cut to what fits, on a character boundary.
	se := newSE()
	huge := strings.Repeat("é", params.MAX_CONTRACT_OUTPUT_BYTES) // 2 bytes each
	se.appendOutput("a", ContractResult{TxId: "t", Err: &code, ErrMsg: huge}, true)
	require.Len(t, se.ContractResults["a"], 1)
	got := se.ContractResults["a"][0]
	require.True(t, utf8.ValidString(got.ErrMsg), "the cut split a character")
	require.LessOrEqual(t, se.contractOutputBytes["a"], params.MAX_CONTRACT_OUTPUT_BYTES)
	require.Greater(t, len(got.ErrMsg), params.MAX_CONTRACT_OUTPUT_BYTES-1000, "the message was cut more than it had to be")

	// A large contract-chosen revert symbol (sdk.Revert) is cut too.
	se = newSE()
	symbol := contracts.ContractOutputError(strings.Repeat("s", 4_500_000))
	se.appendOutput("a", ContractResult{TxId: "t", Err: &symbol, ErrMsg: "x"}, true)
	require.Len(t, se.ContractResults["a"], 1)
	require.Len(t, string(*se.ContractResults["a"][0].Err), params.MAX_ERROR_SYMBOL_BYTES)

	// Tens of thousands of failures: the output stops growing at the bound.
	se = newSE()
	for i := 0; i < 50_000; i++ {
		se.appendOutput("a", ContractResult{TxId: fmt.Sprintf("bafyreigyby4arvnjssyvqap6pkgazs4ra3ki35ow7drlxdtxizmdwy64kq-%d", i), Err: &code, ErrMsg: "boom"}, true)
	}
	require.LessOrEqual(t, se.contractOutputBytes["a"], params.MAX_CONTRACT_OUTPUT_BYTES)
	require.Less(t, len(se.ContractResults["a"]), 50_000, "every failure was kept")
	require.Greater(t, len(se.ContractResults["a"]), 1000)
	require.LessOrEqual(t, encodedLen(t, outputObject(se.ContractResults["a"])), params.MAX_CONTRACT_OUTPUT_BYTES+1024, "the encoded output is over the bound")

	// Unbounded (below 0.16.0) nothing is cut or left out, as before.
	se = newSE()
	se.appendOutput("a", ContractResult{TxId: "t", Err: &symbol, ErrMsg: huge}, false)
	require.Equal(t, symbol, *se.ContractResults["a"][0].Err)
	require.Equal(t, huge, se.ContractResults["a"][0].ErrMsg)

	// A message that fits is left alone.
	se = newSE()
	se.appendOutput("a", ContractResult{TxId: "t", Err: &code, ErrMsg: "boom"}, true)
	require.Equal(t, "boom", se.ContractResults["a"][0].ErrMsg)
}

// A failed transaction keeps only its failed results.
func TestFailedResultsOnly(t *testing.T) {
	code := contracts.LEDGER_ERROR
	outs := []ContractIdResult{
		{ContractId: "btc", Output: ContractResult{TxId: "t-0", Success: true, TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "btc-main", Args: "abcd"}}}},
		{ContractId: "router", Output: ContractResult{TxId: "t-1", Err: &code, ErrMsg: "no"}},
	}
	kept := failedResultsOnly(outs)
	require.Len(t, kept, 1)
	require.Equal(t, "router", kept[0].ContractId)
	require.Empty(t, failedResultsOnly([]ContractIdResult{{ContractId: "btc", Output: ContractResult{Success: true}}}))
}

// CallOutputs builds what ExecuteBatch built inline before: the called
// contract's result with the return value, one result per other contract, in
// contract id order.
func TestCallOutputs(t *testing.T) {
	logs := map[string]contract_session.LogOutput{
		"zeta":  {Logs: []string{"z"}},
		"alpha": {Logs: []string{"a"}, TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "k", Args: "m"}}},
		"main":  {Logs: []string{"m1", "m2"}},
	}
	outs := CallOutputs("tx-0", "main", "ret", true, logs)
	require.Len(t, outs, 3)
	require.Equal(t, []string{"alpha", "main", "zeta"}, []string{outs[0].ContractId, outs[1].ContractId, outs[2].ContractId})
	require.Equal(t, "ret", outs[1].Output.Ret)
	require.Equal(t, "", outs[0].Output.Ret)
	require.Equal(t, "", outs[2].Output.Ret)
	require.Len(t, outs[0].Output.TssOps, 1)
	for _, o := range outs {
		require.Equal(t, "tx-0", o.Output.TxId)
		require.True(t, o.Output.Success)
	}
}

// The dry run counts what the chain counts: the call's transaction id and TSS
// operations as well as its return value and logs.
func TestDryRunOverBoundCountsWhatTheChainCounts(t *testing.T) {
	logs := map[string]contract_session.LogOutput{
		"main": {TssOps: []tss_db.TssOp{{Type: "sign", KeyId: "k", Args: strings.Repeat("ab", 2000)}}},
	}
	// A return value that leaves less room than the TSS operations take.
	ret := strings.Repeat("r", params.MAX_CONTRACT_OUTPUT_BYTES-2000)
	outs := CallOutputs("bafyreigyby4arvnjssyvqap6pkgazs4ra3ki35ow7drlxdtxizmdwy64kq-0", "main", ret, true, logs)
	chain := (&StateEngine{contractOutputBytes: map[string]int{}, LedgerState: &ledgerSystem.LedgerState{}}).slotObjectsOverBound(outs, 0)
	require.NotEmpty(t, chain, "the chain would refuse this call")
	require.Equal(t, chain, DryRunOverBound(map[string]int{}, outs, 0), "the dry run must refuse it the same way")

	used := map[string]int{}
	small := CallOutputs("t-0", "main", "ok", true, map[string]contract_session.LogOutput{"main": {}})
	require.Empty(t, DryRunOverBound(used, small, 0))
	require.Equal(t, addedOutputBytes(small)["main"], used["main"], "a call that fits is added to what later calls see")
}

func TestCutUTF8(t *testing.T) {
	require.Equal(t, "", cutUTF8("abc", 0))
	require.Equal(t, "", cutUTF8("abc", -5))
	require.Equal(t, "ab", cutUTF8("abc", 2))
	require.Equal(t, "abc", cutUTF8("abc", 3))
	require.Equal(t, "abc", cutUTF8("abc", 30))
	require.Equal(t, "a", cutUTF8("a€", 2)) // € is 3 bytes
	require.Equal(t, "a", cutUTF8("a€", 3))
	require.Equal(t, "a€", cutUTF8("a€", 4))
	require.Equal(t, "", cutUTF8("😀", 3))
}

// The counter follows ContractResults: AppendOutput adds, Flush empties.
func TestContractOutputBytesFollowsTheResults(t *testing.T) {
	se := &StateEngine{ContractResults: map[string][]ContractResult{}}
	r := ContractResult{TxId: "t", Success: true, Ret: strings.Repeat("r", 1000)}
	se.AppendOutput("a", r)
	se.AppendOutput("a", r)
	se.AppendOutput("b", r)
	require.Equal(t, 2*contractResultBytes(r), se.contractOutputBytes["a"])
	require.Equal(t, contractResultBytes(r), se.contractOutputBytes["b"])
	se.Flush()
	require.Empty(t, se.contractOutputBytes)
	require.Empty(t, se.ContractResults)
}

// The chain always has an entry for the called contract (ExecuteTx opens its
// session first), so a call that only returns a value is counted; the dry run
// adds the same entry.
func TestWithCalledContract(t *testing.T) {
	none := map[string]contract_session.LogOutput{}
	got := WithCalledContract(none, "main")
	require.Contains(t, got, "main")
	require.Empty(t, none, "the caller's map was changed")
	outs := CallOutputs("t-0", "main", strings.Repeat("r", 100), true, got)
	require.Len(t, outs, 1)
	require.Equal(t, 100, len(outs[0].Output.Ret))

	has := map[string]contract_session.LogOutput{"main": {Logs: []string{"x"}}}
	require.Equal(t, []string{"x"}, WithCalledContract(has, "main")["main"].Logs)
}
