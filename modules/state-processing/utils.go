package state_engine

import (
	"bytes"
	"context"
	"crypto"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"vsc-node/modules/common/params"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/hive/streamer"

	vscCommon "vsc-node/modules/common"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	kadDht "github.com/libp2p/go-libp2p-kad-dht"
	libCrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	rhost "github.com/libp2p/go-libp2p/p2p/host/routed"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
	"github.com/vsc-eco/hivego"
)

// Key DIDs are always supposed to be normalized
// Note: it might make sense to define address format for regular DIDs.
// However, what we have right now suffices
var SUPPORTED_TYPES = []string{
	"ethereum",
	"hive",
}

func NormalizeAddress(address string, addressType string) (*string, error) {

	if !slices.Contains(SUPPORTED_TYPES, addressType) {
		return nil, errors.New("unsupported address type")
	}

	switch addressType {

	case "ethereum":
		{
			if !strings.HasPrefix(address, "0x") || len(address) != 42 {
				return nil, errors.New("invalid ethereum address")
			}
			hexBytes := common.FromHex(address)
			addr := common.Address{}
			addr.SetBytes(hexBytes)
			eip55Addr := common.AddressEIP55(addr)

			// normalizedAddr := common.
			returnVal := "did:pkh:eip155:1:" + eip55Addr.String()

			return &returnVal, nil
		}
	case "hive":
		{
			if !strings.HasPrefix(address, "hive:") {
				returnVal := address
				return &returnVal, nil
			}
			returnVal := "hive:" + address
			return &returnVal, nil
		}
	}

	return nil, errors.New("unsupported address type")
}

// Checks if the system transaction contains a fee payment transfer to gateway wallet at the second operation
//
// TODO: We should find a way to execute contract deployments in ExecuteBatch() where ledgerSession is accessible
// before regular ops so that fees can be paid from Magi balance alternatively
func hasFeePaymentOp(ops []hivego.Operation, feeAmt int64, feeAsset string) (bool, int64, string) {
	if len(ops) < 2 {
		return false, 0, ""
	}

	secondOp := ops[1]
	if secondOp.Type == "transfer" {
		amountData := secondOp.Value["amount"].(map[string]any)
		amount, err := strconv.ParseInt(amountData["amount"].(string), 10, 64)

		if err != nil {
			return false, 0, ""
		}

		feeNai := "@@000000013"
		if feeAsset != "hbd" {
			feeNai = "@@000000021"
		}

		if amount < feeAmt || amountData["nai"] != feeNai || secondOp.Value["to"] != params.GATEWAY_WALLET {
			return false, 0, ""
		}
		return true, amount, secondOp.Value["from"].(string)
	} else {
		return false, 0, ""
	}
}

var BOOTSTRAP = []string{
	"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWAvxZcLJmZVUaoAtey28REvaBwxvfTvQfxWtXJ2fpqWnw",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmbLHAnMoJPWSCR5Zhtx6BHJX9KiKNN6tpvbUcqanj75Nb",
	"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
	"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",         // mars.i.ipfs.io
	"/ip4/104.131.131.82/udp/4001/quic-v1/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ", // mars.i.ipfs.io
	"/ip4/85.215.198.234/tcp/4001/p2p/12D3KooWJf8f9anWjHLc5CPcVdpmzGMAKhGXCjKsjqCQ5o8r1e1d",
	"/ip4/95.211.231.201/tcp/4001/p2p/12D3KooWJAsC5ZtFNGpLZCQVzwfmssodvxJ4g1SuvtqEvk7KFjfZ",
	// @vsc.node2
	"/ip4/149.56.25.168/tcp/4001/p2p/12D3KooWECQvjztesJsSwcYodLBeALGhwpczbYRjgLMyD3DAo6o8",
}

type setupResult struct {
	Host host.Host
	Dht  *kadDht.IpfsDHT
}

func SetupEnv() setupResult {
	pkbytes := []byte("PRIVATE_KEY_TEST_ONLY")
	pk, _, _ := libCrypto.GenerateSecp256k1Key(bytes.NewReader(pkbytes))
	host, _ := libp2p.New(libp2p.Identity(pk))
	ctx := context.Background()

	dht, _ := kadDht.New(ctx, host)
	routedHost := rhost.Wrap(host, dht)
	for _, peerStr := range BOOTSTRAP {
		peerId, _ := peer.AddrInfoFromString(peerStr)

		routedHost.Connect(ctx, *peerId)
	}
	dht.Bootstrap(ctx)

	return setupResult{
		Host: routedHost,
		Dht:  dht,
	}
}

// Bad block txs that were out of slot (invalid witness per slot)

type AuthCheckType struct {
	Level    string
	Required []string
}

func AuthCheck(customJson CustomJson, args AuthCheckType) bool {
	// if args.Level != nil {

	// }
	//Write code for auth check
	return false
}

// Mock block reader which aims to recreate the behavior of the real reader
type MockReader struct {
	//Mock mempool for testing
	Mempool  []hive_blocks.Tx
	VMempool []hivego.VirtualOp

	ProcessFunction streamer.ProcessFunction
	LastBlock       uint64

	lastTs time.Time
	mutex  *sync.Mutex
}

// Mines empty blocks to simulate empty activity
func (mr *MockReader) MineNullBlocks(count int) {
	// Mine null blocks

	for i := 0; i < count; i++ {
		mr.witnessBlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (mr *MockReader) CreateBlockWithOps(txs []hive_blocks.Tx, ops []hivego.VirtualOp) {
	for _, tx := range txs {
		mr.IngestTx(tx)
	}
	for _, vop := range ops {
		mr.VMempool = append(mr.VMempool, vop)
	}

	mr.witnessBlock()
}

func (mr *MockReader) CreateBlock() {
	mr.witnessBlock()
}

func (mr *MockReader) BroadcastTx() {

}

// Run the mock in real time, producing blocks every 3s.
func (mr *MockReader) StartRealtime() {
	ticker := time.NewTicker(3 * time.Second)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				// do stuff
				mr.witnessBlock()
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
}

func (mr *MockReader) witnessBlock() {
	mr.mutex.Lock()
	ts := mr.lastTs.Add(3 * time.Second)

	bn := mr.LastBlock + 1
	headerbytes := make([]byte, 4)
	binary.BigEndian.PutUint32(headerbytes[0:4], uint32(bn))
	memBytes, _ := json.Marshal(mr.Mempool)

	h := crypto.SHA256.New()
	h.Write(headerbytes)
	h.Write(memBytes)
	hashId := h.Sum(nil)

	blockId := make([]byte, 20)
	copy(blockId[0:4], headerbytes[0:4])
	copy(blockId[4:20], hashId[0:16])

	hb := hive_blocks.HiveBlock{
		BlockNumber:  bn,
		BlockID:      hex.EncodeToString(blockId[0:20]),
		MerkleRoot:   "fake-merkle-root",
		Transactions: mr.Mempool,
		Timestamp:    ts.Format("2006-01-02T15:04:05Z"),
		VirtualOps:   mr.VMempool,
	}

	if mr.ProcessFunction != nil {
		go mr.ProcessFunction(hb, &bn)
	}

	mr.lastTs = ts
	mr.LastBlock = mr.LastBlock + 1
	mr.Mempool = make([]hive_blocks.Tx, 0)
	mr.VMempool = make([]hivego.VirtualOp, 0)
	mr.mutex.Unlock()
}

func (mr *MockReader) IngestTx(tx hive_blocks.Tx) {
	mr.mutex.Lock()
	tx.Index = len(mr.Mempool) + 1
	mr.Mempool = append(mr.Mempool, tx)
	mr.mutex.Unlock()
}

func NewMockReader() *MockReader {
	return &MockReader{
		mutex: &sync.Mutex{},
	}
}

func NewMockCreator(mr *MockReader) *MockCreator {
	return &MockCreator{
		Mr: mr,
	}
}

// Mock Transaction creator
type MockCreator struct {
	Mr *MockReader
}

type MockJson struct {
	RequiredAuths        []string
	RequiredPostingAuths []string
	Id                   string
	Json                 string
}

func (mc *MockCreator) CustomJson(mj MockJson) TxConfirmation {
	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "custom_json",
				Value: map[string]interface{}{
					"id":                     mj.Id,
					"json":                   mj.Json,
					"required_auths":         mj.RequiredAuths,
					"required_posting_auths": mj.RequiredPostingAuths,
				},
			},
		},
	}

	return mc.ingestTx(tx)
}

func (mc *MockCreator) Transfer(from string, to string, amount string, asset string, memo string) TxConfirmation {
	var nai string
	if asset == "HBD" {
		nai = "@@000000013"
	} else {
		nai = "@@000000021"
	}
	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "transfer",
				Value: map[string]interface{}{
					"from": from,
					"to":   to,
					"amount": map[string]interface{}{
						"amount":    amount,
						"nai":       nai,
						"precision": 3,
					},
					"memo": memo,
				},
			},
		},
	}

	return mc.ingestTx(tx)
}

func (mc *MockCreator) AccountUpdate(account string, json string) TxConfirmation {
	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "account_update",
				Value: map[string]interface{}{
					"account": account,
					"json":    json,
				},
			},
		},
	}

	return mc.ingestTx(tx)
}

func (mc *MockCreator) ClaimInterest(account string, amount int) TxConfirmation {

	amtStr := strconv.Itoa(amount)

	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "transfer_to_savings",
				Value: map[string]interface{}{

					"interest": map[string]interface{}{
						"amount":    amtStr,
						"nai":       "@@000000013",
						"precision": 3,
					},
					"from": account,
					"memo": "",
					"to":   account,
				},
			},
		},
	}

	//Note not every field is filled out as it is not necessary for the test
	mc.ingestVp(hivego.VirtualOp{
		Op: struct {
			Type  string                 "json:\"type\""
			Value map[string]interface{} "json:\"value\""
		}{
			Type: "interest_operation",
			Value: map[string]interface{}{
				"interest": map[string]interface{}{
					"nai":       "@@000000013",
					"precision": 3,
					"amount":    amtStr,
				},
				"owner": account,
			},
		},
	})

	return mc.ingestTx(tx)
}

func (mc *MockCreator) BroadcastOps(ops []hivego.Operation, txId string) TxConfirmation {
	tx := hive_blocks.Tx{
		Operations: ops,
	}

	return mc.ingestTx(tx, txId)
}

func (mc *MockCreator) BroadcastTx() {
	//Figure this out
}

func (mc *MockCreator) ingestTx(tx hive_blocks.Tx, txId ...string) TxConfirmation {

	bbytes, _ := json.Marshal(tx)

	if len(txId) > 0 {
		tx.TransactionID = txId[0]
	} else {
		txId := mc.hashTx(bbytes)
		tx.TransactionID = txId
	}

	mc.Mr.IngestTx(tx)

	return TxConfirmation{
		Id: tx.TransactionID,
	}
}

func (mc *MockCreator) ingestVp(v hivego.VirtualOp) {
	// mc.Mr.mutex.Lock()
	mc.Mr.VMempool = append(mc.Mr.VMempool, v)
	// mc.Mr.mutex.Unlock()
}

func (mc *MockCreator) hashTx(bbytes []byte) string {
	h := crypto.SHA256.New()
	h.Write(bbytes)
	h.Write([]byte(mc.Mr.lastTs.String()))
	hash := h.Sum(nil)

	txId := hex.EncodeToString(hash[0:20])

	return txId
}

type TxConfirmation struct {
	Id string
}

type SlotStatus struct {
	Done           bool
	SlotHeight     uint64
	Producer       string
	BalanceUpdated bool
}

func MakeTxId(TxId string, opIdx int) string {
	if opIdx == 0 {
		return TxId
	} else {
		return TxId + "-" + strconv.Itoa(opIdx)
	}
}

// Note: this is functionality different than original implementation
// It doesn't matter as this is just for DB serialization
// null vs bool value
func HashAuths(auths []string) *cid.Cid {
	obj := make(map[string]bool)

	for _, v := range auths {
		obj[v] = true
	}

	dag, _ := vscCommon.EncodeDagCbor(obj)

	cid, _ := cid.Prefix{
		Version:  1,
		Codec:    uint64(multicodec.DagCbor),
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}.Sum(dag)

	return &cid
}

func copyJsonToType(src map[string]interface{}, dest interface{}) error {
	bytes, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, dest)
}

type Buffer struct {
	c chan byte
	// *io.PipeReader
	// *io.PipeWriter
}

func NewBuffer() *Buffer {
	return &Buffer{c: make(chan byte, 8*1024*1024)} // TODO by making the buffer size very large, the test will no longer be flaky. Does this indicate a likely problem in production?
	// r, w := io.Pipe()
	// return &Buffer{r, w}
}

func (b *Buffer) Read(p []byte) (n int, err error) {
	for i := 0; i < len(p); i++ {
		if len(b.c) == 0 {
			return i, nil
		}
		p[i] = <-b.c
	}
	return len(p), nil
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	for i, v := range p {
		if len(b.c) == cap(b.c) {
			return i, nil
		}
		b.c <- v
	}
	return len(p), nil
}
